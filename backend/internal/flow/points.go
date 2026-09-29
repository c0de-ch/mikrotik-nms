package flow

import (
	"database/sql"
	"fmt"
	"log"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// uplinkFreshness is how old a device_uplinks row may be to count (the
// topology poller refreshes them every cycle).
const uplinkFreshness = 10 * time.Minute

// pointKey is one (exporter, ifIndex) native point.
type pointKey struct {
	exporterID int64
	ifIndex    uint32
}

// pointContext is what native-point naming and facing need, read once per
// batch of points.
type pointContext struct {
	exporters map[int64]queries.FlowExporter
	ifaces    map[int64]map[uint32]queries.FlowIface
	uplinks   []queries.DeviceUplink
	ifTypes   map[string]map[string]string // device -> iface -> type (interfaces table)
	internal  []netip.Prefix               // flow_prefixes internal/vpn + flow_internal_prefixes
}

// loadPointContext reads the context for the given exporters (all when nil).
func loadPointContext(db *sql.DB) (*pointContext, error) {
	exps, err := queries.ListFlowExporters(db)
	if err != nil {
		return nil, err
	}
	ifs, err := queries.ListFlowIfaces(db, 0)
	if err != nil {
		return nil, err
	}
	ctx := &pointContext{
		exporters: make(map[int64]queries.FlowExporter, len(exps)),
		ifaces:    map[int64]map[uint32]queries.FlowIface{},
		ifTypes:   map[string]map[string]string{},
	}
	for _, e := range exps {
		ctx.exporters[e.ID] = e
		if e.DeviceID != "" && ctx.ifTypes[e.DeviceID] == nil {
			m := map[string]string{}
			if list, err := queries.ListInterfacesByDevice(db, e.DeviceID); err == nil {
				for _, i := range list {
					m[i.Name] = i.Type
				}
			}
			ctx.ifTypes[e.DeviceID] = m
		}
	}
	for _, f := range ifs {
		m := ctx.ifaces[f.ExporterID]
		if m == nil {
			m = map[uint32]queries.FlowIface{}
			ctx.ifaces[f.ExporterID] = m
		}
		m[f.IfIndex] = f
	}
	if ctx.uplinks, err = queries.ListDeviceUplinks(db, uplinkFreshness); err != nil {
		return nil, err
	}
	if plan, err := queries.ListFlowPrefixes(db); err == nil {
		for _, p := range plan {
			if p.Class == "internal" || p.Class == "vpn" {
				ctx.internal = append(ctx.internal, p.Prefix)
			}
		}
	}
	ctx.internal = append(ctx.internal, prefixListSetting(db, "flow_internal_prefixes")...)
	return ctx, nil
}

// nativeSpec computes the auto native point of (exporter, ifIndex): name,
// facing, managed port and local_internal (contract §6.4).
func (ctx *pointContext) nativeSpec(exp queries.FlowExporter, idx uint32) queries.FlowPoint {
	iface := ctx.ifaces[exp.ID][idx]
	label := iface.Name
	if label == "" {
		label = "if#" + strconv.FormatUint(uint64(idx), 10)
	}
	p := queries.FlowPoint{
		ExporterID:    exp.ID,
		Name:          exp.Name + " · " + label,
		Kind:          "native",
		IfIndexes:     []uint32{idx},
		Facing:        "down",
		LocalInternal: exp.Kind == "opnsense",
		ExcludeAttach: []queries.PortRef{},
		Auto:          true,
		Enabled:       true,
	}
	if exp.DeviceID != "" {
		p.PortSide, p.DeviceID, p.Iface = "same", exp.DeviceID, iface.Name
		if ctx.deviceIfaceFacesUp(exp.DeviceID, iface) {
			p.Facing = "up"
		}
		return p
	}
	if ctx.learnedIfaceFacesUp(exp, iface) {
		p.Facing = "up"
	}
	return p
}

// deviceIfaceFacesUp: a RouterOS interface faces the Internet when it is the
// interface of a fresh non-VPN default-route uplink, a fresh vpn uplink or a
// fresh gateway-host uplink of that device, or of a VPN type.
func (ctx *pointContext) deviceIfaceFacesUp(deviceID string, iface queries.FlowIface) bool {
	if iface.Name == "" {
		return false
	}
	typ := iface.Type
	if typ == "" {
		typ = ctx.ifTypes[deviceID][iface.Name]
	}
	if topology.IsVPNIfaceType(typ) {
		return true
	}
	for _, u := range ctx.uplinks {
		if u.DeviceID != deviceID || u.Interface != iface.Name {
			continue
		}
		switch u.Kind {
		case "default-route":
			if !topology.IsVPNIfaceType(u.IfaceType) {
				return true
			}
		case "vpn", "gateway-host":
			return true
		}
	}
	return false
}

// learnedIfaceFacesUp: for an exporter without a device the admin's role
// decides (wan/vpn up, lan/other down); without one, an interface faces up
// unless its learned ingress prefixes look local — no hint, a prefix holding
// the exporter's own address, or one overlapping the internal address plan.
func (ctx *pointContext) learnedIfaceFacesUp(exp queries.FlowExporter, iface queries.FlowIface) bool {
	switch iface.Role {
	case "wan", "vpn":
		return true
	case "lan", "other":
		return false
	}
	hints := parsePrefixCSV(iface.Hint)
	if len(hints) == 0 {
		return false
	}
	self, _ := netip.ParseAddr(exp.Address)
	self = self.Unmap()
	for _, h := range hints {
		if self.IsValid() && h.Contains(self) {
			return false
		}
		for _, in := range ctx.internal {
			if h.Overlaps(in) {
				return false
			}
		}
	}
	return true
}

// ensureNativePoints creates the missing native points of cands (writer
// goroutine, after the flush that saw their bytes) in one transaction. The
// log line naming them is throttled per exporter (wireLogEvery).
func (c *Collector) ensureNativePoints(cands []pointKey) {
	ctx, err := loadPointContext(c.db)
	if err != nil {
		log.Printf("flow collector: native points: %v", err)
		return
	}
	ps := make([]queries.FlowPoint, 0, len(cands))
	for _, k := range cands {
		exp, ok := ctx.exporters[k.exporterID]
		if !ok || k.ifIndex == 0 {
			continue
		}
		ps = append(ps, ctx.nativeSpec(exp, k.ifIndex))
	}
	created, err := queries.EnsureNativeFlowPoints(c.db, ps)
	if err != nil {
		log.Printf("flow collector: native points: %v", err)
		return
	}
	byExp := map[int64][]string{}
	var ids []int64
	for i, p := range ps {
		if !created[i] {
			continue
		}
		if byExp[p.ExporterID] == nil {
			ids = append(ids, p.ExporterID)
		}
		byExp[p.ExporterID] = append(byExp[p.ExporterID], fmt.Sprintf("%q (facing %s)", p.Name, p.Facing))
	}
	now := c.now()
	for _, id := range ids {
		lt := c.pointLog[id]
		if lt == nil {
			lt = &logThrottle{}
			c.pointLog[id] = lt
		}
		ok, held := lt.allow(now, wireLogEvery)
		if !ok {
			continue
		}
		names := byExp[id]
		more := ""
		if len(names) > maxPointNamesLogged {
			more = fmt.Sprintf(" and %d more", len(names)-maxPointNamesLogged)
			names = names[:maxPointNamesLogged]
		}
		log.Printf("flow collector: new observation point(s) %s%s%s", strings.Join(names, ", "), more, heldNote(held))
	}
}

// maxPointNamesLogged bounds the point names one log line lists.
const maxPointNamesLogged = 5

// staleUplinkDevices returns the devices that have egress rows
// (default-route, vpn, gateway-host) but none fresh: their uplink facts are
// unknown right now (device not polled, or right after a restart), not gone.
func staleUplinkDevices(fresh, all []queries.DeviceUplink) map[string]bool {
	hasFresh := map[string]bool{}
	for _, u := range fresh {
		hasFresh[u.DeviceID] = true
	}
	out := map[string]bool{}
	for _, u := range all {
		switch u.Kind {
		case "default-route", "vpn", "gateway-host":
			if !hasFresh[u.DeviceID] {
				out[u.DeviceID] = true
			}
		}
	}
	return out
}

// reconcileNativePoints recomputes every auto native point (names appear
// after an ifsync, uplinks and roles change) and rewrites the changed ones.
// The facing of a device exporter's point is only changed on fresh uplink
// evidence and only down → up: stale uplink rows (device not polled) and a
// vanished default route (WAN link down: the route is inactive) must not
// swap a WAN point's download and upload for its whole history. up → down
// is an admin edit (or the ifIndex now names another interface).
func reconcileNativePoints(db *sql.DB) {
	ctx, err := loadPointContext(db)
	if err != nil {
		log.Printf("flow collector: reconcile points: %v", err)
		return
	}
	points, err := queries.ListFlowPoints(db)
	if err != nil {
		log.Printf("flow collector: reconcile points: %v", err)
		return
	}
	all, err := queries.ListDeviceUplinks(db, 365*24*time.Hour)
	if err != nil {
		log.Printf("flow collector: reconcile points: %v", err)
		return
	}
	stale := staleUplinkDevices(ctx.uplinks, all)
	for _, p := range points {
		if p.Kind != "native" || !p.Auto || len(p.IfIndexes) != 1 {
			continue
		}
		exp, ok := ctx.exporters[p.ExporterID]
		if !ok {
			continue
		}
		want := ctx.nativeSpec(exp, p.IfIndexes[0])
		if exp.DeviceID != "" && want.Facing != p.Facing &&
			(stale[exp.DeviceID] || (p.Facing == "up" && want.Facing == "down" && want.Iface == p.Iface)) {
			want.Facing = p.Facing // (an ifIndex that now names another interface is recomputed)
		}
		if want.Name == p.Name && want.Facing == p.Facing && want.PortSide == p.PortSide &&
			want.DeviceID == p.DeviceID && want.Iface == p.Iface {
			continue
		}
		want.ID = p.ID
		if err := queries.UpdateAutoFlowPoint(db, &want); err != nil {
			log.Printf("flow collector: update point %q: %v", p.Name, err)
		} else if !strings.EqualFold(want.Name, p.Name) || want.Facing != p.Facing {
			log.Printf("flow collector: observation point %q is now %q (facing %s)", p.Name, want.Name, want.Facing)
		}
	}
}
