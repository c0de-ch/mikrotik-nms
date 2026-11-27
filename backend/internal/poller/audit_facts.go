package poller

import (
	"log"
	"net/netip"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
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

	if roles, _, err := topology.LoadDeviceRoles(a.db, UplinkAge(a.cadence), now); err != nil {
		log.Printf("poller audit: role of %s: %v", name, err)
	} else if r, ok := roles[dev.ID]; ok {
		f.Role, f.RoleReason = r.Role, r.Reason
	}
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
