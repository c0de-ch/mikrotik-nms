package routeros

import (
	"errors"
	"strings"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
)

// scriptedDevice answers Run from a handler and records every call, so a
// test can assert both the parsed result and the exact command sequence.
type scriptedDevice struct {
	calls  []string
	handle func(cmd string, args []string) (*ros.Reply, error)
}

func (d *scriptedDevice) Run(command string, args ...string) (*ros.Reply, error) {
	d.calls = append(d.calls, strings.Join(append([]string{command}, args...), " "))
	return d.handle(command, args)
}

// count returns how many recorded calls start with prefix.
func (d *scriptedDevice) count(prefix string) int {
	n := 0
	for _, c := range d.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func sentences(rows ...map[string]string) *ros.Reply {
	r := &ros.Reply{Done: &proto.Sentence{Word: "!done"}}
	for _, m := range rows {
		r.Re = append(r.Re, &proto.Sentence{Word: "!re", Map: m})
	}
	return r
}

// errTrap is how go-routeros reports a !trap: a *ros.DeviceError — the only
// error the readers take as a verdict on a command form.
var errTrap error = &ros.DeviceError{Sentence: &proto.Sentence{Word: "!trap", Map: map[string]string{"message": "no such command or argument"}}}

// errDead is a transport failure: the session died, nothing was answered.
var errDead = errors.New("write tcp 192.0.2.10:51234->192.0.2.1:8728: use of closed network connection")

func argValue(args []string, prefix string) string {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix)
		}
	}
	return ""
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

var testNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func bridgeMonitorRow() map[string]string {
	return map[string]string{
		"bridge-id": "0x8000.D4:01:C3:00:00:01", "root-bridge": "yes", "root-bridge-id": "0x8000.D4:01:C3:00:00:01",
		"root-port": "none", "root-path-cost": "0", "port-count": "4",
	}
}

// device724 prints ports the way RouterOS 7.24 does: the STP runtime fields
// are read-only properties of the port, so no port monitor is ever needed.
func device724(t *testing.T) *scriptedDevice {
	t.Helper()
	return &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		switch cmd {
		case "/interface/bridge/port/print":
			return sentences(
				map[string]string{".id": "*1", "bridge": "bridge", "interface": "ether1", "disabled": "false", "inactive": "false",
					"dynamic": "false", "hw": "true", "hw-offload": "true", "hw-offload-group": "switch1", "edge": "auto",
					"point-to-point": "auto", "pvid": "1", "path-cost": "10", "status": "in-bridge", "role": "root-port",
					"forwarding": "true", "learning": "true", "edge-port": "false", "point-to-point-port": "true",
					"designated-bridge-id": "0x8000.D4:01:C3:00:00:01", "topology-changes": "3"},
				map[string]string{".id": "*2", "bridge": "bridge", "interface": "ether2", "disabled": "false", "inactive": "false",
					"hw": "true", "hw-offload": "true", "edge": "yes", "pvid": "28", "status": "in-bridge", "role": "designated-port",
					"forwarding": "true", "learning": "true", "edge-port": "true", "point-to-point-port": "true",
					"designated-bridge-id": "0x8000.D4:01:C3:00:00:02", "topology-changes": "5"},
				map[string]string{".id": "*3", "bridge": "bridge", "interface": "ether3", "disabled": "false", "inactive": "true",
					"hw": "true", "hw-offload": "true", "edge": "auto", "status": "inactive", "role": "disabled-port",
					"forwarding": "false", "learning": "false", "edge-port": "false", "point-to-point-port": "false",
					"topology-changes": "0"},
				map[string]string{".id": "*4", "bridge": "bridge", "interface": "ether4", "disabled": "false", "inactive": "false",
					"hw": "true", "hw-offload": "true", "edge": "auto", "status": "in-bridge", "role": "alternate-port",
					"forwarding": "false", "learning": "false", "edge-port": "false", "point-to-point-port": "true",
					"designated-bridge-id": "0x8000.D4:01:C3:00:00:09", "topology-changes": "1"},
				// A phantom row (empty bridge and interface) and a port of a
				// bridge the caller filtered out: both must be dropped.
				map[string]string{".id": "*9", "bridge": "", "interface": ""},
				map[string]string{".id": "*10", "bridge": "wifi-guest", "interface": "wifi2", "disabled": "false",
					"status": "in-bridge", "role": "designated-port", "forwarding": "true", "topology-changes": "7"},
			), nil
		case "/interface/bridge/monitor":
			if !hasArg(args, "=once=") {
				t.Fatalf("bridge monitor without =once=: %v", args)
			}
			var rows []map[string]string
			for range strings.Split(argValue(args, "=numbers="), ",") {
				rows = append(rows, bridgeMonitorRow())
			}
			return sentences(rows...), nil
		case "/interface/bridge/port/monitor":
			t.Fatalf("a 7.24 device must never be port-monitored: %v", args)
		}
		return nil, errTrap
	}}
}

func portByName(t *testing.T, ports []BridgePortInfo, name string) BridgePortInfo {
	t.Helper()
	for _, p := range ports {
		if p.Interface == name {
			return p
		}
	}
	t.Fatalf("port %s missing from %+v", name, ports)
	return BridgePortInfo{}
}

func TestReadBridgeState724OnePrintNoMonitors(t *testing.T) {
	dev := device724(t)
	bridges := []BridgeInfo{{ID: "*1", Name: "bridge", ProtocolMode: "rstp"}}
	st, err := readBridgeState(dev, bridges, BridgeCaps{}, "7.24.5", testNow)
	if err != nil {
		t.Fatal(err)
	}
	// One port print plus one batched bridge monitor: 55 ports would cost
	// the same two commands.
	if st.Path != BridgePathPrint || st.Commands != 2 {
		t.Fatalf("path %q, %d commands; want print, 2 (calls: %v)", st.Path, st.Commands, dev.calls)
	}
	if n := dev.count("/interface/bridge/port/monitor"); n != 0 {
		t.Fatalf("%d port monitors on a 7.24 device", n)
	}
	if !strings.Contains(dev.calls[0], "=.proplist="+bridgePortProplist) {
		t.Fatalf("port print without the proplist: %s", dev.calls[0])
	}
	c := st.Caps
	if !c.StpInPrint || !c.RoleInPrint || !c.TCNInPrint || !c.ProbedAt.Equal(testNow) || c.Version != "7.24.5" {
		t.Fatalf("caps = %+v", c)
	}
	if len(st.Ports) != 4 {
		t.Fatalf("got %d ports, want 4 (phantom and foreign-bridge rows dropped): %+v", len(st.Ports), st.Ports)
	}

	e1 := portByName(t, st.Ports, "ether1")
	if e1.Role != "root" || e1.Status != "forwarding" || e1.PortState != "in-bridge" || !e1.HasRuntime ||
		e1.Edge || !e1.PointToPoint || e1.DesignatedBridge != "0x8000.D4:01:C3:00:00:01" ||
		e1.TopologyChanges != 3 || !e1.HasTopologyChanges || !e1.HWOffload || !e1.HWOffloadReported ||
		e1.HWOffloadGroup != "switch1" || e1.PVID != 1 || e1.PathCost != 10 || e1.EdgeConfig != "auto" {
		t.Fatalf("ether1 = %+v", e1)
	}
	e2 := portByName(t, st.Ports, "ether2")
	if e2.Role != "designated" || e2.Status != "forwarding" || !e2.Edge || e2.PVID != 28 {
		t.Fatalf("ether2 = %+v", e2)
	}
	e3 := portByName(t, st.Ports, "ether3")
	if e3.Role != "disabled" || e3.Status != "inactive" || !e3.Inactive || e3.Forwarding {
		t.Fatalf("ether3 = %+v", e3)
	}
	e4 := portByName(t, st.Ports, "ether4")
	if e4.Role != "alternate" || e4.Status != "discarding" || e4.Forwarding || e4.Learning {
		t.Fatalf("ether4 = %+v", e4)
	}

	b := st.Bridges[0]
	if b.TopologyChanges != 9 || !b.IsRoot || b.BridgeID != "0x8000.D4:01:C3:00:00:01" || b.PortCount != 4 || b.RootPort != "none" {
		t.Fatalf("bridge = %+v", b)
	}
	if st.Caps.BridgeMonitor != monitorBatched {
		t.Fatalf("bridge monitor form %q, want batched", st.Caps.BridgeMonitor)
	}
}

// device723 prints ports without runtime fields (RouterOS < 7.24) and routes
// the port monitor to portMonitor, which sees the raw arguments.
func device723(portMonitor func(args []string) (*ros.Reply, error)) *scriptedDevice {
	return &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		switch cmd {
		case "/interface/bridge/port/print":
			return sentences(
				map[string]string{".id": "*1", "bridge": "bridge", "interface": "ether1", "disabled": "false", "inactive": "false", "edge": "auto", "path-cost": "10"},
				map[string]string{".id": "*2", "bridge": "bridge", "interface": "ether2", "disabled": "false", "inactive": "false", "edge": "yes"},
				map[string]string{".id": "*3", "bridge": "bridge", "interface": "ether3", "disabled": "true", "inactive": "true"},
				map[string]string{".id": "*4", "bridge": "bridge", "interface": "ether4", "disabled": "false", "inactive": "true"},
			), nil
		case "/interface/bridge/port/monitor":
			return portMonitor(args)
		case "/interface/bridge/monitor":
			return sentences(map[string]string{"bridge-id": "0x8000.AA", "root-bridge": "no", "root-bridge-id": "0x8000.BB",
				"root-port": "ether1", "root-path-cost": "10", "port-count": "2"}), nil
		}
		return nil, errTrap
	}}
}

func isBatchedMonitor(args []string) bool {
	return strings.Contains(argValue(args, "=numbers="), ",")
}

// perPortReply answers today's one-port monitor form, in both the 7.19+
// (in-bridge + forwarding) and the older (status=forwarding) vocabularies.
func perPortReply(args []string) (*ros.Reply, error) {
	switch argValue(args, "=numbers=") {
	case "*1":
		return sentences(map[string]string{"status": "in-bridge", "role": "root-port", "forwarding": "true", "learning": "true",
			"designated-bridge-id": "0x8000.BB"}), nil
	case "*2":
		return sentences(map[string]string{"status": "forwarding", "role": "designated-port", "designated-bridge": "0x8000.AA"}), nil
	}
	return nil, errTrap
}

func TestReadBridgeStatePre724BatchedMonitor(t *testing.T) {
	var batchedArgs []string
	dev := device723(func(args []string) (*ros.Reply, error) {
		if !isBatchedMonitor(args) {
			t.Fatalf("per-port monitor although the batched form works: %v", args)
		}
		batchedArgs = args
		// Reverse order on purpose: replies are matched by interface name.
		return sentences(
			map[string]string{"interface": "ether2", "status": "forwarding", "role": "designated-port", "edge-port": "true"},
			map[string]string{"interface": "ether1", "status": "in-bridge", "role": "root-port", "forwarding": "true", "learning": "true",
				"designated-bridge-id": "0x8000.BB", "topology-changes": "4"},
		), nil
	})
	bridges := []BridgeInfo{{ID: "*1", Name: "bridge", ProtocolMode: "rstp"}}
	st, err := readBridgeState(dev, bridges, BridgeCaps{}, "7.23.1", testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Port print, one batched port monitor, one bridge monitor.
	if st.Path != BridgePathMonitorBatched || st.Commands != 3 {
		t.Fatalf("path %q, %d commands; want monitor-batched, 3 (calls: %v)", st.Path, st.Commands, dev.calls)
	}
	// Only the active ports are monitored, with the runtime proplist.
	if got := argValue(batchedArgs, "=numbers="); got != "*1,*2" {
		t.Fatalf("monitored numbers %q, want *1,*2 (disabled and inactive ports excluded)", got)
	}
	if !hasArg(batchedArgs, "=once=") || !hasArg(batchedArgs, "=.proplist="+bridgePortMonitorProplist) {
		t.Fatalf("batched monitor args = %v", batchedArgs)
	}
	c := st.Caps
	if c.StpInPrint || c.RoleInPrint || c.TCNInPrint || c.PortMonitor != monitorBatched || !c.ProbedAt.Equal(testNow) {
		t.Fatalf("caps = %+v", c)
	}
	e1 := portByName(t, st.Ports, "ether1")
	if e1.Role != "root" || e1.Status != "forwarding" || e1.DesignatedBridge != "0x8000.BB" || e1.TopologyChanges != 4 || !e1.HasTopologyChanges {
		t.Fatalf("ether1 = %+v", e1)
	}
	e2 := portByName(t, st.Ports, "ether2")
	if e2.Role != "designated" || e2.Status != "forwarding" || !e2.Edge {
		t.Fatalf("ether2 = %+v", e2)
	}
	for _, name := range []string{"ether3", "ether4"} {
		if p := portByName(t, st.Ports, name); p.HasRuntime || p.Role != "" {
			t.Fatalf("%s should carry no runtime state: %+v", name, p)
		}
	}
	if st.Bridges[0].TopologyChanges != 4 || st.Bridges[0].RootPort != "ether1" {
		t.Fatalf("bridge = %+v", st.Bridges[0])
	}
}

func TestReadBridgeStateMonitorFallbackCascade(t *testing.T) {
	batched := 0
	dev := device723(func(args []string) (*ros.Reply, error) {
		if isBatchedMonitor(args) {
			batched++
			return nil, errTrap // neither batched form is understood
		}
		return perPortReply(args)
	})
	bridges := []BridgeInfo{{ID: "*1", Name: "bridge", ProtocolMode: "rstp"}}
	st, err := readBridgeState(dev, bridges, BridgeCaps{}, "7.20.8", testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Port print + 2 rejected batched attempts + 2 per-port monitors + bridge monitor.
	if st.Path != BridgePathMonitorPerPort || st.Commands != 6 || batched != 2 {
		t.Fatalf("path %q, %d commands, %d batched attempts; want monitor-per-port, 6, 2 (calls: %v)",
			st.Path, st.Commands, batched, dev.calls)
	}
	if st.Caps.PortMonitor != monitorPerItem {
		t.Fatalf("learned port monitor form %q, want per-item", st.Caps.PortMonitor)
	}
	e1 := portByName(t, st.Ports, "ether1")
	if e1.Role != "root" || e1.Status != "forwarding" || e1.DesignatedBridge != "0x8000.BB" {
		t.Fatalf("ether1 = %+v", e1)
	}
	// designated-bridge (pre-7.19 key) and status=forwarding are still read.
	e2 := portByName(t, st.Ports, "ether2")
	if e2.Role != "designated" || e2.Status != "forwarding" || e2.DesignatedBridge != "0x8000.AA" {
		t.Fatalf("ether2 = %+v", e2)
	}

	// The next cycle goes straight to per-port: no batched attempt is repeated
	// within bridgeCapsTTL on the same version.
	st2, err := readBridgeState(dev, bridges, st.Caps, "7.20.8", testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if st2.Commands != 4 || batched != 2 || st2.Path != BridgePathMonitorPerPort {
		t.Fatalf("second cycle: %d commands, %d batched attempts, path %q", st2.Commands, batched, st2.Path)
	}

	// After the TTL the batched form is tried again.
	st3, err := readBridgeState(dev, bridges, st2.Caps, "7.20.8", testNow.Add(bridgeCapsTTL+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if batched != 4 || st3.Caps.PortMonitor != monitorPerItem {
		t.Fatalf("after TTL: %d batched attempts, form %q", batched, st3.Caps.PortMonitor)
	}
}

// A batched monitor the device did not answer (a timeout, a dead session) is
// no verdict on the form: the read is abandoned with the error — the stored
// rows stay, nothing is learned — and the next read uses the batched form
// again. Only a trap teaches "one monitor per port".
func TestReadBridgeStateTransportFailureLearnsNothing(t *testing.T) {
	broken, batched := false, 0
	dev := device723(func(args []string) (*ros.Reply, error) {
		if broken {
			return nil, errDead
		}
		if isBatchedMonitor(args) {
			batched++
			return sentences(
				map[string]string{"interface": "ether1", "status": "in-bridge", "role": "root-port", "forwarding": "true"},
				map[string]string{"interface": "ether2", "status": "in-bridge", "role": "designated-port", "forwarding": "true"},
			), nil
		}
		return perPortReply(args)
	})
	bridges := []BridgeInfo{{ID: "*1", Name: "bridge", ProtocolMode: "rstp"}}
	st, err := readBridgeState(dev, bridges, BridgeCaps{}, "7.23.1", testNow)
	if err != nil || st.Caps.PortMonitor != monitorBatched {
		t.Fatalf("setup: %v %+v", err, st)
	}
	caps := st.Caps
	broken = true
	if st, err := readBridgeState(dev, bridges, caps, "7.23.1", testNow.Add(2*time.Minute)); err == nil {
		t.Fatalf("a dead session must abandon the read, got %+v", st)
	}
	broken = false
	st, err = readBridgeState(dev, bridges, caps, "7.23.1", testNow.Add(4*time.Minute))
	if err != nil || st.Path != BridgePathMonitorBatched || batched != 2 || dev.count("/interface/bridge/port/monitor =numbers=*1 ") != 0 {
		t.Fatalf("after the failure: path %q err %v batched %d calls %v", st.Path, err, batched, dev.calls)
	}

	// The bridge monitor the same way.
	dead := &scriptedDevice{handle: func(string, []string) (*ros.Reply, error) { return nil, errDead }}
	cmds := 0
	if _, err := monitorBridges(dead, bridges, &caps, &cmds); err == nil || caps.BridgeMonitor == monitorPerItem || cmds != 1 {
		t.Fatalf("bridge monitor on a dead session: err %v, form %q, %d commands", err, caps.BridgeMonitor, cmds)
	}
}

// A form learned as unsupported is re-tried bridgeCapsTTL after the caps
// record was started, however often the device is read in between: the
// record's ProbedAt is not refreshed by every read.
func TestReadBridgeStatePerItemRetriedAfterTTL(t *testing.T) {
	batched := 0
	dev := device723(func(args []string) (*ros.Reply, error) {
		if isBatchedMonitor(args) {
			batched++
			return nil, errTrap
		}
		return perPortReply(args)
	})
	bridges := []BridgeInfo{{ID: "*1", Name: "bridge", ProtocolMode: "rstp"}}
	st, err := readBridgeState(dev, bridges, BridgeCaps{}, "7.20.8", testNow)
	if err != nil || st.Caps.PortMonitor != monitorPerItem || batched != 2 {
		t.Fatalf("setup: %v caps %+v batched %d", err, st.Caps, batched)
	}
	caps := st.Caps
	for ts := testNow.Add(2 * time.Minute); ts.Before(testNow.Add(bridgeCapsTTL + 10*time.Minute)); ts = ts.Add(2 * time.Minute) {
		st, err := readBridgeState(dev, bridges, caps, "7.20.8", ts)
		if err != nil {
			t.Fatal(err)
		}
		caps = st.Caps
	}
	if batched != 4 {
		t.Errorf("batched attempts over a day of 2-minute reads = %d, want the 2 of the first read and 2 after the TTL", batched)
	}
}

// filterProplist answers like RouterOS: with a =.proplist= only the named
// keys come back.
func filterProplist(args []string, rows ...map[string]string) *ros.Reply {
	pl := argValue(args, "=.proplist=")
	if pl == "" {
		return sentences(rows...)
	}
	want := map[string]bool{}
	for _, k := range strings.Split(pl, ",") {
		want[k] = true
	}
	out := make([]map[string]string, 0, len(rows))
	for _, r := range rows {
		m := map[string]string{}
		for k, v := range r {
			if want[k] {
				m[k] = v
			}
		}
		out = append(out, m)
	}
	return sentences(out...)
}

// A pre-7.19 device accepts the proplisted batched monitors (RouterOS ignores
// unknown names) and answers in its own vocabulary — port-state and
// designated-bridge, protocol-mode only from the bridge monitor. The
// proplists ask for those keys too, so nothing the parsers read is dropped.
func TestBatchedMonitorsKeepPre719Keys(t *testing.T) {
	dev := &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		switch cmd {
		case "/interface/bridge/port/print":
			return sentences(
				map[string]string{".id": "*1", "bridge": "bridge", "interface": "ether1", "disabled": "false", "inactive": "false"},
				map[string]string{".id": "*2", "bridge": "bridge", "interface": "ether2", "disabled": "false", "inactive": "false"},
			), nil
		case "/interface/bridge/port/monitor":
			return filterProplist(args,
				map[string]string{"interface": "ether1", "port-state": "forwarding", "role": "root-port", "designated-bridge": "0x8000.BB"},
				map[string]string{"interface": "ether2", "port-state": "discarding", "role": "alternate-port", "designated-bridge": "0x8000.CC"}), nil
		case "/interface/bridge/monitor":
			return filterProplist(args, map[string]string{"bridge-id": "0x8000.AA", "protocol-mode": "rstp", "root-port": "ether1"}), nil
		}
		return nil, errTrap
	}}
	st, err := readBridgeState(dev, []BridgeInfo{{ID: "*1", Name: "bridge"}}, BridgeCaps{}, "7.18.2", testNow)
	if err != nil || st.Path != BridgePathMonitorBatched {
		t.Fatalf("path %q err %v (calls %v)", st.Path, err, dev.calls)
	}
	if p := portByName(t, st.Ports, "ether1"); p.DesignatedBridge != "0x8000.BB" || p.Status != "forwarding" || p.Role != "root" {
		t.Errorf("ether1 = %+v", p)
	}
	if p := portByName(t, st.Ports, "ether2"); p.DesignatedBridge != "0x8000.CC" || p.Status != "discarding" {
		t.Errorf("ether2 = %+v", p)
	}
	if b := st.Bridges[0]; b.ProtocolMode != "rstp" || b.BridgeID != "0x8000.AA" {
		t.Errorf("bridge = %+v, want protocol-mode from the monitor", b)
	}
}

func TestReadBridgeStateEmptyBatchedReplyIsNotLearned(t *testing.T) {
	batched := 0
	dev := device723(func(args []string) (*ros.Reply, error) {
		if isBatchedMonitor(args) {
			batched++
			return sentences(), nil // accepted but empty: no verdict on the form
		}
		return perPortReply(args)
	})
	bridges := []BridgeInfo{{ID: "*1", Name: "bridge", ProtocolMode: "rstp"}}
	st, err := readBridgeState(dev, bridges, BridgeCaps{}, "7.22.2", testNow)
	if err != nil {
		t.Fatal(err)
	}
	// Print + 1 empty batched attempt (the plain form is not tried: the
	// device did not reject the proplisted one) + 2 per-port + bridge monitor.
	if st.Path != BridgePathMonitorPerPort || st.Commands != 5 || batched != 1 {
		t.Fatalf("path %q, %d commands, %d batched; want monitor-per-port, 5, 1 (calls: %v)", st.Path, st.Commands, batched, dev.calls)
	}
	if st.Caps.PortMonitor != "" {
		t.Fatalf("an empty reply must not decide the form, got %q", st.Caps.PortMonitor)
	}
}

func TestReadBridgeStateReprobesOnVersionChange(t *testing.T) {
	dev := device723(func(args []string) (*ros.Reply, error) {
		if !isBatchedMonitor(args) {
			t.Fatalf("per-port monitor after an upgrade: %v", args)
		}
		return sentences(
			map[string]string{"interface": "ether1", "status": "in-bridge", "role": "root-port", "forwarding": "true"},
			map[string]string{"interface": "ether2", "status": "in-bridge", "role": "designated-port", "forwarding": "true"},
		), nil
	})
	bridges := []BridgeInfo{{ID: "*1", Name: "bridge", ProtocolMode: "rstp"}}
	caps := BridgeCaps{ProbedAt: testNow, Version: "7.20.8", PortMonitor: monitorPerItem, BridgeMonitor: monitorPerItem}
	st, err := readBridgeState(dev, bridges, caps, "7.23.2", testNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if st.Path != BridgePathMonitorBatched || st.Caps.PortMonitor != monitorBatched || st.Caps.Version != "7.23.2" {
		t.Fatalf("path %q, caps %+v", st.Path, st.Caps)
	}
}

func TestReadBridgeStateInactivePortsOnlyIsInconclusive(t *testing.T) {
	dev := &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		switch cmd {
		case "/interface/bridge/port/print":
			return sentences(
				map[string]string{".id": "*1", "bridge": "bridge", "interface": "ether1", "disabled": "false", "inactive": "true"},
				map[string]string{".id": "*2", "bridge": "bridge", "interface": "ether2", "disabled": "true", "inactive": "true"},
			), nil
		case "/interface/bridge/monitor":
			return sentences(bridgeMonitorRow()), nil
		case "/interface/bridge/port/monitor":
			t.Fatalf("nothing to monitor: %v", args)
		}
		return nil, errTrap
	}}
	st, err := readBridgeState(dev, []BridgeInfo{{Name: "bridge", ProtocolMode: "rstp"}}, BridgeCaps{}, "7.24.5", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.Path != BridgePathNone || st.Commands != 2 || !st.Caps.ProbedAt.IsZero() {
		t.Fatalf("path %q, %d commands, caps %+v", st.Path, st.Commands, st.Caps)
	}
	if e1 := portByName(t, st.Ports, "ether1"); e1.Status != "inactive" || e1.HasRuntime {
		t.Fatalf("ether1 = %+v", e1)
	}
	if e2 := portByName(t, st.Ports, "ether2"); e2.Status != "disabled" {
		t.Fatalf("ether2 = %+v", e2)
	}
}

func TestReadBridgeStateNoBridgesCostsNothing(t *testing.T) {
	dev := &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		t.Fatalf("unexpected command %s %v", cmd, args)
		return nil, nil
	}}
	st, err := readBridgeState(dev, nil, BridgeCaps{}, "7.24.5", testNow)
	if err != nil || st.Commands != 0 || st.Path != BridgePathNone || len(st.Ports) != 0 {
		t.Fatalf("state %+v, err %v", st, err)
	}
}

func TestMonitorBridgesBatchedOrderAndFallback(t *testing.T) {
	bridges := []BridgeInfo{{ID: "*1", Name: "bridge"}, {ID: "*2", Name: "guest"}}

	// A complete batched reply is mapped in request order.
	dev := &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		if argValue(args, "=numbers=") != "*1,*2" || !hasArg(args, "=.proplist="+bridgeMonitorProplist) {
			t.Fatalf("unexpected bridge monitor args %v", args)
		}
		return sentences(
			map[string]string{"bridge-id": "0x8000.AA", "root-bridge": "yes", "port-count": "10"},
			map[string]string{"bridge-id": "0x8000.BB", "root-bridge": "no", "root-port": "ether9", "port-count": "2"},
		), nil
	}}
	caps := BridgeCaps{}
	cmds := 0
	out, _ := monitorBridges(dev, bridges, &caps, &cmds)
	if cmds != 1 || caps.BridgeMonitor != monitorBatched || out[0].BridgeID != "0x8000.AA" || !out[0].IsRoot ||
		out[1].BridgeID != "0x8000.BB" || out[1].RootPort != "ether9" || out[1].PortCount != 2 {
		t.Fatalf("cmds %d, caps %+v, out %+v", cmds, caps, out)
	}

	// A short reply cannot be attributed (the monitor names no bridge): fall
	// back to one call per bridge this cycle, without learning a form.
	dev = &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		if strings.Contains(argValue(args, "=numbers="), ",") {
			return sentences(map[string]string{"bridge-id": "0x8000.AA"}), nil
		}
		return sentences(map[string]string{"bridge-id": "per-" + argValue(args, "=numbers=")}), nil
	}}
	caps, cmds = BridgeCaps{}, 0
	out, _ = monitorBridges(dev, bridges, &caps, &cmds)
	if cmds != 3 || caps.BridgeMonitor != "" || out[0].BridgeID != "per-*1" || out[1].BridgeID != "per-*2" {
		t.Fatalf("cmds %d, caps %+v, out %+v", cmds, caps, out)
	}

	// Both batched forms rejected: per-bridge, and remembered.
	dev = &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		if strings.Contains(argValue(args, "=numbers="), ",") {
			return nil, errTrap
		}
		return sentences(map[string]string{"bridge-id": "per-" + argValue(args, "=numbers=")}), nil
	}}
	caps, cmds = BridgeCaps{}, 0
	out, _ = monitorBridges(dev, bridges, &caps, &cmds)
	if cmds != 4 || caps.BridgeMonitor != monitorPerItem || out[1].BridgeID != "per-*2" {
		t.Fatalf("cmds %d, caps %+v, out %+v", cmds, caps, out)
	}
	cmds = 0
	monitorBridges(dev, bridges, &caps, &cmds)
	if cmds != 2 {
		t.Fatalf("learned per-item form still tried batched: %d commands", cmds)
	}
}

func TestListBridges(t *testing.T) {
	dev := &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		if cmd != "/interface/bridge/print" || !hasArg(args, "=.proplist="+bridgeProplist) {
			t.Fatalf("unexpected %s %v", cmd, args)
		}
		return sentences(
			map[string]string{".id": "*1", "name": "bridge", "protocol-mode": "RSTP", "vlan-filtering": "true", "disabled": "false", "running": "true", "comment": "core"},
			map[string]string{".id": "*2", "name": "guest", "protocol-mode": "none", "vlan-filtering": "false", "disabled": "true", "running": "false"},
			map[string]string{".id": "*3"}, // nameless rows are useless
		), nil
	}}
	bridges, err := listBridges(dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(bridges) != 2 {
		t.Fatalf("got %d bridges: %+v", len(bridges), bridges)
	}
	b := bridges[0]
	if b.ID != "*1" || b.Name != "bridge" || b.ProtocolMode != "rstp" || !b.VLANFiltering || b.Disabled || !b.Running || b.Comment != "core" || !b.STPEnabled() {
		t.Fatalf("bridge = %+v", b)
	}
	if g := bridges[1]; g.STPEnabled() || !g.Disabled || g.VLANFiltering {
		t.Fatalf("guest = %+v", g)
	}
}

func TestDeriveSTPStatus(t *testing.T) {
	tests := []struct {
		name                      string
		state, role               string
		forwarding, learning, has bool
		want                      string
	}{
		{"7.19+ forwarding", "in-bridge", "designated", true, true, true, "forwarding"},
		{"7.19+ learning", "in-bridge", "designated", false, true, true, "learning"},
		{"7.19+ discarding", "in-bridge", "alternate", false, false, true, "discarding"},
		{"7.19+ inactive", "inactive", "disabled", false, false, true, "inactive"},
		{"disabled role without booleans", "in-bridge", "disabled", false, false, false, "disabled"},
		{"old monitor vocabulary", "forwarding", "designated", false, false, false, "forwarding"},
		{"old monitor discarding", "discarding", "alternate", false, false, false, "discarding"},
		{"no state at all", "in-bridge", "designated", false, false, false, ""},
	}
	for _, tc := range tests {
		if got := deriveSTPStatus(tc.state, tc.role, tc.forwarding, tc.learning, tc.has); got != tc.want {
			t.Errorf("%s: deriveSTPStatus = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestApplyPortRuntimeOldKeys(t *testing.T) {
	p := BridgePortInfo{Interface: "ether5", Edge: true}
	applyPortRuntime(&p, map[string]string{"port-state": "Discarding", "role": "Alternate-Port", "designated-bridge": "0x8000.CC"})
	if !p.HasRuntime || p.Role != "alternate" || p.Status != "discarding" || p.DesignatedBridge != "0x8000.CC" || !p.Edge || p.HasTopologyChanges {
		t.Fatalf("port = %+v", p)
	}
	// A sentence without role and status leaves the port untouched.
	q := BridgePortInfo{Interface: "ether6"}
	applyPortRuntime(&q, map[string]string{"forwarding": "true"})
	if q.HasRuntime || q.Status != "" {
		t.Fatalf("port = %+v", q)
	}
}

func TestParseBridgePortRowPhantoms(t *testing.T) {
	for _, m := range []map[string]string{
		{".id": "*1", "bridge": "", "interface": "ether1"},
		{".id": "*2", "bridge": "bridge", "interface": ""},
		{".id": "*3", "bridge": "bridge", "interface": "ether1,ether2"},
	} {
		if _, ok := parseBridgePortRow(m); ok {
			t.Errorf("phantom row accepted: %v", m)
		}
	}
	p, ok := parseBridgePortRow(map[string]string{".id": "*4", "bridge": "bridge", "interface": "ether4", "edge": "yes-discover", "point-to-point": "yes", "disabled": "true"})
	if !ok || !p.Edge || !p.PointToPoint || p.Status != "disabled" || p.HasRuntime {
		t.Fatalf("port = %+v", p)
	}
}
