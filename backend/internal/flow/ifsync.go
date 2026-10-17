package flow

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"runtime/debug"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// ifsync / maintenance cadence (§8.10).
const (
	ifsyncFirstDelay  = 20 * time.Second // the pool dials devices first
	ifsyncInterval    = 5 * time.Minute
	ifsyncOnDemandGap = time.Minute // an unknown ifIndex triggers at most one sync per minute
	pointsInterval    = 30 * time.Second
)

var errNoClient = errors.New("no pooled connection")

// IfaceSource serves a managed device's latest scheduled interface read
// from memory — the port-stats collector's Tier 0 snapshot — so the ifIndex
// sync needs no interface print of its own.
type IfaceSource interface {
	DeviceSnapshot(deviceID string) (rows []routeros.InterfaceSnapshot, at time.Time, ok bool)
}

// UseIfaceSource makes the ifIndex sync take interface names from src; the
// device's interface table is printed only while src has no snapshot of it
// (cold start, a device the collector cannot read). Call before Run.
func (c *Collector) UseIfaceSource(src IfaceSource) {
	if c != nil {
		c.ifaceSrc = src
	}
}

// exporterIfaces returns deviceID's interface rows: src's snapshot when it
// holds one, else one Tier 0 print on run.
func exporterIfaces(src IfaceSource, run routeros.Runner, deviceID string) ([]routeros.InterfaceStats, error) {
	if src != nil {
		if rows, _, ok := src.DeviceSnapshot(deviceID); ok {
			stats := make([]routeros.InterfaceStats, len(rows))
			for i, r := range rows {
				stats[i] = r.InterfaceStats
			}
			return stats, nil
		}
	}
	return routeros.GetInterfaceStats(run)
}

// syncExporterIfaces maps a RouterOS exporter's ifIndexes to names: the hex
// .id of /interface/print is the flow-export ifIndex — taken from src's
// snapshot when it has one, else printed — and /interface/vlan adds vlan-id
// and parent. Manual names survive. A device whose polling is paused is not
// read (its datagrams are still decoded with the names it had).
func syncExporterIfaces(db *sql.DB, pool *routeros.Pool, src IfaceSource, exp queries.FlowExporter) error {
	if pool == nil || exp.DeviceID == "" {
		return errNoClient
	}
	if dev, err := queries.GetDevice(db, exp.DeviceID); err == nil && dev.PollingPaused(time.Now()) {
		return errNoClient
	}
	client := pool.GetLive(exp.DeviceID)
	if client == nil {
		return errNoClient
	}
	stats, err := exporterIfaces(src, routeros.ClientRunner{Client: client}, exp.DeviceID)
	if err != nil {
		return err
	}
	vlans := map[string]routeros.VLANInfo{}
	if list, err := routeros.GetVLANInfo(client); err == nil {
		for _, v := range list {
			vlans[v.Name] = v
		}
	}
	return queries.UpsertDeviceFlowIfaces(db, exp.ID, ifacesFromStats(stats, vlans))
}

// ifacesFromStats converts /interface/print rows (and the VLAN table) into
// flow interface rows; rows whose .id is not a valid ifIndex are skipped.
func ifacesFromStats(stats []routeros.InterfaceStats, vlans map[string]routeros.VLANInfo) []queries.FlowIface {
	out := make([]queries.FlowIface, 0, len(stats))
	for _, s := range stats {
		idx, ok := routeros.IfIndexFromID(s.ID)
		if !ok || s.Name == "" {
			continue
		}
		f := queries.FlowIface{IfIndex: idx, Name: s.Name, Type: s.Type}
		if v, ok := vlans[s.Name]; ok {
			f.VLANID, f.Parent = v.VLANID, v.Parent
		}
		out = append(out, f)
	}
	return out
}

// ifsyncAll syncs every enabled RouterOS exporter with a device.
func (c *Collector) ifsyncAll() {
	exps, err := queries.ListFlowExporters(c.db)
	if err != nil {
		log.Printf("flow ifsync: list exporters: %v", err)
		return
	}
	for _, e := range exps {
		if e.DeviceID == "" || !e.Enabled {
			continue
		}
		if err := syncExporterIfaces(c.db, c.pool, c.ifaceSrc, e); err != nil && !errors.Is(err, errNoClient) {
			log.Printf("flow ifsync: %s: %v", e.Name, err)
		}
	}
}

// ifsyncOne syncs one exporter on demand (an unknown ifIndex appeared).
func (c *Collector) ifsyncOne(id int64) {
	e, err := queries.GetFlowExporter(c.db, id)
	if err != nil || e == nil || e.DeviceID == "" {
		return
	}
	if err := syncExporterIfaces(c.db, c.pool, c.ifaceSrc, *e); err != nil && !errors.Is(err, errNoClient) {
		log.Printf("flow ifsync: %s: %v", e.Name, err)
	}
}

// safeMaint runs one maintenance step, recovering a panic.
func safeMaint(what string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("flow collector: panic in %s: %v\n%s", what, r, debug.Stack())
		}
	}()
	f()
}

// maintLoop runs the pool- and DB-heavy side jobs off the worker: ifsync
// every 5 min (and on demand), the address plan every 15 min (first after
// 60 s; again after 60 s while uplink rows are stale) and the native-point
// facing reconcile every 30 s.
func (c *Collector) maintLoop(ctx context.Context) {
	ifsyncT := time.NewTimer(ifsyncFirstDelay)
	planT := time.NewTimer(planFirstDelay)
	pointsT := time.NewTicker(pointsInterval)
	defer ifsyncT.Stop()
	defer planT.Stop()
	defer pointsT.Stop()
	lastOnDemand := map[int64]time.Time{}
	planRetries := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ifsyncT.C:
			safeMaint("ifsync", func() {
				c.ifsyncAll()
				reconcileNativePoints(c.db)
			})
			ifsyncT.Reset(ifsyncInterval)
		case id := <-c.ifsyncReq:
			now := c.now()
			if now.Sub(lastOnDemand[id]) < ifsyncOnDemandGap {
				continue
			}
			lastOnDemand[id] = now
			safeMaint("ifsync", func() {
				c.ifsyncOne(id)
				reconcileNativePoints(c.db)
			})
		case <-planT.C:
			retry := false
			safeMaint("plan", func() { retry = refreshPlan(c.db, c.pool, c.now()) })
			if retry && planRetries < planMaxRetries {
				planRetries++
				planT.Reset(planRetry)
			} else {
				planRetries = 0
				planT.Reset(planInterval)
			}
		case <-pointsT.C:
			safeMaint("points", func() { reconcileNativePoints(c.db) })
		}
	}
}
