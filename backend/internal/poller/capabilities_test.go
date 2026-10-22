package poller

import (
	"errors"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

func TestProbeCapabilitiesClassifiesMenus(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		routeros.WifiProbeCommand:       {done: map[string]string{"ret": "0"}},
		routeros.WirelessProbeCommand:   {err: trapErr("not enough permissions (9)")},
		routeros.DHCPServerProbeCommand: {done: map[string]string{"ret": "1"}},
		routeros.BridgeProbeCommand:     {done: map[string]string{"ret": "1"}},
		// caps-man: not in byCmd → "no such command prefix"
	}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	caps := probeCapabilities(f, "7.24.5", now)
	if caps.Wifi != CapPresent || caps.CAPsMAN != CapAbsent || caps.Wireless != CapPermissionMissing ||
		caps.DHCPServer != CapPresent || caps.Bridge != CapPresent {
		t.Fatalf("caps = %+v", caps)
	}
	if menus := caps.RegistrationMenus(); len(menus) != 1 || menus[0] != routeros.RegMenuWifi || !caps.HasWifiRole() {
		t.Fatalf("menus = %v", menus)
	}
	if !caps.complete() || !caps.needsPermissionRetry() {
		t.Fatalf("complete=%v permissionRetry=%v", caps.complete(), caps.needsPermissionRetry())
	}
	for _, c := range f.calls {
		if !endsWith(c, "=count-only=") {
			t.Fatalf("probe %q is not count-only", c)
		}
	}

	// A switch: no wireless package at all.
	sw := probeCapabilities(&fakeRunner{byCmd: map[string]fakeReply{
		routeros.BridgeProbeCommand: {done: map[string]string{"ret": "1"}},
	}}, "7.24.5", now)
	if sw.HasWifiRole() || sw.Bridge != CapPresent {
		t.Fatalf("switch caps = %+v", sw)
	}

	// A transport failure stops the probe: the rest stays unknown.
	dead := &fakeRunner{queue: []fakeReply{{done: map[string]string{"ret": "0"}}, {err: errors.New("i/o timeout")}}}
	part := probeCapabilities(dead, "7.24.5", now)
	if part.Wifi != CapPresent || part.CAPsMAN != CapUnknown || part.Bridge != CapUnknown || part.complete() || len(dead.calls) != 2 {
		t.Fatalf("partial = %+v calls=%v", part, dead.calls)
	}
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func TestProbeDue(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	full := Caps{Wifi: CapPresent, CAPsMAN: CapAbsent, Wireless: CapAbsent, DHCPServer: CapAbsent, Bridge: CapPresent,
		ProbedAt: now, Version: "7.24.5"}
	if !probeDue(Caps{}, "7.24.5", now) {
		t.Fatal("never probed must be due")
	}
	if probeDue(full, "7.24.5", now.Add(time.Hour)) {
		t.Fatal("a complete probe is good for a day")
	}
	if !probeDue(full, "7.24.5", now.Add(capProbeEvery)) {
		t.Fatal("due after a day")
	}
	if !probeDue(full, "7.25.1", now.Add(time.Minute)) {
		t.Fatal("a version change re-probes at once")
	}
	perm := full
	perm.Wireless = CapPermissionMissing
	if probeDue(perm, "7.24.5", now.Add(30*time.Minute)) || !probeDue(perm, "7.24.5", now.Add(capPermissionRetry)) {
		t.Fatal("permission traps retry hourly")
	}
	part := full
	part.Bridge = CapUnknown
	if probeDue(part, "7.24.5", now.Add(10*time.Second)) || !probeDue(part, "7.24.5", now.Add(capIncompleteRetry)) {
		t.Fatal("an incomplete probe retries after a minute")
	}
}

func TestCapabilityCacheEnsureMergesPartialProbes(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCapabilityCache()
	ok := map[string]fakeReply{
		routeros.WifiProbeCommand:       {done: map[string]string{"ret": "2"}},
		routeros.DHCPServerProbeCommand: {done: map[string]string{"ret": "1"}},
		routeros.BridgeProbeCommand:     {done: map[string]string{"ret": "1"}},
	}
	f := &fakeRunner{byCmd: ok}
	first := c.Ensure("d1", "7.24.5", f, now)
	if !first.complete() || !first.HasWifiRole() || len(f.calls) != 5 {
		t.Fatalf("first = %+v calls=%d", first, len(f.calls))
	}
	// Cached: no device calls within the day.
	c.Ensure("d1", "7.24.5", f, now.Add(time.Hour))
	if len(f.calls) != 5 {
		t.Fatalf("cached probe still sent %d calls", len(f.calls))
	}
	// Version change: re-probe; the device dies mid-probe, the old answers
	// are kept for the menus left unknown and a retry is due in a minute.
	dead := &fakeRunner{queue: []fakeReply{{done: map[string]string{"ret": "2"}}, {err: errors.New("EOF")}}}
	second := c.Ensure("d1", "7.25.1", dead, now.Add(2*time.Hour))
	if second.Wifi != CapPresent || second.Bridge != CapPresent || second.DHCPServer != CapPresent || second.CAPsMAN != CapAbsent {
		t.Fatalf("merged = %+v", second)
	}
	got, _ := c.Get("d1")
	if got.Version != "7.25.1" || !got.Partial || probeDue(got, "7.25.1", now.Add(2*time.Hour+10*time.Second)) ||
		!probeDue(got, "7.25.1", now.Add(2*time.Hour+capIncompleteRetry)) {
		t.Fatalf("cached after partial = %+v", got)
	}
	// The retry completes and clears the partial mark.
	third := c.Ensure("d1", "7.25.1", &fakeRunner{byCmd: ok}, now.Add(2*time.Hour+capIncompleteRetry))
	if third.Partial || !third.complete() || probeDue(third, "7.25.1", now.Add(3*time.Hour)) {
		t.Fatalf("after retry = %+v", third)
	}
	c.Forget("d1")
	if _, ok := c.Get("d1"); ok {
		t.Fatal("Forget")
	}
}

// A registration read that traps on a menu the probe found present — the
// legacy wireless package swapped for wifi, same RouterOS version — marks the
// menu gone and has the device re-probed within a minute, which finds the new
// menu; a transport error changes nothing.
func TestCapabilityCacheReadTrapReprobes(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewCapabilityCache()
	legacy := &fakeRunner{byCmd: map[string]fakeReply{
		routeros.WirelessProbeCommand:   {done: map[string]string{"ret": "1"}},
		routeros.DHCPServerProbeCommand: {done: map[string]string{"ret": "0"}},
		routeros.BridgeProbeCommand:     {done: map[string]string{"ret": "1"}},
	}}
	caps := c.Ensure("ap", "7.24.5", legacy, now)
	if menus := caps.RegistrationMenus(); len(menus) != 1 || menus[0] != routeros.RegMenuWireless {
		t.Fatalf("menus = %v", menus)
	}
	if c.NoteReadTrap("ap", routeros.RegMenuWireless, errors.New("i/o timeout"), now.Add(time.Hour)) {
		t.Fatal("a transport error must not change the cache")
	}
	if !c.NoteReadTrap("ap", routeros.RegMenuWireless, trapErr("no such command prefix"), now.Add(time.Hour)) {
		t.Fatal("the trap did not reach the cache")
	}
	got, _ := c.Get("ap")
	if got.HasWifiRole() || probeDue(got, "7.24.5", now.Add(time.Hour+10*time.Second)) || !probeDue(got, "7.24.5", now.Add(time.Hour+capIncompleteRetry)) {
		t.Fatalf("after the trap: %+v, want the menu gone and a re-probe due in a minute", got)
	}
	swapped := &fakeRunner{byCmd: map[string]fakeReply{
		routeros.WifiProbeCommand:       {done: map[string]string{"ret": "1"}},
		routeros.DHCPServerProbeCommand: {done: map[string]string{"ret": "0"}},
		routeros.BridgeProbeCommand:     {done: map[string]string{"ret": "1"}},
	}}
	caps = c.Ensure("ap", "7.24.5", swapped, now.Add(time.Hour+capIncompleteRetry))
	if menus := caps.RegistrationMenus(); len(menus) != 1 || menus[0] != routeros.RegMenuWifi || caps.Partial {
		t.Fatalf("after the re-probe: %+v, want the wifi menu only", caps)
	}
	if !c.NoteReadTrap("ap", routeros.RegMenuWifi, trapErr("not enough permissions (9)"), now.Add(2*time.Hour)) {
		t.Fatal("permission trap ignored")
	}
	if got, _ := c.Get("ap"); got.Wifi != CapPermissionMissing {
		t.Fatalf("after a permission trap: %+v", got)
	}
}
