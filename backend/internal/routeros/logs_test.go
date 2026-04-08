package routeros

import "testing"

func TestParseWirelessLogMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want WirelessLogEvent
	}{
		{
			name: "connected",
			in:   "00:00:5E:00:53:A0@2ghz-wlan-ap14(ExampleWLAN) connected, signal strength -30",
			want: WirelessLogEvent{
				Event: "connected", MAC: "00:00:5E:00:53:A0",
				AP: "2ghz-wlan-ap14", SSID: "ExampleWLAN", Signal: "-30",
			},
		},
		{
			name: "disconnected_connection_lost",
			in:   "00:00:5E:00:53:A0@2ghz-wlan-ap14(ExampleWLAN) disconnected, connection lost, signal strength -35",
			want: WirelessLogEvent{
				Event: "disconnected", MAC: "00:00:5E:00:53:A0",
				AP: "2ghz-wlan-ap14", SSID: "ExampleWLAN",
				Reason: "connection lost", Signal: "-35",
			},
		},
		{
			name: "disconnected_not_responding",
			in:   "00:00:5E:00:53:D0@2ghz-wlan-ap03(ExampleWLAN) disconnected, not responding, signal strength -49",
			want: WirelessLogEvent{
				Event: "disconnected", MAC: "00:00:5E:00:53:D0",
				AP: "2ghz-wlan-ap03", SSID: "ExampleWLAN",
				Reason: "not responding", Signal: "-49",
			},
		},
		{
			name: "reconnecting",
			in:   "00:00:5E:00:53:6A@5ghz-wlan-ap07(ExampleWLAN) reconnecting, signal strength -68",
			want: WirelessLogEvent{
				Event: "reconnecting", MAC: "00:00:5E:00:53:6A",
				AP: "5ghz-wlan-ap07", SSID: "ExampleWLAN", Signal: "-68",
			},
		},
		{
			name: "roamed",
			in:   "00:00:5E:00:53:D7@2ghz-wlan-ap14b(GuestWiFi) roamed to 00:00:5E:00:53:D7@2ghz-wlan-ap09(GuestWiFi), signal strength -74",
			want: WirelessLogEvent{
				Event: "roamed", MAC: "00:00:5E:00:53:D7",
				AP: "2ghz-wlan-ap14b", SSID: "GuestWiFi",
				ToMAC: "00:00:5E:00:53:D7", ToAP: "2ghz-wlan-ap09", ToSSID: "GuestWiFi",
				Signal: "-74",
			},
		},
		{
			name: "lowercase_mac_normalized",
			in:   "aa:bb:cc:dd:ee:ff@wlan1(net) connected, signal strength -50",
			want: WirelessLogEvent{
				Event: "connected", MAC: "AA:BB:CC:DD:EE:FF",
				AP: "wlan1", SSID: "net", Signal: "-50",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseWirelessLogMessage(c.in)
			if got == nil {
				t.Fatalf("ParseWirelessLogMessage returned nil for %q", c.in)
			}
			if *got != c.want {
				t.Errorf("\n got %+v\nwant %+v", *got, c.want)
			}
		})
	}
}

func TestParseWirelessLogMessage_Unknown(t *testing.T) {
	for _, in := range []string{
		"",
		"some unrelated log message",
		"AA:BB:CC@badmac connected, signal strength -10",
		"AA:BB:CC:DD:EE:FF@iface_no_ssid connected, signal strength -10",
	} {
		if got := ParseWirelessLogMessage(in); got != nil {
			t.Errorf("expected nil for %q, got %+v", in, got)
		}
	}
}

func TestIsWirelessTopic(t *testing.T) {
	yes := []string{"wireless,info", "wifi,info,debug", "caps,info", "wireless", "info,wifi"}
	no := []string{"", "system,info", "interface,info", "dhcp,info"}
	for _, s := range yes {
		if !isWirelessTopic(s) {
			t.Errorf("expected true for %q", s)
		}
	}
	for _, s := range no {
		if isWirelessTopic(s) {
			t.Errorf("expected false for %q", s)
		}
	}
}
