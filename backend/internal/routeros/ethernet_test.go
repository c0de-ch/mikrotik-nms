package routeros

import (
	"testing"

	ros "github.com/go-routeros/routeros/v3"
)

func TestEthernetProplistIsTier4a(t *testing.T) {
	const want = "=.proplist=name,switch,loop-protect,loop-protect-status,rx-flow-control,tx-flow-control," +
		"tx-drop-packet,tx-drop-byte,rx-overflow,rx-fcs-error,rx-pause,tx-pause,driver-rx-packet," +
		"driver-tx-packet,rx-broadcast,rx-multicast"
	if ethernetProplist != want {
		t.Fatalf("proplist =\n  %s\nwant\n  %s", ethernetProplist, want)
	}
}

func TestParseEthernetStats(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		ok   bool
		want EthernetStats
	}{
		{
			name: "switch port with every field",
			in: map[string]string{
				"name": "ether5", "switch": "switch1", "loop-protect": "on", "loop-protect-status": "on-loop",
				"rx-flow-control": "off", "tx-flow-control": "auto", "tx-drop-packet": "11", "tx-drop-byte": "12",
				"rx-overflow": "13", "rx-fcs-error": "14", "rx-pause": "15", "tx-pause": "16",
				"driver-rx-packet": "17", "driver-tx-packet": "18", "rx-broadcast": "19", "rx-multicast": "20",
			},
			ok: true,
			want: EthernetStats{
				Name: "ether5", Switch: "switch1", LoopProtect: "on", LoopProtectStatus: "on-loop",
				RxFlowControl: "off", TxFlowControl: "auto", TxDropPackets: 11, TxDropBytes: 12,
				RxOverflow: 13, RxFCSErrors: 14, RxPause: 15, TxPause: 16, DriverRxPackets: 17,
				DriverTxPackets: 18, RxBroadcast: 19, RxMulticast: 20, HasStats: true,
			},
		},
		{
			// Older versions and prints without =stats= leave the counters
			// out: the row still carries the loop-protect state.
			name: "config fields only",
			in:   map[string]string{"name": "sfp-sfpplus1", "loop-protect": "default", "loop-protect-status": "off"},
			ok:   true,
			want: EthernetStats{Name: "sfp-sfpplus1", LoopProtect: "default", LoopProtectStatus: "off"},
		},
		{
			name: "row without name is skipped",
			in:   map[string]string{"switch": "switch1", "rx-pause": "1"},
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseEthernetStats(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("parseEthernetStats() =\n  %+v\nwant\n  %+v", got, tt.want)
			}
		})
	}
}

func TestGetEthernetStatsIssuesOneProplistedPrint(t *testing.T) {
	dev := &scriptedDevice{handle: func(cmd string, args []string) (*ros.Reply, error) {
		return sentences(
			map[string]string{"name": "ether1", "loop-protect-status": "on"},
			map[string]string{"switch": "switch1"},
		), nil
	}}
	got, err := GetEthernetStats(dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(dev.calls) != 1 || dev.calls[0] != "/interface/ethernet/print "+ethernetProplist {
		t.Fatalf("calls = %q, want exactly one proplisted ethernet print", dev.calls)
	}
	if len(got) != 1 || got[0].Name != "ether1" || got[0].LoopProtectStatus != "on" {
		t.Fatalf("rows = %+v", got)
	}
}

func TestMergeEthernetByName(t *testing.T) {
	rows := []InterfaceStats{{Name: "ether1", Type: "ether"}, {Name: "bridge", Type: "bridge"}, {Name: "ether2", Type: "ether"}}
	eth := map[string]EthernetStats{
		"ether1": {Name: "ether1", LoopProtectStatus: "on-loop"},
		"ether9": {Name: "ether9"}, // renamed or removed since: not merged
	}
	got := MergeEthernet(rows, eth)
	if len(got) != 3 {
		t.Fatalf("merged %d rows, want 3", len(got))
	}
	if !got[0].HasEthernet || got[0].Ethernet.LoopProtectStatus != "on-loop" || got[0].Name != "ether1" {
		t.Errorf("ether1 = %+v", got[0])
	}
	if got[1].HasEthernet || got[2].HasEthernet {
		t.Errorf("rows without an ethernet row must not claim one: %+v %+v", got[1], got[2])
	}
	// The result is a copy: changing it leaves the input alone.
	got[1].Name = "changed"
	if rows[1].Name != "bridge" {
		t.Error("MergeEthernet must not alias its input")
	}
	if n := len(MergeEthernet(nil, nil)); n != 0 {
		t.Errorf("empty merge = %d rows", n)
	}
}
