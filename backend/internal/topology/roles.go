package topology

import (
	"database/sql"
	"slices"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// DeviceRole is a managed device's role (devclass.ClassifyRole) with the
// facts the flow guardrails also need. It is derived from stored rows only:
// the inventory columns of the device, its fresh device_uplinks rows, the
// interface list and the bridge port table.
type DeviceRole struct {
	Role   devclass.Role
	Reason string
	// Probed is the class the hardware probe derived (C0Unknown = never
	// probed); Budget the class the NMS budgets the device as — its
	// effective class, or C1 while it is unprobed or on the light profile.
	Probed, Budget devclass.Class
	// Light reports the light polling profile, Overridden an admin class
	// override that is in force (both make Budget gentler than Probed).
	Light, Overridden bool
	// Egress is the device's own fresh egress and EgressIfaces the
	// interfaces carrying it (internet first, then VPN; sorted within).
	Egress       devclass.Egress
	EgressIfaces []string
}

// deviceEgress is what a device's fresh uplink rows say about its routes.
type deviceEgress struct {
	internet, vpn []string // interfaces
	gateway       string   // next hop of a default route via another router on the LAN
}

// ownEgress reads each device's own egress out of fresh device_uplinks rows,
// by the rule the map uses (classifyDefaultRoute): a default route to a
// public next hop or out an interface is internet (VPN when the interface is
// a tunnel), a running tunnel interface is VPN. A default route via a managed
// device or a private gateway is not an egress of its own — every switch has
// one for its management address — and is kept as the gateway; gateway-host
// rows say where a gateway is plugged in, not where the device routes.
func ownEgress(devices []queries.Device, uplinks []queries.DeviceUplink) map[string]*deviceEgress {
	managedIP := make(map[string]bool, len(devices))
	for _, d := range devices {
		managedIP[d.Address] = true
	}
	out := map[string]*deviceEgress{}
	get := func(id string) *deviceEgress {
		e := out[id]
		if e == nil {
			e = &deviceEgress{}
			out[id] = e
		}
		return e
	}
	add := func(list *[]string, iface string) {
		if !slices.Contains(*list, iface) {
			*list = append(*list, iface)
		}
	}
	for _, u := range uplinks {
		switch u.Kind {
		case "default-route":
			switch classifyDefaultRoute(u.GatewayIP, u.Interface, u.IfaceType, managedIP) {
			case egressInternet:
				add(&get(u.DeviceID).internet, u.Interface)
			case egressVPN:
				add(&get(u.DeviceID).vpn, u.Interface)
			case egressGateway:
				get(u.DeviceID).gateway = u.GatewayIP
			case egressNone:
				if u.GatewayIP != "" {
					get(u.DeviceID).gateway = u.GatewayIP
				}
			}
		case "vpn":
			if u.Interface != "" {
				add(&get(u.DeviceID).vpn, u.Interface)
			}
		}
	}
	for _, e := range out {
		slices.Sort(e.internet)
		slices.Sort(e.vpn)
	}
	return out
}

// budgetClass is the class a device is budgeted as: its effective class, or
// C1 while it is unprobed (EffectiveClass already says so) or on the light
// profile — the rule the live-view caps and the probe sources follow.
func budgetClass(d *queries.Device) devclass.Class {
	if d.Profile() == devclass.ProfileLight {
		return devclass.C1
	}
	return d.EffectiveClass().Budgeted()
}

// DeviceRoles classifies every device. uplinks must be the rows that are
// current (queries.ListFreshDeviceUplinks): a stale route proves nothing.
// radios and bridgePorts are queries.DeviceRoleCounts.
func DeviceRoles(devices []queries.Device, uplinks []queries.DeviceUplink, radios, bridgePorts map[string]int) map[string]DeviceRole {
	egress := ownEgress(devices, uplinks)
	out := make(map[string]DeviceRole, len(devices))
	for i := range devices {
		d := &devices[i]
		facts := devclass.RoleFacts{Board: d.Board, Architecture: d.Architecture, Class: d.ProbedClass(),
			Radios: radios[d.ID], BridgePorts: bridgePorts[d.ID]}
		r := DeviceRole{Probed: d.ProbedClass(), Budget: budgetClass(d), Light: d.Profile() == devclass.ProfileLight}
		r.Overridden = d.ProbedClass() != devclass.C0Unknown && d.EffectiveClass() != d.ProbedClass()
		if e := egress[d.ID]; e != nil {
			facts.Gateway = e.gateway
			switch {
			case len(e.internet) > 0:
				facts.Egress, facts.EgressVia = devclass.EgressInternet, e.internet[0]
			case len(e.vpn) > 0:
				facts.Egress, facts.EgressVia = devclass.EgressVPN, e.vpn[0]
			}
			r.Egress = facts.Egress
			r.EgressIfaces = append(append([]string{}, e.internet...), e.vpn...)
		}
		r.Role, r.Reason = devclass.ClassifyRole(facts)
		out[d.ID] = r
	}
	return out
}

// LoadDeviceRoles reads the devices and the facts their roles rest on and
// classifies them. Uplink rows are judged by age (nil = the fleet-wide
// default window), exactly as the map judges them. It costs no device a
// command.
func LoadDeviceRoles(db *sql.DB, age queries.UplinkAge, now time.Time) (map[string]DeviceRole, []queries.Device, error) {
	devices, err := queries.ListDevices(db)
	if err != nil {
		return nil, nil, err
	}
	uplinks, err := queries.ListFreshDeviceUplinks(db, age, now)
	if err != nil {
		return nil, nil, err
	}
	radios, bridgePorts, err := queries.DeviceRoleCounts(db)
	if err != nil {
		return nil, nil, err
	}
	return DeviceRoles(devices, uplinks, radios, bridgePorts), devices, nil
}
