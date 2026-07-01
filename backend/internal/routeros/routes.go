package routeros

import (
	"strings"

	ros "github.com/go-routeros/routeros/v3"
)

// DefaultRoute is one active IPv4 default route on a device.
type DefaultRoute struct {
	Gateway   string // next-hop IP ("" when the route is interface-only)
	Interface string // egress interface ("" when not derivable)
}

// GetDefaultRoutes lists the device's active 0.0.0.0/0 routes in the main
// routing table. RouterOS encodes the resolved egress as
// "immediate-gw=192.168.78.81%bridge" (or just "eth1-wan" for interface
// routes); both forms are parsed defensively.
func GetDefaultRoutes(client *ros.Client) ([]DefaultRoute, error) {
	reply, err := RunCommand(client, "/ip/route/print", "?dst-address=0.0.0.0/0")
	if err != nil {
		return nil, err
	}

	var out []DefaultRoute
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		if m["active"] != "true" || m["disabled"] == "true" {
			continue
		}
		// Ignore VRF/policy tables — the map shows the primary egress.
		if rt, ok := m["routing-table"]; ok && rt != "" && rt != "main" {
			continue
		}

		r := DefaultRoute{}
		// gateway can be an IP, an interface name, or "ip%iface".
		gw := m["gateway"]
		if ip, iface, found := strings.Cut(gw, "%"); found {
			r.Gateway, r.Interface = ip, iface
		} else if strings.Contains(gw, ".") {
			r.Gateway = gw
		} else if gw != "" {
			r.Interface = gw
		}
		// immediate-gw carries the resolved egress interface; prefer it.
		if ig := m["immediate-gw"]; ig != "" {
			if ip, iface, found := strings.Cut(ig, "%"); found {
				r.Interface = iface
				if r.Gateway == "" && strings.Contains(ip, ".") {
					r.Gateway = ip
				}
			} else if !strings.Contains(ig, ".") {
				r.Interface = ig
			}
		}
		if r.Gateway == "" && r.Interface == "" {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
