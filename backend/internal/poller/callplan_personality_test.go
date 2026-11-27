package poller

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/mikrotik-nms/backend/internal/hygiene/hygienetest"
)

// The call plans run the hygienetest personalities. A personality's fixture
// lists what the device answers to the audit and to the small scheduled
// reads (resource, firmware, log, capability probes, registrations, ARP,
// leases…). Its per-port tables — the interface list, the ethernet ports,
// the bridges with their ports and the FDB — are derived here from
// Personality.Ports, so what the device answers to the scheduled reads and
// what Personality.Facts says the NMS knows of it are the same thing by
// construction.

// planPersonality is a personality with the tables derived from its ports.
type planPersonality struct{ hygienetest.Personality }

// withPortTables returns p answering the per-port reads of the scheduled
// loops too. A table the fixture lists itself is kept.
func withPortTables(p hygienetest.Personality) hygienetest.Personality {
	d := planPersonality{p}
	tables := []struct {
		command string
		rows    []map[string]string
	}{
		{"/interface/print", d.interfaceRows()},
		{"/interface/ethernet/print", d.ethernetRows()},
		{"/interface/bridge/print", d.bridgeRows()},
		{"/interface/bridge/port/print", d.bridgePortRows()},
		{"/interface/bridge/monitor", d.bridgeMonitorRows()},
		{"/interface/bridge/host/print", d.fdbRows()},
	}
	if !d.stpInPrint() {
		tables = append(tables, struct {
			command string
			rows    []map[string]string
		}{"/interface/bridge/port/monitor", d.bridgePortMonitorRows()})
	}
	for _, t := range tables {
		if _, own := p.Replies[t.command]; !own {
			p = p.With(t.command, t.rows...)
		}
	}
	return p
}

func (p planPersonality) nameHash() uint32 {
	h := fnv.New32a()
	h.Write([]byte(p.Name))
	return h.Sum32()
}

// mac is a stable locally administered MAC address for the personality's
// n-th interface (its position in Ports): what its interface list prints,
// and what its neighbours hear it as.
func (p planPersonality) mac(n int) string {
	sum := p.nameHash()
	return fmt.Sprintf("02:%02X:%02X:%02X:%02X:%02X", byte(sum>>16), byte(sum>>8), byte(sum), byte(n>>8), byte(n))
}

// portIndex is the position of the named port in Ports (-1 = none).
func (p planPersonality) portIndex(name string) int {
	for i, port := range p.Ports {
		if port.Name == name {
			return i
		}
	}
	return -1
}

// stpInPrint reports a RouterOS whose bridge port print carries the STP
// runtime fields (7.24 and later); older versions need a port monitor.
func (p planPersonality) stpInPrint() bool {
	parts := strings.SplitN(p.Version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	return major > 7 || (major == 7 && minor >= 24)
}

func rosBool(b bool) string { return strconv.FormatBool(b) }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// bondMembers maps a bond's member port to its bond, from the fixture's own
// bonding print.
func (p planPersonality) bondMembers() map[string]string {
	out := map[string]string{}
	for _, row := range p.Replies["/interface/bonding/print"].Rows {
		for _, member := range strings.Split(row["slaves"], ",") {
			if member = strings.TrimSpace(member); member != "" {
				out[member] = row["name"]
			}
		}
	}
	return out
}

// interfaceRows is the /interface/print table. The counters stand still: a
// quiet device, whose rates are all zero.
func (p planPersonality) interfaceRows() []map[string]string {
	bonded := p.bondMembers()
	rows := make([]map[string]string, 0, len(p.Ports))
	for i, port := range p.Ports {
		row := map[string]string{
			".id": fmt.Sprintf("*%X", i+1), "name": port.Name, "type": port.Type, "mac-address": p.mac(i),
			"mtu": "1500", "actual-mtu": "1500", "running": rosBool(port.Running), "disabled": "false",
			"slave":      rosBool(port.Bridge != "" || bonded[port.Name] != ""),
			"link-downs": "0",
			"rx-byte":    strconv.Itoa(1_000_000 * (i + 1)), "tx-byte": strconv.Itoa(2_000_000 * (i + 1)),
			"rx-packet": strconv.Itoa(1_000 * (i + 1)), "tx-packet": strconv.Itoa(2_000 * (i + 1)),
			"rx-error": "0", "tx-error": "0", "rx-drop": "0", "tx-drop": "0", "tx-queue-drop": "0",
			"fp-rx-byte": "0", "fp-tx-byte": "0",
		}
		if port.Running {
			row["last-link-up-time"] = "2026-01-04 08:00:00"
		}
		rows = append(rows, row)
	}
	return rows
}

// ethernetRows is the /interface/ethernet/print table: the ether ports with
// their switch chip, flow control and loop-protect setting.
func (p planPersonality) ethernetRows() []map[string]string {
	var rows []map[string]string
	for i, port := range p.Ports {
		if port.Type != "ether" {
			continue
		}
		row := map[string]string{
			".id": fmt.Sprintf("*%X", i+1), "name": port.Name, "default-name": port.Name, "mac-address": p.mac(i),
			"loop-protect": orDefault(port.LoopProtect, "default"), "loop-protect-status": "off",
			"rx-flow-control": orDefault(port.RxFlowControl, "off"), "tx-flow-control": orDefault(port.TxFlowControl, "off"),
			"auto-negotiation": "true", "running": rosBool(port.Running),
			"tx-drop-packet": "0", "tx-drop-byte": "0", "rx-overflow": "0", "rx-fcs-error": "0",
			"rx-pause": "0", "tx-pause": "0", "driver-rx-packet": strconv.Itoa(1_000 * (i + 1)),
			"driver-tx-packet": strconv.Itoa(2_000 * (i + 1)), "rx-broadcast": "0", "rx-multicast": "0",
		}
		if port.Switch != "" {
			row["switch"] = port.Switch
		}
		rows = append(rows, row)
	}
	return rows
}

// bridgeRows is the /interface/bridge/print table.
func (p planPersonality) bridgeRows() []map[string]string {
	var rows []map[string]string
	for i, port := range p.Ports {
		if port.Type != "bridge" {
			continue
		}
		rows = append(rows, map[string]string{
			".id": fmt.Sprintf("*%X", i+1), "name": port.Name, "protocol-mode": "rstp", "vlan-filtering": "true",
			"disabled": "false", "running": rosBool(port.Running), "mac-address": p.mac(i), "priority": "0x8000",
		})
	}
	return rows
}

// bridgeID is a bridge's STP id as RouterOS prints it.
func (p planPersonality) bridgeID(bridge string) string {
	return "0x8000." + p.mac(max(p.portIndex(bridge), 0))
}

// portRuntime is the STP runtime state of one bridge port, in the vocabulary
// of RouterOS 7.19 and later: the port print carries it from 7.24, the port
// monitor before.
func (p planPersonality) portRuntime(port int) map[string]string {
	pt := p.Ports[port]
	role := "designated-port"
	if pt.Trunk {
		role = "root-port"
	}
	return map[string]string{
		"interface": pt.Name, "status": "in-bridge", "role": role, "forwarding": "true", "learning": "true",
		"edge-port": rosBool(!pt.Trunk), "point-to-point-port": "true",
		"designated-bridge-id": p.bridgeID(pt.Bridge), "topology-changes": "0",
	}
}

// bridgePortRows is the /interface/bridge/port/print table.
func (p planPersonality) bridgePortRows() []map[string]string {
	var rows []map[string]string
	for i, port := range p.Ports {
		if port.Bridge == "" {
			continue
		}
		inactive := !port.Running || port.BridgeInactive
		row := map[string]string{
			".id": fmt.Sprintf("*%X", i+1), "bridge": port.Bridge, "interface": port.Name,
			"disabled": rosBool(port.BridgeDisabled), "inactive": rosBool(inactive), "dynamic": "false",
			"edge": "auto", "point-to-point": "auto", "horizon": orDefault(port.Horizon, "none"),
			"pvid": "1", "path-cost": "20000", "priority": "0x80",
		}
		if port.HWKnown {
			row["hw"] = rosBool(port.HW)
		}
		if port.HWOffloadKnown {
			row["hw-offload"] = rosBool(port.HWOffload && !inactive)
		}
		if port.Switch != "" {
			row["hw-offload-group"] = port.Switch
		}
		if p.stpInPrint() {
			if inactive {
				row["status"] = "inactive"
			} else {
				for k, v := range p.portRuntime(i) {
					if k != "interface" {
						row[k] = v
					}
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// bridgePortMonitorRows is what a batched /interface/bridge/port/monitor of
// the active ports answers (devices older than 7.24 only).
func (p planPersonality) bridgePortMonitorRows() []map[string]string {
	var rows []map[string]string
	for i, port := range p.Ports {
		if port.Bridge == "" || port.BridgeDisabled || port.BridgeInactive || !port.Running {
			continue
		}
		row := p.portRuntime(i)
		delete(row, "topology-changes") // the print and the monitor gained it together
		rows = append(rows, row)
	}
	return rows
}

// bridgeMonitorRows is what a batched /interface/bridge/monitor answers: one
// sentence per bridge, in print order.
func (p planPersonality) bridgeMonitorRows() []map[string]string {
	var rows []map[string]string
	for _, port := range p.Ports {
		if port.Type != "bridge" {
			continue
		}
		members, root := 0, "none"
		for _, m := range p.Ports {
			if m.Bridge != port.Name {
				continue
			}
			members++
			if m.Trunk && m.Running && root == "none" {
				root = m.Name
			}
		}
		rows = append(rows, map[string]string{
			"state": "enabled", "bridge-id": p.bridgeID(port.Name), "root-bridge": rosBool(root == "none"),
			"root-bridge-id": "0x7000.02:00:5E:00:53:01", "root-port": root, "root-path-cost": "20000",
			"port-count": strconv.Itoa(members), "designated-port-count": strconv.Itoa(members),
			"last-topology-change": "2026-01-04 08:00:12",
		})
	}
	return rows
}

// fdbRows is the non-local bridge FDB: one learned address behind every
// running member port, a dozen behind a trunk.
func (p planPersonality) fdbRows() []map[string]string {
	var rows []map[string]string
	sum := p.nameHash()
	for i, port := range p.Ports {
		if port.Bridge == "" || !port.Running {
			continue
		}
		hosts := 1
		if port.Trunk {
			hosts = 12
		}
		for h := 0; h < hosts; h++ {
			rows = append(rows, map[string]string{
				".id":          fmt.Sprintf("*%X", len(rows)+1),
				"mac-address":  fmt.Sprintf("06:%02X:%02X:%02X:%02X:%02X", byte(sum>>8), byte(sum), byte(i>>8), byte(i), byte(h)),
				"on-interface": port.Name, "interface": port.Name, "bridge": port.Bridge, "vid": "1",
				"local": "false", "external": "false", "dynamic": "true", "invalid": "false", "disabled": "false",
			})
		}
	}
	return rows
}
