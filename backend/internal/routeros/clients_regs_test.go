package routeros

import "testing"

func TestGetRegistrationsPerMenu(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		"/interface/wifi/registration-table/print": rep(map[string]string{
			"interface": "cap-wifi1/2", "mac-address": "00:00:5E:00:53:A0", "ssid": "Net", "band": "5ghz-ax", "signal": "-55",
		}),
		"/caps-man/registration-table/print": rep(map[string]string{
			"interface": "cap1", "mac-address": "00:00:5E:00:53:A1", "ssid": "Net", "rx-signal": "-60",
		}),
		"/interface/wireless/registration-table/print": rep(map[string]string{
			"interface": "wlan1", "mac-address": "00:00:5E:00:53:A2", "signal-strength": "-70",
		}),
	}}
	regs, err := GetRegistrations(f, RegMenuWifi)
	if err != nil || len(regs) != 1 || regs[0].Source != "wifi-v2" || regs[0].Signal != "-55 dBm" {
		t.Fatalf("wifi: %+v, %v", regs, err)
	}
	regs, err = GetRegistrations(f, RegMenuCAPsMAN)
	if err != nil || len(regs) != 1 || regs[0].Source != "capsman" || regs[0].Signal != "-60 dBm" {
		t.Fatalf("capsman: %+v, %v", regs, err)
	}
	regs, err = GetRegistrations(f, RegMenuWireless)
	if err != nil || len(regs) != 1 || regs[0].Source != "wireless" || regs[0].Signal != "-70 dBm" {
		t.Fatalf("wireless: %+v, %v", regs, err)
	}
	if _, err := GetRegistrations(f, "bogus"); err == nil {
		t.Fatal("unknown menu must fail without a device call")
	}
	if len(f.calls) != 3 {
		t.Fatalf("expected exactly one print per menu, sent %v", f.calls)
	}
	// A menu whose package is missing traps — the callers avoid it by probe.
	if _, err := GetRegistrations(&fakeRunner{}, RegMenuWireless); !IsNoSuchCommand(err) {
		t.Fatalf("missing package: %v", err)
	}
}
