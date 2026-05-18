package poller

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

func TestProcessInterface_FirstRunSuppresses(t *testing.T) {
	m := newPortMonitor()
	settings := portMonitorDefaults

	// First poll: even an already-disabled interface should not fire.
	iface := routeros.InterfaceDetail{
		Name: "ether1", Type: "ether", Disabled: true, Running: false,
	}
	m.rememberFirstRun("dev-1")
	events, state := m.processInterface("dev-1", iface, settings, time.Now())
	if len(events) != 0 {
		t.Fatalf("first-run should suppress events, got %d", len(events))
	}
	if state == nil || state.InterfaceName != "ether1" {
		t.Fatalf("expected state for ether1, got %+v", state)
	}
	m.markDeviceProcessed("dev-1")
}

func TestProcessInterface_LinkDownEvent(t *testing.T) {
	m := newPortMonitor()
	settings := portMonitorDefaults
	now := time.Now()

	// Cycle 1: link is up, not disabled.
	m.rememberFirstRun("dev-1")
	m.processInterface("dev-1", routeros.InterfaceDetail{
		Name: "ether1", Running: true, Disabled: false,
	}, settings, now)
	m.markDeviceProcessed("dev-1")

	// Cycle 2: link goes down.
	events, _ := m.processInterface("dev-1", routeros.InterfaceDetail{
		Name: "ether1", Running: false, Disabled: false,
	}, settings, now.Add(60*time.Second))

	if len(events) != 1 {
		t.Fatalf("expected 1 link_down event, got %d: %+v", len(events), events)
	}
	if events[0].Kind != "port_link_down" {
		t.Fatalf("expected port_link_down, got %s", events[0].Kind)
	}
}

func TestProcessInterface_AdminDisabledEvent(t *testing.T) {
	m := newPortMonitor()
	settings := portMonitorDefaults
	now := time.Now()

	m.rememberFirstRun("dev-1")
	m.processInterface("dev-1", routeros.InterfaceDetail{
		Name: "ether1", Running: true, Disabled: false,
	}, settings, now)
	m.markDeviceProcessed("dev-1")

	events, _ := m.processInterface("dev-1", routeros.InterfaceDetail{
		Name: "ether1", Running: false, Disabled: true,
	}, settings, now.Add(60*time.Second))

	// The transition produces both link_down and disabled. We assert disabled is present.
	gotDisabled := false
	for _, e := range events {
		if e.Kind == "port_disabled" {
			gotDisabled = true
		}
	}
	if !gotDisabled {
		t.Fatalf("expected port_disabled event, got %+v", events)
	}
}

func TestProcessInterface_FlapEvent(t *testing.T) {
	m := newPortMonitor()
	settings := PortMonitorSettings{
		Enabled:       true,
		FlapThreshold: 3,
		FlapWindow:    5 * time.Minute,
	}
	now := time.Now()

	m.rememberFirstRun("dev-1")
	// Cycle 0: up
	m.processInterface("dev-1", routeros.InterfaceDetail{Name: "ether1", Running: true}, settings, now)
	m.markDeviceProcessed("dev-1")

	// Five flaps within the window — should fire on transition #3 (count >= threshold).
	transitions := []bool{false, true, false, true, false}
	var lastEvents []portEvent
	for i, run := range transitions {
		evs, _ := m.processInterface("dev-1",
			routeros.InterfaceDetail{Name: "ether1", Running: run},
			settings,
			now.Add(time.Duration(10*(i+1))*time.Second))
		lastEvents = append(lastEvents, evs...)
	}

	gotFlap := false
	for _, e := range lastEvents {
		if e.Kind == "port_link_flap" && e.Severity == "critical" {
			gotFlap = true
			break
		}
	}
	if !gotFlap {
		t.Fatalf("expected port_link_flap critical event, got %+v", lastEvents)
	}
}

func TestProcessInterface_FlapDoesNotDoubleFire(t *testing.T) {
	m := newPortMonitor()
	settings := PortMonitorSettings{
		Enabled:       true,
		FlapThreshold: 3,
		FlapWindow:    5 * time.Minute,
	}
	now := time.Now()

	m.rememberFirstRun("dev-1")
	m.processInterface("dev-1", routeros.InterfaceDetail{Name: "ether1", Running: true}, settings, now)
	m.markDeviceProcessed("dev-1")

	flapCount := 0
	// 10 transitions in quick succession — flap should fire at most once until window resets.
	for i := 0; i < 10; i++ {
		run := i%2 == 0
		evs, _ := m.processInterface("dev-1",
			routeros.InterfaceDetail{Name: "ether1", Running: run},
			settings,
			now.Add(time.Duration(10*(i+1))*time.Second))
		for _, e := range evs {
			if e.Kind == "port_link_flap" {
				flapCount++
			}
		}
	}
	if flapCount != 1 {
		t.Fatalf("expected exactly 1 flap event despite 10 transitions, got %d", flapCount)
	}
}

func TestPruneDevices_RemovesStale(t *testing.T) {
	m := newPortMonitor()
	settings := portMonitorDefaults
	now := time.Now()

	// Populate state for two devices.
	for _, id := range []string{"dev-1", "dev-2"} {
		m.rememberFirstRun(id)
		m.processInterface(id, routeros.InterfaceDetail{Name: "ether1", Running: true}, settings, now)
		m.markDeviceProcessed(id)
	}

	if len(m.prev) != 2 {
		t.Fatalf("setup: expected 2 tracked devices, got %d", len(m.prev))
	}

	// dev-2 is no longer in the active set.
	m.pruneDevices(map[string]struct{}{"dev-1": {}})

	if _, ok := m.prev["dev-2"]; ok {
		t.Errorf("pruneDevices should have removed dev-2 from prev")
	}
	if _, ok := m.firstRun["dev-2"]; ok {
		t.Errorf("pruneDevices should have removed dev-2 from firstRun")
	}
	if _, ok := m.prev["dev-1"]; !ok {
		t.Errorf("pruneDevices should have kept dev-1")
	}
}

func TestIncludeInterface_Filter(t *testing.T) {
	cases := []struct {
		name    string
		filter  []string
		ifName  string
		ifType  string
		want    bool
	}{
		{"empty filter matches everything", nil, "veth-something", "veth", true},
		{"exact type match", []string{"ether"}, "ether1", "ether", true},
		{"name prefix match", []string{"sfp"}, "sfp-sfpplus1", "ether", true},
		{"no match", []string{"ether"}, "wireguard-tunnel", "wireguard", false},
		{"vlan match", []string{"vlan"}, "vlan100", "vlan", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := PortMonitorSettings{TypeFilter: tc.filter}
			got := s.includeInterface(routeros.InterfaceDetail{Name: tc.ifName, Type: tc.ifType})
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
