package poller

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

func bridgeNames(bs []routeros.BridgeInfo) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name
	}
	return out
}

func TestFilterRealBridges(t *testing.T) {
	bridges := []routeros.BridgeInfo{
		{Name: "bridge"}, {Name: "internal"}, {Name: "ether1"},
		{Name: "lo"}, {Name: "wg-owcam"}, {Name: "net28"}, {Name: "veth1"},
	}
	// Names known (by type) NOT to be bridges.
	nonBridge := map[string]bool{
		"ether1": true, "lo": true, "wg-owcam": true, "net28": true, "veth1": true,
	}
	got := bridgeNames(filterRealBridges(bridges, nonBridge))
	if len(got) != 2 || got[0] != "bridge" || got[1] != "internal" {
		t.Fatalf("expected only real bridges [bridge internal], got %v", got)
	}

	// Empty set -> no-op (never lose bridges when type info is unavailable).
	if n := len(filterRealBridges(bridges, nil)); n != len(bridges) {
		t.Fatalf("nil set should be a no-op, got %d bridges", n)
	}

	// A bridge not in the non-bridge set (unknown) is kept, not dropped.
	got = bridgeNames(filterRealBridges(
		[]routeros.BridgeInfo{{Name: "mystery"}},
		map[string]bool{"ether1": true},
	))
	if len(got) != 1 || got[0] != "mystery" {
		t.Fatalf("unknown bridge should be kept, got %v", got)
	}
}

func TestShouldFlagSTPDisabled(t *testing.T) {
	stp := routeros.BridgeInfo{Name: "bridge", ProtocolMode: "rstp"}
	noStp := routeros.BridgeInfo{Name: "bridge", ProtocolMode: "none"}
	tests := []struct {
		name    string
		b       routeros.BridgeInfo
		everSTP bool
		nonEdge int
		want    bool
	}{
		{"STP running now -> no warn", stp, false, 5, false},
		{"flicker: ever ran STP -> suppressed", noStp, true, 5, false},
		{"genuine STP-off with >1 non-edge -> warn", noStp, false, 3, true},
		{"STP-off single uplink -> no warn", noStp, false, 1, false},
		{"loopback never warns", routeros.BridgeInfo{Name: "lo", ProtocolMode: "none"}, false, 9, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldFlagSTPDisabled(tc.b, tc.everSTP, tc.nonEdge); got != tc.want {
				t.Fatalf("shouldFlagSTPDisabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCountNonEdgePorts(t *testing.T) {
	ports := []routeros.BridgePortInfo{{Edge: true}, {Edge: false}, {Edge: false}, {Edge: true}}
	if n := countNonEdgePorts(ports); n != 2 {
		t.Fatalf("countNonEdgePorts = %d, want 2", n)
	}
	if n := countNonEdgePorts(nil); n != 0 {
		t.Fatalf("countNonEdgePorts(nil) = %d, want 0", n)
	}
}

func TestTCNStormSeverity(t *testing.T) {
	stp := routeros.BridgeInfo{Name: "bridge", ProtocolMode: "rstp"}
	noStp := routeros.BridgeInfo{Name: "ether1", ProtocolMode: "none"}

	tests := []struct {
		name      string
		b         routeros.BridgeInfo
		delta     int
		threshold int
		wantSev   string
		wantFire  bool
	}{
		{"non-STP huge delta never fires", noStp, 710, 30, "", false},
		{"STP below threshold", stp, 10, 30, "", false},
		{"STP warn", stp, 50, 30, "warn", true},
		{"STP critical", stp, 710, 30, "critical", true},
		{"STP critical at ceiling", stp, 100, 30, "critical", true},
		{"STP no change", stp, 0, 30, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sev, fire := tcnStormSeverity(tc.b, tc.delta, tc.threshold)
			if fire != tc.wantFire || sev != tc.wantSev {
				t.Fatalf("tcnStormSeverity = (%q, fire=%v), want (%q, fire=%v)", sev, fire, tc.wantSev, tc.wantFire)
			}
		})
	}
}

func TestNormalizeTCNDelta(t *testing.T) {
	tests := []struct {
		delta   int
		elapsed time.Duration
		want    int
	}{
		{30, time.Minute, 30},
		{30, 61 * time.Second, 30},  // a 60 s cadence jittering by a second is still 30/min
		{5, 5 * time.Second, 5},     // a fast cycle is never inflated
		{300, 10 * time.Minute, 30}, // a 10 min cadence sees the same storm as a 1 min one
		{90, 3 * time.Minute, 30},
		{0, time.Minute, 0},
		{-3, time.Minute, 0},
	}
	for _, tc := range tests {
		if got := normalizeTCNDelta(tc.delta, tc.elapsed); got != tc.want {
			t.Errorf("normalizeTCNDelta(%d, %s) = %d, want %d", tc.delta, tc.elapsed, got, tc.want)
		}
	}
}

func TestBridgeTCNDelta(t *testing.T) {
	n := NewNetworkHealthPoller(nil, nil, nil, time.Minute)
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ports := func(a, b int) []routeros.BridgePortInfo {
		return []routeros.BridgePortInfo{
			{Interface: "ether1", TopologyChanges: a, HasTopologyChanges: true},
			{Interface: "ether2", TopologyChanges: b, HasTopologyChanges: true},
		}
	}
	const key = "dev|bridge"

	// First sample: nothing to compare against.
	if d, ok := n.bridgeTCNDelta(key, ports(10, 20), t0); ok {
		t.Fatalf("first sample: ok with delta %d, want no delta", d)
	}
	// One minute later, +5 and +7 → 12 per minute.
	if d, ok := n.bridgeTCNDelta(key, ports(15, 27), t0.Add(time.Minute)); !ok || d != 12 {
		t.Fatalf("second sample: (%d, %v), want (12, true)", d, ok)
	}
	// A port joining with a big counter and a counter reset contribute
	// nothing; only ether2's +3 counts.
	p := append(ports(2, 30), routeros.BridgePortInfo{Interface: "ether3", TopologyChanges: 500, HasTopologyChanges: true})
	if d, ok := n.bridgeTCNDelta(key, p, t0.Add(2*time.Minute)); !ok || d != 3 {
		t.Fatalf("reset + new port: (%d, %v), want (3, true)", d, ok)
	}
	// Five minutes later, +50 across the ports → 10 per minute.
	p = append(ports(22, 60), routeros.BridgePortInfo{Interface: "ether3", TopologyChanges: 500, HasTopologyChanges: true})
	if d, ok := n.bridgeTCNDelta(key, p, t0.Add(7*time.Minute)); !ok || d != 10 {
		t.Fatalf("five-minute gap: (%d, %v), want (10, true)", d, ok)
	}
	// A device whose RouterOS reports no per-port counter never compares.
	if d, ok := n.bridgeTCNDelta("dev|other", []routeros.BridgePortInfo{{Interface: "ether1"}}, t0); ok {
		t.Fatalf("no counters: ok with delta %d", d)
	}
	if d, ok := n.bridgeTCNDelta("dev|other", []routeros.BridgePortInfo{{Interface: "ether1"}}, t0.Add(time.Minute)); ok {
		t.Fatalf("no counters, second sample: ok with delta %d", d)
	}

	// Pruning a vanished device forgets its samples.
	n.pruneTCState(map[string]struct{}{"other-device": {}})
	if len(n.prevPortTC) != 0 || len(n.prevTCAt) != 0 {
		t.Fatalf("prune left %d port samples, %d bridge samples", len(n.prevPortTC), len(n.prevTCAt))
	}
}
