package flow

import (
	"log"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow/decode"
)

// writerCap bounds the batches waiting for SQLite; a full channel drops the
// batch (writer_dropped) — ingest never blocks on the database.
const writerCap = 8

// Rollup look-back (§8.9).
const (
	rollupMaxLookback  = 3 * time.Hour
	startupRollupHours = 3
)

// flushBatch is one flushed minute of every exporter.
type flushBatch struct {
	minute      time.Time
	final       bool // shutdown flush: no FlushedThrough, no rollup
	rows        []queries.FlowRow
	meta        []queries.FlowMeta
	exporterIDs []int64
	touches     map[int64][]queries.FlowIfaceSeen
	points      []pointKey
	rollupHour  time.Time // non-zero: FlushedThrough crossed this hour boundary
}

type statusDelta struct {
	id    int64
	delta queries.FlowExporterStatusDelta
}

// templateWithdraw deletes templates an exporter withdrew.
type templateWithdraw struct {
	exporterID int64
	keys       []queries.FlowTemplateKey
}

// writeOp is one unit of work for the writer goroutine; exactly one field is
// set.
type writeOp struct {
	flush           *flushBatch
	template        *queries.FlowTemplate
	deleteTemplates int64
	withdraw        *templateWithdraw
	status          []statusDelta
	startupRollup   time.Time
}

// submit hands op to the writer without blocking. A full channel drops it:
// flush batches count writer_dropped. During shutdown (drainBy set) it waits
// for room until that deadline instead, so the final flush is not lost to a
// momentarily full channel. Tests set syncWrites to apply inline.
func (c *Collector) submit(op writeOp) bool {
	if c.syncWrites {
		c.applyWrite(op)
		return true
	}
	var wait <-chan time.Time
	if !c.drainBy.IsZero() {
		timer := time.NewTimer(max(time.Until(c.drainBy), 0))
		defer timer.Stop()
		wait = timer.C
	}
	if wait != nil {
		select {
		case c.writeCh <- op:
			return true
		case <-wait:
		}
	}
	select {
	case c.writeCh <- op:
		return true
	default:
		if op.flush != nil {
			c.global.writerDropped.Add(1)
			log.Printf("flow collector: writer busy, dropped the batch of %s", op.flush.minute.Format("15:04"))
		}
		return false
	}
}

// submitMaint is submit for template ops: it queues op only while at least
// half the writer channel is free, so template churn (a sender withdrawing
// and announcing templates at the rate limit) can never crowd out the flush
// batches that carry every exporter's minute. The caller retries a refused
// op on the exporter's next announcement.
func (c *Collector) submitMaint(op writeOp) bool {
	if !c.syncWrites && c.drainBy.IsZero() && len(c.writeCh) >= writerCap/2 {
		return false
	}
	return c.submit(op)
}

// writerLoop applies ops until the channel is closed.
func (c *Collector) writerLoop() {
	for op := range c.writeCh {
		c.applyWrite(op)
	}
}

func (c *Collector) applyWrite(op writeOp) {
	defer func() {
		if r := recover(); r != nil {
			c.global.panics.Add(1)
			log.Printf("flow collector: panic in writer: %v", r)
		}
	}()
	if c.db == nil {
		return
	}
	switch {
	case op.flush != nil:
		c.applyFlush(op.flush)
	case op.template != nil:
		if err := queries.UpsertFlowTemplate(c.db, *op.template); err != nil {
			log.Printf("flow collector: save template %d: %v", op.template.TemplateID, err)
		} else if _, err := queries.TrimFlowTemplates(c.db, op.template.ExporterID, decode.MaxTemplates); err != nil {
			log.Printf("flow collector: trim templates: %v", err)
		}
	case op.deleteTemplates != 0:
		if err := queries.DeleteFlowTemplates(c.db, op.deleteTemplates); err != nil {
			log.Printf("flow collector: delete templates: %v", err)
		}
	case op.withdraw != nil:
		if err := queries.DeleteFlowTemplateKeys(c.db, op.withdraw.exporterID, op.withdraw.keys); err != nil {
			log.Printf("flow collector: delete withdrawn templates: %v", err)
		}
	case len(op.status) > 0:
		for _, s := range op.status {
			if err := queries.AddFlowExporterStatus(c.db, s.id, s.delta); err != nil {
				log.Printf("flow collector: persist status: %v", err)
			}
		}
	case !op.startupRollup.IsZero():
		c.rollup(op.startupRollup, startupRollupHours)
		c.rollupUnrolled(op.startupRollup)
	}
}

func (c *Collector) applyFlush(b *flushBatch) {
	if len(b.rows) > 0 || len(b.meta) > 0 {
		if err := queries.InsertFlows1m(c.db, b.rows, b.meta); err != nil {
			log.Printf("flow collector: write %s: %v", b.minute.Format("15:04"), err)
			return
		}
	}
	for id, seen := range b.touches {
		if err := queries.TouchFlowIfaces(c.db, id, seen, b.minute); err != nil {
			log.Printf("flow collector: touch interfaces: %v", err)
		}
	}
	if len(b.points) > 0 {
		c.ensureNativePoints(b.points)
	}
	if b.final {
		return
	}
	through := b.minute.Add(time.Minute)
	c.flushedThrough.Store(through.Unix())
	if len(b.exporterIDs) > 0 && c.hub != nil && c.hub.TopicSubscriberCount(TopicFlushed) > 0 {
		c.hub.Publish(TopicFlushed, FlushedEvent{ExporterIDs: b.exporterIDs, Bucket: b.minute, FlushedThrough: through})
	}
	if !b.rollupHour.IsZero() {
		c.rollup(b.rollupHour, 2)
	}
}

// rollupUnrolled rolls every complete hour before now that has flow_1m
// minutes but no flow_1h row (startup after a downtime longer than the
// startup re-roll: the hour in progress at shutdown, or one whose xx:02
// rollup never ran), within flow_1m's retention minus one hour. Such an hour
// has no hourly row to overwrite, so the 3 h look-back clamp does not apply.
func (c *Collector) rollupUnrolled(now time.Time) {
	hi := now.UTC().Truncate(time.Hour)
	lo := hi.Add(-(time.Duration(Retention1mDays(c.db))*24*time.Hour - time.Hour))
	hours, err := queries.UnrolledFlowHours(c.db, lo, hi)
	if err != nil {
		log.Printf("flow collector: startup rollup: %v", err)
		return
	}
	topN := TopNHourly(c.db)
	for _, h := range hours {
		if err := queries.RollupFlows1h(c.db, h.ExporterID, h.Hour, topN); err != nil {
			log.Printf("flow collector: rollup %s of exporter %d: %v", h.Hour.Format("2006-01-02 15:04"), h.ExporterID, err)
		}
	}
	if len(hours) > 0 {
		log.Printf("flow collector: rolled %d hour(s) left unrolled by the previous run", len(hours))
	}
}

// rollup re-rolls the `hours` complete hours before end into flow_1h for
// every exporter, never looking back further than min(3 h, flow_1m_days·24 h
// − 1 h) (an hour partly swept from flow_1m must not be overwritten), then
// advances RolledThrough to end.
func (c *Collector) rollup(end time.Time, hours int) {
	end = end.UTC().Truncate(time.Hour)
	lookback := min(rollupMaxLookback, time.Duration(Retention1mDays(c.db))*24*time.Hour-time.Hour)
	exps, err := queries.ListFlowExporters(c.db)
	if err != nil {
		log.Printf("flow collector: rollup: %v", err)
		return
	}
	topN := TopNHourly(c.db)
	for h := end.Add(-time.Duration(hours) * time.Hour); h.Before(end); h = h.Add(time.Hour) {
		if h.Before(end.Add(-lookback)) {
			continue
		}
		for _, e := range exps {
			if err := queries.RollupFlows1h(c.db, e.ID, h, topN); err != nil {
				log.Printf("flow collector: rollup %s of %s: %v", h.Format("15:04"), e.Name, err)
			}
		}
	}
	c.rolledThrough.Store(end.Unix())
}
