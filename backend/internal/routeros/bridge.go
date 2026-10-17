package routeros

import (
	"strconv"
	"strings"
	"time"
)

// Bridge and STP state readers.
//
// The cheapest read on every RouterOS version is one proplisted
// /interface/bridge/port/print: from 7.24 the STP runtime fields (status,
// role, forwarding, learning, edge-port, point-to-point-port,
// designated-bridge-id, topology-changes) are read-only properties of the
// port itself, so a 55-port switch costs one command instead of 55 per-port
// monitor calls — which was a ~5 s burst at 100 % CPU every cycle on a
// single-core switch. Where the print lacks the runtime fields (7.20–7.23)
// the reader falls back to one batched /interface/bridge/port/monitor over
// the active ports, and only as a last resort to one monitor per active
// port. The bridge summary comes from one batched /interface/bridge/monitor.
// Which forms a device accepts is learned once (BridgeCaps) and kept by the
// poller, so the probing cost is paid per device per bridgeCapsTTL, not per
// cycle.

// bridgeCapsTTL is how long a device's learned capabilities are trusted before
// the reader re-probes: a monitor form that failed under load deserves a
// retry, and a RouterOS upgrade is re-probed at once via the version string.
const bridgeCapsTTL = 24 * time.Hour

// Proplists keep every read at the fields the detectors use. The port print
// is the hot one (one row per port, 55+ on a core switch); unknown names in a
// proplist are ignored by RouterOS, so the runtime fields can be requested on
// versions that do not have them — and the monitor proplists also name the
// pre-7.19 keys the parsers fall back to (port-state, designated-bridge) and
// the bridge monitor's protocol-mode, which some firmware only reports
// there: a proplisted batched monitor returns nothing else.
const (
	bridgeProplist     = ".id,name,protocol-mode,vlan-filtering,disabled,running,comment"
	bridgePortProplist = ".id,bridge,interface,disabled,inactive,dynamic,hw,hw-offload,hw-offload-group," +
		"edge,point-to-point,horizon,pvid,path-cost," +
		"status,role,forwarding,learning,edge-port,point-to-point-port,designated-bridge-id,topology-changes"
	bridgePortMonitorProplist = "interface,status,port-state,role,edge-port,point-to-point-port,learning,forwarding," +
		"designated-bridge-id,designated-bridge,topology-changes"
	bridgeMonitorProplist = "bridge-id,root-bridge,root-bridge-id,root-port,root-path-cost,port-count," +
		"last-topology-change,topology-changes,protocol-mode"
)

// How a device's port STP state was obtained (BridgeState.Path).
const (
	BridgePathPrint          = "print"            // runtime fields in the port print (RouterOS >= 7.24)
	BridgePathMonitorBatched = "monitor-batched"  // one monitor over all active ports
	BridgePathMonitorPerPort = "monitor-per-port" // one monitor per active port (last resort)
	BridgePathNone           = "none"             // no runtime state: no active ports, or every form failed
)

// Monitor forms a device was observed to accept (BridgeCaps.PortMonitor and
// BridgeMonitor), learned once and kept for bridgeCapsTTL.
const (
	monitorBatched      = "batched"       // =numbers=a,b,c =once= with a .proplist
	monitorBatchedPlain = "batched-plain" // the same without .proplist
	monitorPerItem      = "per-item"      // one call per number
)

// BridgeInfo describes one bridge interface and its STP runtime state.
type BridgeInfo struct {
	ID            string // RouterOS internal .id
	Name          string
	ProtocolMode  string // "none", "stp", "rstp", "mstp"
	VLANFiltering bool
	Disabled      bool
	Running       bool
	Comment       string

	// Runtime, from /interface/bridge/monitor.
	BridgeID     string // local bridge id (priority.MAC)
	IsRoot       bool   // root-bridge=yes: this bridge is the STP root
	RootBridgeID string
	RootPathCost int
	RootPort     string
	PortCount    int // port-count as the monitor reports it (not the configured port list)

	// TopologyChanges is the sum of the member ports' topology-changes
	// counters: the bridge monitor has no such field. Zero when the device's
	// RouterOS reports no per-port counter.
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

// BridgePortInfo describes one bridge port: its configuration and, when the
// device reports it, its STP runtime state.
type BridgePortInfo struct {
	ID        string
	Bridge    string
	Interface string

	// Configuration.
	Disabled          bool
	Dynamic           bool
	HW                bool   // hw=yes: hardware offload requested
	HWOffload         bool   // hw-offload=true: the switch chip forwards this port
	HWOffloadReported bool   // the print carried hw-offload at all
	HWOffloadGroup    string // switch chip group, e.g. "switch1"
	EdgeConfig        string // edge=auto|yes|no|yes-discover|no-discover
	P2PConfig         string // point-to-point=auto|yes|no
	Horizon           string
	PVID              int
	PathCost          int

	// Runtime STP state. HasRuntime is false when the read could not obtain
	// it (STP off, or a RouterOS that exposes it neither in the print nor via
	// a monitor the device accepted); Role and Status are then "".
	HasRuntime         bool
	Inactive           bool   // inactive=true or status=inactive: the port is not in the bridge right now
	PortState          string // raw status: "in-bridge" or "inactive" (older monitors: the STP state itself)
	Role               string // root, designated, alternate, backup, disabled
	Status             string // forwarding, learning, discarding, inactive, disabled
	Forwarding         bool
	Learning           bool
	Edge               bool   // runtime edge-port when reported, else the configured edge=yes
	PointToPoint       bool   // runtime point-to-point-port when reported, else the configuration
	DesignatedBridge   string // designated-bridge-id
	TopologyChanges    int
	HasTopologyChanges bool // the device reported a per-port topology-changes counter
}

// BridgeCaps remembers what a device's RouterOS exposes, so the reader does
// not re-learn the cheapest path every cycle. The zero value means "not
// probed"; the poller keeps one per device, and the reader re-probes after
// bridgeCapsTTL or a version change.
type BridgeCaps struct {
	ProbedAt time.Time
	Version  string

	// StpInPrint: the port print carried role, status and topology-changes
	// (RouterOS >= 7.24) — no port monitor is needed at all. RoleInPrint is
	// the weaker form (STP state without the counter); TCNInPrint the counter.
	StpInPrint  bool
	RoleInPrint bool
	TCNInPrint  bool

	// The monitor forms the device accepted on the fallback paths (the
	// monitor* constants). Empty = not tried yet.
	PortMonitor   string
	BridgeMonitor string
}

// BridgeState is one device's bridge and port view.
type BridgeState struct {
	Bridges  []BridgeInfo
	Ports    []BridgePortInfo
	Caps     BridgeCaps
	Path     string // a BridgePath* value: how the port STP state was obtained
	Commands int    // RouterOS commands this read cost (the bridge print of ListBridges excluded)
}

// ListBridges returns the device's bridges from one proplisted print, without
// runtime state. Callers filter the list (RouterOS 7.23 lists non-bridge
// interfaces here) before passing it to ReadBridgeState, which then reads and
// monitors only genuine bridges.
func ListBridges(run Runner) ([]BridgeInfo, error) {
	return listBridges(run)
}

// ReadBridgeState reads the ports of the given bridges and their STP runtime
// state in as few commands as the device allows (see the file comment), plus
// each bridge's monitor summary. caps is the device's previous capability
// record (zero = unprobed) and version its RouterOS version; the returned
// state carries the updated caps for the caller to keep.
func ReadBridgeState(run Runner, bridges []BridgeInfo, caps BridgeCaps, version string, now time.Time) (*BridgeState, error) {
	return readBridgeState(run, bridges, caps, version, now)
}

func listBridges(r Runner) ([]BridgeInfo, error) {
	reply, err := r.Run("/interface/bridge/print", "=.proplist="+bridgeProplist)
	if err != nil {
		return nil, err
	}
	bridges := make([]BridgeInfo, 0, len(reply.Re))
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		if m["name"] == "" {
			continue
		}
		bridges = append(bridges, BridgeInfo{
			ID:            m[".id"],
			Name:          m["name"],
			ProtocolMode:  strings.ToLower(m["protocol-mode"]),
			VLANFiltering: rosBool(m["vlan-filtering"]),
			Disabled:      rosBool(m["disabled"]),
			Running:       rosBool(m["running"]),
			Comment:       m["comment"],
		})
	}
	return bridges, nil
}

func readBridgeState(r Runner, bridges []BridgeInfo, caps BridgeCaps, version string, now time.Time) (*BridgeState, error) {
	st := &BridgeState{Caps: caps, Path: BridgePathNone}
	if len(bridges) == 0 {
		return st, nil
	}
	// Re-probe after a version change or bridgeCapsTTL: an upgrade can add the
	// print fields, and a monitor form that was rejected may work now.
	if caps.Version != version || now.Sub(caps.ProbedAt) > bridgeCapsTTL {
		st.Caps = BridgeCaps{Version: version}
	}

	keep := make(map[string]bool, len(bridges))
	for _, b := range bridges {
		keep[b.Name] = true
	}
	ports, err := printBridgePorts(r, keep)
	st.Commands++
	if err != nil {
		return nil, err
	}

	// The first print is also the probe. It is only conclusive with an active
	// port in the reply: RouterOS reports no runtime state for inactive ones.
	// ProbedAt dates the caps record, not the latest read: the monitor forms
	// it learned are re-tried bridgeCapsTTL after it was started.
	probe := probeSTPInPrint(ports)
	if probe.conclusive {
		if st.Caps.ProbedAt.IsZero() {
			st.Caps.ProbedAt = now
		}
		st.Caps.RoleInPrint = probe.roleStatus
		st.Caps.TCNInPrint = probe.tcn
		st.Caps.StpInPrint = probe.roleStatus && probe.tcn
	}
	switch {
	case probe.roleStatus:
		st.Path = BridgePathPrint
	case probe.conclusive:
		if st.Path, err = monitorPorts(r, ports, &st.Caps, &st.Commands); err != nil {
			return nil, err
		}
	}

	if st.Bridges, err = monitorBridges(r, bridges, &st.Caps, &st.Commands); err != nil {
		return nil, err
	}

	// TCN per bridge is the sum of its ports' counters: the bridge monitor
	// has no topology-changes field, so this is the only source.
	byName := make(map[string]int, len(st.Bridges))
	for i := range st.Bridges {
		byName[st.Bridges[i].Name] = i
	}
	for _, p := range ports {
		if i, ok := byName[p.Bridge]; ok && p.HasTopologyChanges {
			st.Bridges[i].TopologyChanges += p.TopologyChanges
		}
	}
	st.Ports = ports
	return st, nil
}

// printBridgePorts is the one proplisted port print, restricted to the ports
// of the bridges the caller kept.
func printBridgePorts(r Runner, keep map[string]bool) ([]BridgePortInfo, error) {
	reply, err := r.Run("/interface/bridge/port/print", "=.proplist="+bridgePortProplist)
	if err != nil {
		return nil, err
	}
	ports := make([]BridgePortInfo, 0, len(reply.Re))
	for _, re := range reply.Re {
		p, ok := parseBridgePortRow(GetSentenceMap(re))
		if !ok || !keep[p.Bridge] {
			continue
		}
		ports = append(ports, p)
	}
	return ports, nil
}

// parseBridgePortRow turns one /interface/bridge/port/print row into a port.
// ok is false for the phantom rows some firmware versions return with an
// empty bridge or interface (orphans of removed bridges, dynamic entries
// that have not materialised): grouped under an empty bridge name they
// falsely fire the stp_disabled detector.
func parseBridgePortRow(m map[string]string) (BridgePortInfo, bool) {
	bridge, iface := m["bridge"], m["interface"]
	if bridge == "" || iface == "" || strings.Contains(iface, ",") {
		return BridgePortInfo{}, false
	}
	p := BridgePortInfo{
		ID:             m[".id"],
		Bridge:         bridge,
		Interface:      iface,
		Disabled:       rosBool(m["disabled"]),
		Dynamic:        rosBool(m["dynamic"]),
		Inactive:       rosBool(m["inactive"]),
		HW:             rosBool(m["hw"]),
		HWOffloadGroup: m["hw-offload-group"],
		EdgeConfig:     m["edge"],
		P2PConfig:      m["point-to-point"],
		Horizon:        m["horizon"],
		PVID:           atoi(m["pvid"]),
		PathCost:       atoi(m["path-cost"]),
		// Configured values stand in until a runtime field says otherwise:
		// edge=auto stays "not edge", exactly as before.
		Edge:         m["edge"] == "yes" || m["edge"] == "yes-discover",
		PointToPoint: m["point-to-point"] == "yes",
	}
	if v, ok := m["hw-offload"]; ok {
		p.HWOffload, p.HWOffloadReported = rosBool(v), true
	}
	applyPortRuntime(&p, m)
	switch {
	case p.Status != "":
	case p.Disabled:
		p.Status = "disabled"
	case p.Inactive:
		p.Status = "inactive"
	}
	return p, true
}

// applyPortRuntime fills the STP runtime fields from a print row (>= 7.24) or
// a monitor sentence; both use the same keys. A sentence without role and
// status carries no runtime state and leaves the port untouched.
func applyPortRuntime(p *BridgePortInfo, m map[string]string) {
	role, status := strings.ToLower(m["role"]), strings.ToLower(m["status"])
	if status == "" {
		status = strings.ToLower(m["port-state"]) // pre-7.19 monitors
	}
	if role == "" && status == "" {
		return
	}
	p.HasRuntime = true
	p.PortState = status
	if status == "inactive" {
		p.Inactive = true
	}
	p.Role = strings.TrimSuffix(role, "-port") // root-port → root (7.19+ vocabulary)
	p.Forwarding = rosBool(m["forwarding"])
	p.Learning = rosBool(m["learning"])
	_, hasForwarding := m["forwarding"]
	p.Status = deriveSTPStatus(status, p.Role, p.Forwarding, p.Learning, hasForwarding)
	if v, ok := m["edge-port"]; ok {
		p.Edge = rosBool(v)
	}
	if v, ok := m["point-to-point-port"]; ok {
		p.PointToPoint = rosBool(v)
	}
	// designated-bridge was renamed designated-bridge-id in 7.19.
	if v := m["designated-bridge-id"]; v != "" {
		p.DesignatedBridge = v
	} else if v := m["designated-bridge"]; v != "" {
		p.DesignatedBridge = v
	}
	if v, ok := m["topology-changes"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			p.TopologyChanges, p.HasTopologyChanges = n, true
		}
	}
}

// deriveSTPStatus maps RouterOS's port fields onto the forwarding states the
// UI has always shown. From 7.19 `status` only says in-bridge or inactive and
// the STP state is in the forwarding and learning booleans; older monitors
// put forwarding/discarding/learning in status (or port-state) directly.
func deriveSTPStatus(portState, role string, forwarding, learning, hasForwarding bool) string {
	switch portState {
	case "forwarding", "discarding", "learning", "blocking", "listening", "disabled", "inactive":
		return portState
	}
	if role == "disabled" {
		return "disabled"
	}
	switch {
	case forwarding:
		return "forwarding"
	case learning:
		return "learning"
	case hasForwarding:
		return "discarding"
	}
	return ""
}

// stpProbe is what one port print revealed about the device's RouterOS.
type stpProbe struct {
	conclusive bool // at least one active port was in the reply
	roleStatus bool // role and status came back: STP state needs no monitor
	tcn        bool // a per-port topology-changes counter came back
}

func probeSTPInPrint(ports []BridgePortInfo) stpProbe {
	var pr stpProbe
	for i := range ports {
		p := &ports[i]
		if p.Disabled || p.Inactive {
			continue
		}
		pr.conclusive = true
		if p.HasRuntime && p.Role != "" && p.PortState != "" {
			pr.roleStatus = true
		}
		if p.HasTopologyChanges {
			pr.tcn = true
		}
	}
	return pr
}

// monitorOutcome is the verdict on one batched monitor attempt.
type monitorOutcome int

const (
	monitorOK       monitorOutcome = iota
	monitorRejected                // the device trapped: this form is not supported
	monitorEmpty                   // no usable sentence this time; not a verdict on the form
	monitorFailed                  // the device did not answer (transport error, timeout): no verdict at all
)

// batchedOutcome classifies a batched monitor's error: only a trap is a
// verdict on the form. A timeout or a dead session says nothing about what
// the device accepts — learning "per item" from it would turn one bad
// moment into a monitor per port every cycle for a day.
func batchedOutcome(err error) monitorOutcome {
	if isTrap(err) {
		return monitorRejected
	}
	return monitorFailed
}

// monitorPorts fills the runtime state of the active ports on a device whose
// print lacks it, learning the cheapest monitor form the device accepts:
// batched with a proplist, batched without, else one call per port. Returns
// the BridgePath* taken, or the error of a monitor the device did not answer
// (the read is abandoned: half-read ports must not replace the stored ones,
// and nothing is learned).
func monitorPorts(r Runner, ports []BridgePortInfo, caps *BridgeCaps, commands *int) (string, error) {
	var active []int
	for i := range ports {
		if p := &ports[i]; !p.Disabled && !p.Inactive && p.ID != "" {
			active = append(active, i)
		}
	}
	if len(active) == 0 {
		return BridgePathNone, nil
	}
	if caps.PortMonitor != monitorPerItem {
		forms := []string{monitorBatched, monitorBatchedPlain}
		if caps.PortMonitor == monitorBatchedPlain {
			forms = forms[1:]
		}
		rejected := true
		for _, form := range forms {
			*commands++
			out, err := monitorPortsBatched(r, ports, active, form == monitorBatched)
			switch out {
			case monitorOK:
				caps.PortMonitor = form
				return BridgePathMonitorBatched, nil
			case monitorFailed:
				return "", err
			case monitorEmpty:
				rejected = false
			}
			if !rejected {
				break
			}
		}
		if rejected {
			caps.PortMonitor = monitorPerItem
		}
	}
	got := false
	for _, i := range active {
		*commands++
		reply, err := r.Run("/interface/bridge/port/monitor", "=numbers="+ports[i].ID, "=once=")
		if err != nil && !isTrap(err) {
			return "", err
		}
		if err != nil || reply == nil || len(reply.Re) == 0 {
			continue
		}
		applyPortRuntime(&ports[i], GetSentenceMap(reply.Re[0]))
		got = got || ports[i].HasRuntime
	}
	if !got {
		return BridgePathNone, nil
	}
	return BridgePathMonitorPerPort, nil
}

// monitorPortsBatched runs one monitor over the active ports. Replies are
// matched to ports by interface name; a reply that names no interface is
// accepted only when it is complete and in request order. The error is the
// device's, for monitorFailed.
func monitorPortsBatched(r Runner, ports []BridgePortInfo, active []int, proplist bool) (monitorOutcome, error) {
	ids := make([]string, len(active))
	for j, i := range active {
		ids[j] = ports[i].ID
	}
	args := []string{"/interface/bridge/port/monitor", "=numbers=" + strings.Join(ids, ","), "=once="}
	if proplist {
		args = append(args, "=.proplist="+bridgePortMonitorProplist)
	}
	reply, err := r.Run(args[0], args[1:]...)
	if err != nil {
		return batchedOutcome(err), err
	}
	if reply == nil || len(reply.Re) == 0 {
		return monitorEmpty, nil
	}
	byIface := make(map[string]int, len(active))
	for _, i := range active {
		byIface[ports[i].Interface] = i
	}
	named := 0
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		if i, ok := byIface[m["interface"]]; ok && m["interface"] != "" {
			applyPortRuntime(&ports[i], m)
			named++
		}
	}
	if named > 0 {
		return monitorOK, nil
	}
	if len(reply.Re) != len(active) {
		return monitorEmpty, nil
	}
	for j, re := range reply.Re {
		applyPortRuntime(&ports[active[j]], GetSentenceMap(re))
	}
	return monitorOK, nil
}

// monitorBridges adds each bridge's monitor summary, batched when the device
// accepts it. The bridge monitor names no bridge in its reply, so a batched
// reply is only trusted when it is complete and in request order. A monitor
// the device did not answer abandons the read with its error, as in
// monitorPorts.
func monitorBridges(r Runner, bridges []BridgeInfo, caps *BridgeCaps, commands *int) ([]BridgeInfo, error) {
	out := make([]BridgeInfo, len(bridges))
	copy(out, bridges)
	if caps.BridgeMonitor != monitorPerItem {
		forms := []string{monitorBatched, monitorBatchedPlain}
		if caps.BridgeMonitor == monitorBatchedPlain {
			forms = forms[1:]
		}
		rejected := true
		for _, form := range forms {
			*commands++
			res, err := monitorBridgesBatched(r, out, form == monitorBatched)
			switch res {
			case monitorOK:
				caps.BridgeMonitor = form
				return out, nil
			case monitorFailed:
				return nil, err
			case monitorEmpty:
				rejected = false
			}
			if !rejected {
				break
			}
		}
		if rejected {
			caps.BridgeMonitor = monitorPerItem
		}
	}
	for i := range out {
		*commands++
		reply, err := r.Run("/interface/bridge/monitor", "=numbers="+bridgeNumber(out[i]), "=once=")
		if err != nil && !isTrap(err) {
			return nil, err
		}
		if err != nil || reply == nil || len(reply.Re) == 0 {
			continue
		}
		applyBridgeMonitor(&out[i], GetSentenceMap(reply.Re[0]))
	}
	return out, nil
}

func monitorBridgesBatched(r Runner, bridges []BridgeInfo, proplist bool) (monitorOutcome, error) {
	nums := make([]string, len(bridges))
	for i := range bridges {
		nums[i] = bridgeNumber(bridges[i])
	}
	args := []string{"/interface/bridge/monitor", "=numbers=" + strings.Join(nums, ","), "=once="}
	if proplist {
		args = append(args, "=.proplist="+bridgeMonitorProplist)
	}
	reply, err := r.Run(args[0], args[1:]...)
	if err != nil {
		return batchedOutcome(err), err
	}
	if reply == nil || len(reply.Re) != len(bridges) {
		return monitorEmpty, nil
	}
	for i, re := range reply.Re {
		applyBridgeMonitor(&bridges[i], GetSentenceMap(re))
	}
	return monitorOK, nil
}

// bridgeNumber is the =numbers= reference for a bridge: its .id, or the name
// when the print carried none (RouterOS accepts both).
func bridgeNumber(b BridgeInfo) string {
	if b.ID != "" {
		return b.ID
	}
	return b.Name
}

func applyBridgeMonitor(b *BridgeInfo, m map[string]string) {
	b.BridgeID = m["bridge-id"]
	b.IsRoot = rosBool(m["root-bridge"])
	b.RootBridgeID = m["root-bridge-id"]
	b.RootPort = m["root-port"]
	b.RootPathCost = atoi(m["root-path-cost"])
	b.PortCount = atoi(m["port-count"])
	if v := m["last-topology-change"]; v != "" {
		b.LastTopologyChange = v
	}
	// Some firmware versions only expose protocol-mode via monitor, not print.
	if b.ProtocolMode == "" {
		b.ProtocolMode = strings.ToLower(m["protocol-mode"])
	}
}

// BridgeVLAN describes one /interface/bridge/vlan row: which VLAN IDs are
// configured on a bridge and which ports they're tagged / untagged on.
type BridgeVLAN struct {
	Bridge          string
	VLANIDs         string // configured vlan-ids, e.g. "28" or "28,78"
	Tagged          string // statically configured tagged ports (comma-separated)
	Untagged        string // statically configured untagged ports
	CurrentTagged   string // runtime tagged ports (incl. dynamic)
	CurrentUntagged string // runtime untagged ports
	Comment         string
}

// GetBridgeVLANs returns the bridge VLAN table on the device.
//
// On devices without bridge VLAN filtering this returns an empty slice. Rows
// with an empty vlan-ids field are skipped — they carry no useful VLAN info.
func GetBridgeVLANs(run Runner) ([]BridgeVLAN, error) {
	reply, err := run.Run("/interface/bridge/vlan/print")
	if err != nil {
		return nil, err
	}

	vlans := make([]BridgeVLAN, 0, len(reply.Re))
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		vlanIDs := m["vlan-ids"]
		if vlanIDs == "" {
			continue
		}
		vlans = append(vlans, BridgeVLAN{
			Bridge:          m["bridge"],
			VLANIDs:         vlanIDs,
			Tagged:          m["tagged"],
			Untagged:        m["untagged"],
			CurrentTagged:   m["current-tagged"],
			CurrentUntagged: m["current-untagged"],
			Comment:         m["comment"],
		})
	}
	return vlans, nil
}

// rosBool reads a RouterOS boolean, which prints as true/false or yes/no
// depending on the property and version.
func rosBool(v string) bool {
	switch strings.ToLower(v) {
	case "true", "yes":
		return true
	}
	return false
}

func atoi(v string) int {
	n, _ := strconv.Atoi(v)
	return n
}
