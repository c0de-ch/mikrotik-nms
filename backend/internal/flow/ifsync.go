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

// syncExporterIfaces maps a RouterOS exporter's ifIndexes to names: the
// hex .id of /interface/print is the flow-export ifIndex, and
// /interface/vlan adds vlan-id and parent. Manual names survive.
func syncExporterIfaces(db *sql.DB, pool *routeros.Pool, exp queries.FlowExporter) error {
	if pool == nil || exp.DeviceID == "" {
		return errNoClient
	}
	client := pool.GetLive(exp.DeviceID)
	if client == nil {
		return errNoClient
	}
	stats, err := routeros.GetInterfaceStats(client)
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
		if err := syncExporterIfaces(c.db, c.pool, e); err != nil && !errors.Is(err, errNoClient) {
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
	if err := syncExporterIfaces(c.db, c.pool, *e); err != nil && !errors.Is(err, errNoClient) {
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
