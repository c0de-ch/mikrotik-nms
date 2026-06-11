package routeros

import (
	"strings"

	ros "github.com/go-routeros/routeros/v3"
)

// DeviceAddress is one address configured on a device, annotated with the
// VLAN id when its interface is an /interface/vlan.
type DeviceAddress struct {
	Address   string `json:"address"`   // as configured, e.g. 192.168.28.26/24
	IP        string `json:"ip"`        // host part, e.g. 192.168.28.26
	Interface string `json:"interface"` // e.g. net28, bridge
	VlanID    string `json:"vlan_id"`   // e.g. "28" when the interface is a VLAN, else ""
	Family    string `json:"family"`    // "ip" | "ipv6"
}

// ListAddresses returns the device's usable source addresses for
// self-originated traffic (/tool/fetch src-address, /ping src-address):
// enabled IPv4 plus global IPv6 addresses. Link-local IPv6 is skipped —
// fetch cannot source from fe80:: without a zone. VLAN annotation is
// best-effort: a device with no /interface/vlan rows simply gets none.
func ListAddresses(client *ros.Client) ([]DeviceAddress, error) {
	vlanIDs := map[string]string{}
	if reply, err := RunCommand(client, "/interface/vlan/print", "=.proplist=name,vlan-id"); err == nil {
		for _, re := range reply.Re {
			m := GetSentenceMap(re)
			if m["name"] != "" {
				vlanIDs[m["name"]] = m["vlan-id"]
			}
		}
	}

	var out []DeviceAddress
	for _, src := range []struct{ path, family string }{
		{"/ip/address/print", "ip"},
		{"/ipv6/address/print", "ipv6"},
	} {
		reply, err := RunCommand(client, src.path, "=.proplist=address,interface,disabled")
		if err != nil {
			if src.family == "ipv6" {
				continue // IPv6 package may be disabled on the device
			}
			return nil, err
		}
		for _, re := range reply.Re {
			m := GetSentenceMap(re)
			if m["disabled"] == "true" {
				continue
			}
			addr := m["address"]
			ip := addr
			if i := strings.IndexByte(ip, '/'); i >= 0 {
				ip = ip[:i]
			}
			if ip == "" {
				continue
			}
			// Loopback and link-local are never valid fetch sources.
			if ip == "::1" || strings.HasPrefix(ip, "127.") {
				continue
			}
			if src.family == "ipv6" && strings.HasPrefix(strings.ToLower(ip), "fe80") {
				continue
			}
			out = append(out, DeviceAddress{
				Address:   addr,
				IP:        ip,
				Interface: m["interface"],
				VlanID:    vlanIDs[m["interface"]],
				Family:    src.family,
			})
		}
	}
	return out, nil
}
