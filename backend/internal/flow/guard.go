package flow

import (
	"cmp"
	"log"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowguard"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Flow-export guardrails (P0-7): which senders are dropped before decode
// (deleted exporters, managed switches / access points / C1 devices), which
// managed devices may be accepted automatically, and the corroborated alarm
// for a device that exports although it must not.

const (
	// guardNoteEvery bounds how often one sender's datagrams are recorded as
	// a sighting (a device_findings bump and a request for the confirmation
	// read): a sender exports every few seconds, the record is per minute.
	guardNoteEvery = time.Minute
	// exportCheckEvery is the budget of the confirmation read: two read-only
	// prints, at most once per hour per device.
	exportCheckEvery = time.Hour
	// guardQueueCap bounds the sightings waiting for the worker; a full
	// queue drops the note, and the sender's next datagram tries again.
	guardQueueCap = 64
)

// guardStats are the counters of one guarded address, shared by the gates of
// successive reloads so a reload neither loses drops nor repeats a sighting.
type guardStats struct {
	total    atomic.Int64 // datagrams dropped: the persisted count when first loaded, plus every drop since
	pending  atomic.Int64 // of those, not persisted yet
	lastSeen atomic.Int64 // unix seconds of the last dropped datagram
	noted    atomic.Int64 // unix seconds of the last sighting handed to the worker
}

// drop counts one dropped datagram.
func (s *guardStats) drop(now time.Time) {
	s.total.Add(1)
	s.pending.Add(1)
	s.lastSeen.Store(now.Unix())
}

// guardGate is the reader's view of an address whose flow datagrams are
// dropped before decode. It is immutable; a reload builds a new one.
type guardGate struct {
	addr netip.Addr
	// policy is the address's flow_sender_policy row. nil = a managed device
	// that must not export and has not sent anything yet: its first flow
	// datagram blocks it.
	policy *queries.FlowSenderPolicy
	// alarmDevice is the managed device the address belongs to when that
	// device must not export (switch, access point, CPE, C1); "" = a plain
	// block (a deleted router or firewall) that raises no alarm.
	alarmDevice string
	name        string // the device's name
	reason      string // why it must not export
	stats       *guardStats
}

// guardEvent is one sighting on its way from a reader to the worker.
type guardEvent struct {
	gate *guardGate
	at   time.Time
}

// noteGuard hands a sighting to the worker without blocking, at most once
// per guardNoteEvery per address.
func (c *Collector) noteGuard(g *guardGate, now time.Time) {
	ts := now.Unix()
	last := g.stats.noted.Load()
	if last != 0 && ts-last < int64(guardNoteEvery/time.Second) {
		return
	}
	if !g.stats.noted.CompareAndSwap(last, ts) {
		return // another reader noted it
	}
	select {
	case c.guardCh <- guardEvent{gate: g, at: now}:
	default:
		g.stats.noted.Store(last) // the worker is busy: the next datagram tries again
	}
}

// handleGuard is the worker's side of a sighting: the first flow datagram of
// a managed device that must not export blocks its address (a tombstone with
// the reason), and every sighting of such a device is recorded for the
// flow_export_on_switch alert.
func (c *Collector) handleGuard(ev guardEvent) {
	g := ev.gate
	if c.db == nil {
		return
	}
	if g.policy == nil {
		if g.alarmDevice == "" {
			return
		}
		created, err := queries.BlockFlowSender(c.db, &queries.FlowSenderPolicy{
			Address: g.addr.String(), Reason: g.reason, Source: queries.FlowBlockAuto, Name: g.name,
			Kind: "routeros", DeviceID: g.alarmDevice, CreatedAt: ev.at,
		})
		if err != nil {
			log.Printf("flow collector: block %s: %v", g.addr, err)
		} else if created {
			log.Printf("flow collector: blocked flow datagrams from %s (%s): %s", g.addr, g.name, g.reason)
		}
		c.reconcile(ev.at)
	}
	if g.alarmDevice != "" {
		c.noteExportSighting(g.alarmDevice, g.addr, ev.at)
	}
}

// noteExportSighting records that a flow datagram arrived from the address
// of a managed device that must not export, and asks the maintenance loop
// for the confirmation read. The finding is a warning until that read
// confirmed the export: a source address alone can be forged.
func (c *Collector) noteExportSighting(deviceID string, addr netip.Addr, at time.Time) {
	if c.db == nil {
		return
	}
	if err := queries.NoteFlowExportSighting(c.db, deviceID, addr.String(), ListenPort(c.cfg.Listen), at); err != nil {
		log.Printf("flow collector: record sighting of %s: %v", addr, err)
		return
	}
	if err := queries.UpsertFlowExportFinding(c.db, deviceID, flowguard.FindingExportOnSwitch,
		flowguard.FindingDetail(addr.String())); err != nil {
		log.Printf("flow collector: record finding for %s: %v", addr, err)
	}
	select {
	case c.exportCheckReq <- deviceID:
	default:
	}
}

// checkExport is the confirmation read behind the flow_export_on_switch
// alert, run by the maintenance loop: one read of the device's traffic-flow
// settings and targets — two read-only, proplisted prints on its pooled
// session — and it reports whether the read ran.
//
// It does not run when polling is off, on a paused device, without a
// sighting to confirm, without a pooled session, or within exportCheckEvery
// of the device's last read (the time is stored, so a restart does not reset
// the budget). Only a read that says the device exports makes the finding
// critical; "does not export" and a failed read leave it a warning.
func (c *Collector) checkExport(deviceID string, now time.Time) bool {
	if c.db == nil || routeros.PollingAllowed() != nil {
		return false
	}
	if last, ok := c.exportChecked[deviceID]; ok && now.Sub(last) < exportCheckEvery {
		return false
	}
	dev, err := queries.GetDevice(c.db, deviceID)
	if err != nil || dev.PollingPaused(now) {
		return false
	}
	chk, err := queries.GetFlowExportCheck(c.db, deviceID)
	if err != nil || chk == nil || chk.LastSeen.IsZero() {
		return false
	}
	if !chk.CheckedAt.IsZero() && now.Sub(chk.CheckedAt) < exportCheckEvery {
		return false
	}
	if c.pool == nil {
		return false
	}
	client := c.pool.GetLive(deviceID)
	if client == nil {
		return false // no session: the next sighting asks again
	}
	c.exportChecked[deviceID] = now

	name := dev.Identity
	if name == "" {
		name = dev.Address
	}
	result, detail := queries.FlowExportNotExporting, ""
	var targets []netip.AddrPort
	st, err := routeros.ReadTrafficFlow(routeros.ClientRunner{Client: client})
	switch {
	case err != nil:
		result, detail = queries.FlowExportCheckFailed, "the read of /ip/traffic-flow failed: "+err.Error()
		log.Printf("flow collector: %s: could not confirm its flow export: %v", name, err)
	case st.Exporting():
		result, detail = queries.FlowExportExporting, st.Summary()
		for _, t := range st.Targets {
			if a, err := netip.ParseAddr(t.DstAddress); err == nil && !t.Disabled && t.Port > 0 && t.Port <= 65535 {
				targets = append(targets, netip.AddrPortFrom(a.Unmap(), uint16(t.Port)))
			}
		}
		log.Printf("flow collector: %s exports flows although it must not (confirmed on the device: %s)", name, detail)
	default:
		detail = st.Summary()
		log.Printf("flow collector: flow datagrams carry the address of %s, but the device does not export (%s)", name, detail)
	}
	if err := queries.SetFlowExportCheckResult(c.db, deviceID, now, result, detail, targets); err != nil {
		log.Printf("flow collector: store the export check of %s: %v", name, err)
		return true
	}
	if err := queries.RefreshFlowExportFindingSeverity(c.db, deviceID, flowguard.FindingExportOnSwitch); err != nil {
		log.Printf("flow collector: update the finding of %s: %v", name, err)
	}
	return true
}

// UseAcceptHook registers fn to be called when the collector starts
// accepting an exporter: a managed router auto-accepted on its first
// datagram, an exporter an admin added, one whose address changed, and one
// switched back from "ignore datagrams". It is not called for the exporters
// found at startup. fn runs on the worker goroutine and must not block. Call
// before Run.
//
// G6 SEAM (flow-export CPU watch): this is the moment "an exporter is
// accepted". The watch of the device's 60 s CPU series for the following
// 30 minutes (flow_export_cpu_high) hooks in here; e.DeviceID names the
// device ("" = an exporter that is not a managed device, nothing to watch).
func (c *Collector) UseAcceptHook(fn func(e queries.FlowExporter, at time.Time)) {
	if c != nil {
		c.onAccepted = fn
	}
}

// exporterAccepted runs the accept hook (see UseAcceptHook).
func (c *Collector) exporterAccepted(e queries.FlowExporter, at time.Time) {
	if c.onAccepted != nil {
		c.onAccepted(e, at)
	}
}

// blockedSnapshot lists the blocked senders with their live counters,
// ordered by name, then address.
func (c *Collector) blockedSnapshot() []BlockedSender {
	out := []BlockedSender{}
	al := c.allow.Load()
	if al == nil {
		return out
	}
	for _, g := range al.guarded {
		if g.policy == nil {
			continue
		}
		b := BlockedSenderFromRow(*g.policy)
		b.Datagrams = g.stats.total.Load()
		if ls := g.stats.lastSeen.Load(); ls > 0 {
			if t := time.Unix(ls, 0).UTC(); b.LastSeen == nil || t.After(*b.LastSeen) {
				b.LastSeen = &t
			}
		}
		if dev, ok := al.devices[g.addr]; ok {
			b.DeviceName, b.DeviceRole, b.DeviceRoleReason = dev.name, dev.role, dev.reason
		}
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b BlockedSender) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Address, b.Address))
	})
	return out
}

// persistDrops writes the datagrams dropped from blocked addresses since the
// last persist (60 s tick and shutdown). A batch that cannot be queued is
// counted again next time.
func (c *Collector) persistDrops() {
	al := c.allow.Load()
	if al == nil {
		return
	}
	var drops []senderDrop
	var taken []*guardGate
	for _, g := range al.guarded {
		if g.policy == nil {
			continue
		}
		n := g.stats.pending.Swap(0)
		if n == 0 {
			continue
		}
		drops = append(drops, senderDrop{address: g.addr.String(), datagrams: n, lastSeen: time.Unix(g.stats.lastSeen.Load(), 0)})
		taken = append(taken, g)
	}
	if len(drops) == 0 {
		return
	}
	if !c.submit(writeOp{drops: drops}) {
		for i, g := range taken {
			g.stats.pending.Add(drops[i].datagrams)
		}
	}
}

// senderDrop is one blocked address's dropped datagrams for the writer.
type senderDrop struct {
	address   string
	datagrams int64
	lastSeen  time.Time
}
