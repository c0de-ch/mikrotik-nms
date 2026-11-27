package poller

import (
	"log"
	"net/netip"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/hygiene"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// facts gathers what the NMS already knows about a device for a pass: its
// class and role, the interface snapshot with the ethernet properties and
// the port rates (the port-stats collector's memory), the bridge port table
// (what the network-health poller stored), the links to other managed
// devices, and the counters the previous pass left behind. None of it costs
// the device a command; what is not at hand stays unknown, and the rules
// that need it judge nothing.
func (a *Auditor) facts(dev queries.Device, now time.Time) hygiene.Facts {
	f := hygiene.Facts{
		Class: dev.ProbedClass(), Board: dev.Board, Architecture: dev.Architecture, Version: dev.ROSVersion,
		NMSUser: dev.Username, CollectorPort: a.collectorPort, Now: now,
	}
	name := nonempty(dev.Identity, dev.Address)

	f.Role, f.RoleReason, f.RoleMissing = a.roleOf(dev, now)
	if a.self != nil {
		f.NMSAddresses = a.self(&dev, now)
	}
	if v, err := queries.GetSetting(a.db, "flow_advertise_address"); err == nil {
		if addr, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil {
			f.Collector = addr.Unmap().String()
		}
	}
	if caps, ok := a.capsOf(dev.ID); ok {
		f.WifiPackage = caps.Wifi == CapPresent
		f.LegacyCAPsMAN = caps.CAPsMAN == CapPresent
	}
	if raw, _, err := queries.LatestAuditRuntime(a.db, dev.ID); err == nil {
		if rt, ok := hygiene.ParseRuntime(raw); ok {
			f.Prev = &rt
		}
	}

	// The interface snapshot: Tier 0 rows with the Tier 4a row merged in.
	snap, ok := a.ifaces(dev.ID)
	if !ok {
		return f
	}
	f.InterfacesKnown = true
	f.EthernetKnown = !snap.EthernetAt.IsZero()
	rates := a.rates(dev.ID)
	trunks := a.trunkPorts(dev.ID)
	bridgePorts, err := queries.ListBridgePortStatusByDevice(a.db, dev.ID)
	if err != nil {
		log.Printf("poller audit: bridge ports of %s: %v", name, err)
	}
	members := make(map[string]queries.BridgePortStatus, len(bridgePorts))
	for _, bp := range bridgePorts {
		members[bp.PortInterface] = bp
	}
	f.BridgePortsKnown = len(bridgePorts) > 0

	for _, row := range snap.Rows {
		p := hygiene.Port{Name: row.Name, Type: row.Type, Running: row.Running && !row.Disabled, Trunk: trunks[row.Name]}
		switch row.Type {
		case "bridge":
			f.HasBridge = true
		case "bond":
			f.HasBond = true
		}
		if row.HasEthernet {
			p.Switch = row.Ethernet.Switch
			p.RxFlowControl, p.TxFlowControl = row.Ethernet.RxFlowControl, row.Ethernet.TxFlowControl
			p.LoopProtect = row.Ethernet.LoopProtect
		}
		if bps, ok := rates[row.Name]; ok {
			p.Bps, p.RateKnown = bps, true
		}
		if bp, ok := members[row.Name]; ok {
			f.HasBridge = true
			p.Bridge, p.Horizon = bp.BridgeName, bp.Horizon
			p.BridgeDisabled = bp.Disabled != nil && *bp.Disabled
			p.BridgeInactive = bp.Inactive != nil && *bp.Inactive
			if bp.HW != nil {
				p.HW, p.HWKnown = *bp.HW, true
			}
			if bp.HWOffload != nil {
				p.HWOffload, p.HWOffloadKnown = *bp.HWOffload, true
			}
		}
		f.Ports = append(f.Ports, p)
	}
	return f
}

// trunkPorts is the set of a device's ports that are an end of a link to
// another managed device (the link graph the topology loop keeps).
func (a *Auditor) trunkPorts(deviceID string) map[string]bool {
	out := map[string]bool{}
	links, err := queries.ListLinks(a.db)
	if err != nil {
		return out
	}
	prefix := deviceID + ":"
	for key := range interSwitchPorts(links) {
		if name, ok := strings.CutPrefix(key, prefix); ok {
			out[name] = true
		}
	}
	return out
}

// roleOf is the device's role for a pass, and whether it is missing: the
// lookup failed, or the classifier answered "unknown" while every route row
// the device has is too old to count. Such a device is unknown for want of
// facts — after a restart, or while it is too busy to answer its route read
// — and the rules that depend on the role must leave its findings alone. A
// device without any route row is unknown for what it is (a read that finds
// no default route leaves none), and is judged as that: the findings of a
// device whose role really changed do not stay for ever.
func (a *Auditor) roleOf(dev queries.Device, now time.Time) (role devclass.Role, reason string, missing bool) {
	name := nonempty(dev.Identity, dev.Address)
	roles, err := a.loadRoles(now)
	if err != nil {
		log.Printf("poller audit: role of %s: %v", name, err)
		return devclass.RoleUnknown, "the role could not be read from the database", true
	}
	r, ok := roles[dev.ID]
	if !ok {
		return devclass.RoleUnknown, "the device was not among the classified devices", true
	}
	if r.Role != devclass.RoleUnknown {
		return r.Role, r.Reason, false
	}
	stale, err := a.routeFactsStale(dev.ID, now)
	if err != nil {
		log.Printf("poller audit: routes of %s: %v", name, err)
		return devclass.RoleUnknown, "the device's routes could not be read from the database", true
	}
	if stale {
		return devclass.RoleUnknown, "the device's routes have not been read recently, so its role cannot be told right now", true
	}
	return r.Role, r.Reason, false
}

// routeFactsStale reports a device that has default-route or VPN rows in
// device_uplinks, none of them current: its last successful route read is
// too long ago (the rule the flow address plan applies, flow.staleUplinks).
func (a *Auditor) routeFactsStale(deviceID string, now time.Time) (bool, error) {
	own := func(u queries.DeviceUplink) bool {
		return u.DeviceID == deviceID && (u.Kind == "default-route" || u.Kind == "vpn")
	}
	fresh, err := queries.ListFreshDeviceUplinks(a.db, UplinkAge(a.cadence), now)
	if err != nil {
		return false, err
	}
	for _, u := range fresh {
		if own(u) {
			return false, nil
		}
	}
	all, err := queries.ListDeviceUplinks(a.db, 365*24*time.Hour)
	if err != nil {
		return false, err
	}
	for _, u := range all {
		if own(u) {
			return true, nil
		}
	}
	return false, nil
}

// loadRoles classifies the devices from the stored facts (a.roles in tests).
func (a *Auditor) loadRoles(now time.Time) (map[string]topology.DeviceRole, error) {
	if a.roles != nil {
		return a.roles(now)
	}
	roles, _, err := topology.LoadDeviceRoles(a.db, UplinkAge(a.cadence), now)
	return roles, err
}
