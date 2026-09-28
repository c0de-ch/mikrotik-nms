package routeros

import (
	"slices"
	"strings"
	"testing"
)

func TestParseInterfaceStats(t *testing.T) {
	tests := []struct {
		name   string
		in     map[string]string
		ok     bool
		want   InterfaceStats
		absent bool // error/drop counters expected to be 0 because absent
	}{
		{
			name: "ether port without error/drop attributes, counter above 2^32",
			in: map[string]string{
				".id": "*1", "name": "ether1", "type": "ether", "running": "true", "disabled": "false",
				"rx-byte": "12135953092", "tx-byte": "987654321", "rx-packet": "11223344", "tx-packet": "5566778",
			},
			ok: true,
			want: InterfaceStats{
				ID: "*1", Name: "ether1", Type: "ether", Running: true,
				RxBytes: 12135953092, TxBytes: 987654321, RxPackets: 11223344, TxPackets: 5566778,
				HasCounters: true,
			},
		},
		{
			name: "vlan with every counter and a comment",
			in: map[string]string{
				".id": "*40C", "name": "net28", "type": "vlan", "running": "true", "disabled": "false",
				"comment": "iot", "rx-byte": "100", "tx-byte": "200", "rx-packet": "3", "tx-packet": "4",
				"rx-error": "5", "tx-error": "6", "rx-drop": "7", "tx-drop": "8",
			},
			ok: true,
			want: InterfaceStats{
				ID: "*40C", Name: "net28", Type: "vlan", Comment: "iot", Running: true,
				RxBytes: 100, TxBytes: 200, RxPackets: 3, TxPackets: 4,
				RxErrors: 5, TxErrors: 6, RxDrops: 7, TxDrops: 8, HasCounters: true,
			},
		},
		{
			name: "dynamic CAPsMAN radio: not running, zero counters",
			in: map[string]string{
				".id": "*50", "name": "cap-wifi3", "type": "cap", "running": "false", "disabled": "false",
				"rx-byte": "0", "tx-byte": "0", "rx-packet": "0", "tx-packet": "0",
			},
			ok:   true,
			want: InterfaceStats{ID: "*50", Name: "cap-wifi3", Type: "cap", HasCounters: true},
		},
		{
			name: "disabled row without counters",
			in:   map[string]string{".id": "*7", "name": "sfp1", "type": "ether", "running": "false", "disabled": "true"},
			ok:   true,
			want: InterfaceStats{ID: "*7", Name: "sfp1", Type: "ether", Disabled: true},
		},
		{
			name: "only tx-byte present still counts as counters",
			in:   map[string]string{"name": "lo", "type": "loopback", "running": "true", "tx-byte": " 42 "},
			ok:   true,
			want: InterfaceStats{Name: "lo", Type: "loopback", Running: true, TxBytes: 42, HasCounters: true},
		},
		{
			name: "garbage counters parse as absent",
			in:   map[string]string{"name": "ether9", "rx-byte": "n/a", "tx-byte": ""},
			ok:   true,
			want: InterfaceStats{Name: "ether9"},
		},
		{
			name: "row without name is skipped",
			in:   map[string]string{".id": "*9", "type": "ether", "rx-byte": "1"},
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseInterfaceStats(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if got != tt.want {
				t.Errorf("parseInterfaceStats() =\n  %+v\nwant\n  %+v", got, tt.want)
			}
		})
	}
}

func TestInterfaceStatsProplistHasNoStatsFlag(t *testing.T) {
	// =stats= is accepted by RouterOS but redundant; the proplist must name
	// every counter the parser reads.
	for _, attr := range []string{".id", "name", "type", "running", "disabled", "comment",
		"rx-byte", "tx-byte", "rx-packet", "tx-packet", "rx-error", "tx-error", "rx-drop", "tx-drop"} {
		if !containsWord(interfaceStatsProplist, attr) {
			t.Errorf("proplist missing %q: %s", attr, interfaceStatsProplist)
		}
	}
}

// containsWord reports whether the comma list after "=.proplist=" contains attr.
func containsWord(proplist, attr string) bool {
	list, ok := strings.CutPrefix(proplist, "=.proplist=")
	return ok && slices.Contains(strings.Split(list, ","), attr)
}
