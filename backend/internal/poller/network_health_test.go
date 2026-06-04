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
	ifaces := []routeros.InterfaceInfo{
		{Name: "bridge", Type: "bridge"},
		{Name: "internal", Type: "bridge"},
		{Name: "ether1", Type: "ether"},
		{Name: "lo", Type: "loopback"},
		{Name: "wg-owcam", Type: "wg"},
		{Name: "net28", Type: "vlan"},
		{Name: "veth1", Type: "veth"},
	}
	got := bridgeNames(filterRealBridges(bridges, ifaces))
	if len(got) != 2 || got[0] != "bridge" || got[1] != "internal" {
		t.Fatalf("expected only real bridges [bridge internal], got %v", got)
	}

	// No interface info -> no-op (never lose bridges on a transient fetch error).
	if n := len(filterRealBridges(bridges, nil)); n != len(bridges) {
		t.Fatalf("nil ifaces should be a no-op, got %d bridges", n)
	}

	// A bridge not present in the interface list (unknown) is kept, not dropped.
	got = bridgeNames(filterRealBridges(
		[]routeros.BridgeInfo{{Name: "mystery"}},
		[]routeros.InterfaceInfo{{Name: "ether1", Type: "ether"}},
	))
	if len(got) != 1 || got[0] != "mystery" {
		t.Fatalf("unknown bridge should be kept, got %v", got)
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
