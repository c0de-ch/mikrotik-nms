package routeros

import "testing"

func TestParseBridgeLogMessage(t *testing.T) {
	cases := []struct {
		name       string
		msg        string
		wantKind   string
		wantPort   string
		wantOther  string
		wantBridge string
		wantMAC    string
	}{
		{
			name:     "loop detected on port",
			msg:      "loop detected on ether3",
			wantKind: "loop_detected",
			wantPort: "ether3",
		},
		{
			name:       "loop detected with bridge prefix",
			msg:        "bridge1: loop detected on ether7",
			wantKind:   "loop_detected",
			wantBridge: "bridge1",
			wantPort:   "ether7",
		},
		{
			name:       "mac flap moved from to",
			msg:        "bridge1: host AA:BB:CC:DD:EE:FF moved from ether2 to ether5",
			wantKind:   "mac_flap",
			wantBridge: "bridge1",
			wantMAC:    "AA:BB:CC:DD:EE:FF",
			wantPort:   "ether2",
			wantOther:  "ether5",
		},
		{
			name:     "mac flap short form",
			msg:      "mac flapping detected: 11:22:33:44:55:66 on ether8",
			wantKind: "mac_flap",
			wantMAC:  "11:22:33:44:55:66",
			wantPort: "ether8",
		},
		{
			name:     "bpdu on edge port",
			msg:      "BPDU received on edge port ether10",
			wantKind: "bpdu_on_edge",
			wantPort: "ether10",
		},
		{
			name: "ignore unrelated",
			msg:  "interface ether1 link up",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseBridgeLogMessage(tc.msg)
			if tc.wantKind == "" {
				if got != nil {
					t.Fatalf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected kind=%q, got nil", tc.wantKind)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("kind: want %q got %q", tc.wantKind, got.Kind)
			}
			if got.Port != tc.wantPort {
				t.Errorf("port: want %q got %q", tc.wantPort, got.Port)
			}
			if got.OtherPort != tc.wantOther {
				t.Errorf("other port: want %q got %q", tc.wantOther, got.OtherPort)
			}
			if got.Bridge != tc.wantBridge {
				t.Errorf("bridge: want %q got %q", tc.wantBridge, got.Bridge)
			}
			if got.MAC != tc.wantMAC {
				t.Errorf("mac: want %q got %q", tc.wantMAC, got.MAC)
			}
		})
	}
}
