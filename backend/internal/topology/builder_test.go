package topology

import "testing"

func TestInferDeviceType(t *testing.T) {
	tests := []struct {
		board string
		want  string
	}{
		// Routers
		{"CCR2004-1G-12S+2XS", "router"},
		{"CCR1036-12G-4S", "router"},
		{"RB4011iGS+5HacQ2HnD", "router"},
		{"RB5009UG+S+IN", "router"},
		{"hEX S", "router"},
		{"RB750Gr3", "router"},

		// Switches
		{"CSS610-8G-2S+IN", "switch"},
		{"CRS326-24G-2S+RM", "switch"},
		{"CRS328-24P-4S+RM", "switch"},

		// Access points
		{"cAP ac", "ap"},
		{"wAP ac", "ap"},
		{"hAP ac3", "ap"},
		{"Audience", "ap"},

		// Empty board -> unknown
		{"", "unknown"},

		// Unrecognized board -> default router
		{"SomeOtherBoard", "router"},
	}

	for _, tt := range tests {
		t.Run(tt.board, func(t *testing.T) {
			got := inferDeviceType(tt.board)
			if got != tt.want {
				t.Errorf("inferDeviceType(%q) = %q, want %q", tt.board, got, tt.want)
			}
		})
	}
}

func TestIsWirelessType(t *testing.T) {
	tests := []struct {
		ifaceType string
		want      bool
	}{
		{"wlan", true},
		{"WLAN", true},
		{"wireless", true},
		{"Wireless", true},
		{"wifi-channel", true},
		{"60g-something", true},
		{"ether", false},
		{"bridge", false},
		{"vlan", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.ifaceType, func(t *testing.T) {
			got := isWirelessType(tt.ifaceType)
			if got != tt.want {
				t.Errorf("isWirelessType(%q) = %v, want %v", tt.ifaceType, got, tt.want)
			}
		})
	}
}
