package routeros

import (
	"slices"
	"strings"
	"testing"

	ros "github.com/go-routeros/routeros/v3"
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
				"rx-error": "5", "tx-error": "6", "rx-drop": "7", "tx-drop": "8", "tx-queue-drop": "9",
			},
			ok: true,
			want: InterfaceStats{
				ID: "*40C", Name: "net28", Type: "vlan", Comment: "iot", Running: true,
				RxBytes: 100, TxBytes: 200, RxPackets: 3, TxPackets: 4,
				RxErrors: 5, TxErrors: 6, RxDrops: 7, TxDrops: 8, TxQueueDrops: 9, HasCounters: true,
			},
		},
		{
			name: "full Tier 0 row: inventory, link state and link-downs",
			in: map[string]string{
				".id": "*3", "name": "ether3", "type": "ether", "mac-address": "02:00:00:00:00:03", "mtu": "1500",
				"running": "true", "disabled": "false", "slave": "true", "comment": "uplink",
				"last-link-up-time": "2026-10-16 09:14:22", "last-link-down-time": "2026-10-16 09:14:19",
				"link-downs": "7", "rx-byte": "1", "tx-byte": "2",
			},
			ok: true,
			want: InterfaceStats{
				ID: "*3", Name: "ether3", Type: "ether", MAC: "02:00:00:00:00:03", MTU: 1500, Comment: "uplink",
				Running: true, Slave: true, LastLinkUp: "2026-10-16 09:14:22", LastLinkDown: "2026-10-16 09:14:19",
				LinkDowns: 7, HasLinkDowns: true, RxBytes: 1, TxBytes: 2, HasCounters: true,
			},
		},
		{
			name: "link-downs 0 is a counter, a non-numeric mtu is absent",
			in:   map[string]string{"name": "pppoe-out1", "type": "pppoe-out", "mtu": "auto", "link-downs": "0"},
			ok:   true,
			want: InterfaceStats{Name: "pppoe-out1", Type: "pppoe-out", HasLinkDowns: true},
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
			in:   map[string]string{"name": "ether9", "rx-byte": "n/a", "tx-byte": "", "link-downs": "x"},
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

// The Tier 0 proplist is part of the device-load contract (design Appendix
// B): every consumer of the snapshot reads from exactly this attribute set,
// so a change here is a change to what every device is asked every cycle.
func TestInterfaceStatsProplistIsTier0(t *testing.T) {
	const want = "=.proplist=.id,name,type,mac-address,mtu,running,disabled,slave,comment," +
		"last-link-up-time,last-link-down-time,link-downs,rx-byte,tx-byte,rx-packet,tx-packet," +
		"rx-error,tx-error,rx-drop,tx-drop,tx-queue-drop"
	if interfaceStatsProplist != want {
		t.Fatalf("proplist =\n  %s\nwant\n  %s", interfaceStatsProplist, want)
	}
	// =stats= is accepted by RouterOS but redundant; the proplist names every
	// attribute the parser reads.
	for _, attr := range []string{".id", "name", "type", "mac-address", "mtu", "running", "disabled", "slave",
		"comment", "last-link-up-time", "last-link-down-time", "link-downs", "rx-byte", "tx-byte",
		"rx-packet", "tx-packet", "rx-error", "tx-error", "rx-drop", "tx-drop", "tx-queue-drop"} {
		if !containsWord(interfaceStatsProplist, attr) {
			t.Errorf("proplist missing %q", attr)
		}
	}
}

func TestGetInterfaceStatsIssuesOneProplistedPrint(t *testing.T) {
	dev := &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		return sentences(
			map[string]string{".id": "*1", "name": "ether1", "type": "ether", "running": "true", "rx-byte": "10", "tx-byte": "20", "link-downs": "2"},
			map[string]string{".id": "*2", "type": "ether"}, // nameless: dropped
		), nil
	}}
	rows, err := GetInterfaceStats(dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(dev.calls) != 1 || dev.calls[0] != "/interface/print "+interfaceStatsProplist {
		t.Fatalf("calls = %q, want exactly one proplisted /interface/print", dev.calls)
	}
	if len(rows) != 1 || rows[0].Name != "ether1" || rows[0].LinkDowns != 2 || !rows[0].HasLinkDowns {
		t.Fatalf("rows = %+v", rows)
	}

	failing := &scriptedDevice{handle: func(string, []string) (*ros.Reply, error) { return nil, errTrap }}
	if _, err := GetInterfaceStats(failing); err == nil {
		t.Fatal("a trap must surface as an error")
	}
}

func TestGetInterfacesIsProplisted(t *testing.T) {
	dev := &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		return sentences(
			map[string]string{"name": "ether1", "type": "ether", "mac-address": "02:00:00:00:00:01"},
			map[string]string{"name": "bridge", "type": "bridge", "mac-address": "02:00:00:00:00:01"},
		), nil
	}}
	got, err := GetInterfaces(dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(dev.calls) != 1 || dev.calls[0] != "/interface/print =.proplist=name,type,mac-address" {
		t.Fatalf("calls = %q, want one /interface/print with the identity proplist", dev.calls)
	}
	if len(got) != 2 || got[1] != (InterfaceInfo{Name: "bridge", Type: "bridge", MACAddress: "02:00:00:00:00:01"}) {
		t.Fatalf("interfaces = %+v", got)
	}
}

// containsWord reports whether the comma list after "=.proplist=" contains attr.
func containsWord(proplist, attr string) bool {
	list, ok := strings.CutPrefix(proplist, "=.proplist=")
	return ok && slices.Contains(strings.Split(list, ","), attr)
}
