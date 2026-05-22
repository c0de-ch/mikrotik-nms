package routeros

import (
	"strconv"
	"strings"

	ros "github.com/go-routeros/routeros"
)

// BridgeInfo describes one bridge interface and its STP runtime state.
type BridgeInfo struct {
	ID                 string // RouterOS internal .id, used for monitor calls
	Name               string
	ProtocolMode       string // "none", "stp", "rstp", "mstp"
	BridgeID           string // local bridge id (priority.MAC)
	RootBridgeID       string // root bridge id observed via STP
	RootPathCost       int
	RootPort           string
	TopologyChanges    int
	LastTopologyChange string
}

// STPEnabled returns true when the bridge runs an STP variant.
func (b *BridgeInfo) STPEnabled() bool {
	switch b.ProtocolMode {
	case "stp", "rstp", "mstp":
		return true
	}
	return false
}

// BridgePortInfo describes one bridge port and its runtime STP state.
type BridgePortInfo struct {
	ID               string
	Bridge           string
	Interface        string
	Role             string // "root", "designated", "alternate", "backup", "disabled"
	Status           string // "forwarding", "discarding", "learning", "blocking"
	Edge             bool
	PointToPoint     bool
	PathCost         int
	DesignatedBridge string
}

// GetBridges returns all bridges on the device with STP runtime state.
//
// On routers without bridges this returns an empty slice. Monitor calls are
// best-effort: if STP is disabled the device may return no monitor sentence,
// in which case only the static fields (Name, ProtocolMode) are populated.
func GetBridges(client *ros.Client) ([]BridgeInfo, error) {
	reply, err := RunCommand(client, "/interface/bridge/print")
	if err != nil {
		return nil, err
	}

	bridges := make([]BridgeInfo, 0, len(reply.Re))
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		b := BridgeInfo{
			ID:           m[".id"],
			Name:         m["name"],
			ProtocolMode: strings.ToLower(m["protocol-mode"]),
		}
		// monitor enriches with runtime STP state. Use the bridge name as the
		// numbers reference — RouterOS accepts both .id and name.
		if b.Name != "" {
			enrichBridgeFromMonitor(client, &b)
		}
		bridges = append(bridges, b)
	}
	return bridges, nil
}

func enrichBridgeFromMonitor(client *ros.Client, b *BridgeInfo) {
	reply, err := RunCommand(client, "/interface/bridge/monitor",
		"=numbers="+b.Name,
		"=once=",
	)
	if err != nil || len(reply.Re) == 0 {
		return
	}
	m := GetSentenceMap(reply.Re[0])
	b.BridgeID = m["bridge-id"]
	b.RootBridgeID = m["root-bridge-id"]
	b.RootPort = m["root-port"]
	b.LastTopologyChange = m["last-topology-change"]
	if v, err := strconv.Atoi(m["root-path-cost"]); err == nil {
		b.RootPathCost = v
	}
	if v, err := strconv.Atoi(m["topology-changes"]); err == nil {
		b.TopologyChanges = v
	}
	// Some firmware versions only expose protocol-mode via monitor, not print.
	if b.ProtocolMode == "" {
		b.ProtocolMode = strings.ToLower(m["protocol-mode"])
	}
}

// GetBridgePorts returns all bridge ports with their STP role/status.
//
// Iterates /interface/bridge/port/print and follows up with a per-port
// monitor call to fetch role/status. If STP is disabled on the bridge, the
// monitor call may return empty fields — those ports get blank Role/Status.
func GetBridgePorts(client *ros.Client) ([]BridgePortInfo, error) {
	reply, err := RunCommand(client, "/interface/bridge/port/print")
	if err != nil {
		return nil, err
	}

	ports := make([]BridgePortInfo, 0, len(reply.Re))
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		bridge := m["bridge"]
		iface := m["interface"]
		// Some firmware versions return phantom rows from
		// /interface/bridge/port/print with an empty bridge or interface
		// field — orphans from removed bridges, dynamic entries that
		// haven't fully materialised, or interfaces that aren't actual
		// bridge ports. They show up grouped under an empty bridge name
		// and falsely fire the stp_disabled detector. Drop them.
		if bridge == "" || iface == "" || strings.Contains(iface, ",") {
			continue
		}
		p := BridgePortInfo{
			ID:           m[".id"],
			Bridge:       bridge,
			Interface:    iface,
			Edge:         m["edge"] == "yes" || m["edge"] == "true",
			PointToPoint: m["point-to-point"] == "yes" || m["point-to-point"] == "true",
		}
		if v, err := strconv.Atoi(m["path-cost"]); err == nil {
			p.PathCost = v
		}
		if p.ID != "" {
			enrichBridgePortFromMonitor(client, &p)
		}
		ports = append(ports, p)
	}
	return ports, nil
}

func enrichBridgePortFromMonitor(client *ros.Client, p *BridgePortInfo) {
	reply, err := RunCommand(client, "/interface/bridge/port/monitor",
		"=numbers="+p.ID,
		"=once=",
	)
	if err != nil || len(reply.Re) == 0 {
		return
	}
	m := GetSentenceMap(reply.Re[0])
	p.Role = strings.ToLower(m["role"])
	p.Status = strings.ToLower(m["status"])
	p.DesignatedBridge = m["designated-bridge"]
	// Some ROS versions report port state via "port-state" instead of "status".
	if p.Status == "" {
		p.Status = strings.ToLower(m["port-state"])
	}
}
