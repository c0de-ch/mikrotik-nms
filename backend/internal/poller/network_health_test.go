package poller

import (
	"testing"

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
	stp := func(tc int) routeros.BridgeInfo {
		return routeros.BridgeInfo{Name: "bridge", ProtocolMode: "rstp", TopologyChanges: tc}
	}
	noStp := func(tc int) routeros.BridgeInfo {
		return routeros.BridgeInfo{Name: "ether1", ProtocolMode: "none", TopologyChanges: tc}
	}

	tests := []struct {
		name      string
		b         routeros.BridgeInfo
		prev      int
		threshold int
		wantSev   string
		wantFire  bool
	}{
		{"non-STP huge delta never fires", noStp(710), 0, 30, "", false},
		{"STP below threshold", stp(10), 0, 30, "", false},
		{"STP warn", stp(50), 0, 30, "warn", true},
		{"STP critical", stp(710), 0, 30, "critical", true},
		{"STP critical at ceiling", stp(100), 0, 30, "critical", true},
		{"STP no change", stp(5), 5, 30, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sev, _, fire := tcnStormSeverity(tc.b, tc.prev, tc.threshold)
			if fire != tc.wantFire || sev != tc.wantSev {
				t.Fatalf("tcnStormSeverity = (%q, fire=%v), want (%q, fire=%v)", sev, fire, tc.wantSev, tc.wantFire)
			}
		})
	}
}
