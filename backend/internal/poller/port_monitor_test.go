package poller

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

// ifRow wraps a Tier 0 row as the snapshot row the port monitor reads.
func ifRow(s routeros.InterfaceStats) routeros.InterfaceSnapshot {
	return routeros.InterfaceSnapshot{InterfaceStats: s}
}

// linkRow is a running ether port whose link-downs counter reads downs.
func linkRow(name string, running bool, downs int64) routeros.InterfaceSnapshot {
	return ifRow(routeros.InterfaceStats{Name: name, Type: "ether", Running: running, LinkDowns: downs, HasLinkDowns: true})
}

func countKind(events []portEvent, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestProcessInterface_FirstRunSuppresses(t *testing.T) {
	m := newPortMonitor()
	settings := portMonitorDefaults

	// First poll: even an already-disabled interface should not fire.
	iface := ifRow(routeros.InterfaceStats{
		Name: "ether1", Type: "ether", Disabled: true, Running: false,
	})
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
	m.processInterface("dev-1", ifRow(routeros.InterfaceStats{
		Name: "ether1", Running: true, Disabled: false,
	}), settings, now)
	m.markDeviceProcessed("dev-1")

	// Cycle 2: link goes down.
	events, _ := m.processInterface("dev-1", ifRow(routeros.InterfaceStats{
		Name: "ether1", Running: false, Disabled: false,
	}), settings, now.Add(60*time.Second))

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
	m.processInterface("dev-1", ifRow(routeros.InterfaceStats{
		Name: "ether1", Running: true, Disabled: false,
	}), settings, now)
	m.markDeviceProcessed("dev-1")

	events, _ := m.processInterface("dev-1", ifRow(routeros.InterfaceStats{
		Name: "ether1", Running: false, Disabled: true,
	}), settings, now.Add(60*time.Second))

	// The transition produces both link_down and disabled. We assert disabled is present.
	if countKind(events, "port_disabled") == 0 {
		t.Fatalf("expected port_disabled event, got %+v", events)
	}
}

// Rows without a link-downs counter fall back to running transitions: each
// up → down is one flap.
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
	m.processInterface("dev-1", ifRow(routeros.InterfaceStats{Name: "ether1", Running: true}), settings, now)
	m.markDeviceProcessed("dev-1")

	// Three link losses within the window — fires on the third.
	transitions := []bool{false, true, false, true, false}
	var lastEvents []portEvent
	for i, run := range transitions {
		evs, _ := m.processInterface("dev-1",
			ifRow(routeros.InterfaceStats{Name: "ether1", Running: run}),
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
	m.processInterface("dev-1", ifRow(routeros.InterfaceStats{Name: "ether1", Running: true}), settings, now)
	m.markDeviceProcessed("dev-1")

	flapCount := 0
	// 10 transitions in quick succession — flap should fire at most once until window resets.
	for i := 0; i < 10; i++ {
		run := i%2 == 0
		evs, _ := m.processInterface("dev-1",
			ifRow(routeros.InterfaceStats{Name: "ether1", Running: run}),
			settings,
			now.Add(time.Duration(10*(i+1))*time.Second))
		flapCount += countKind(evs, "port_link_flap")
	}
	if flapCount != 1 {
		t.Fatalf("expected exactly 1 flap event despite 10 transitions, got %d", flapCount)
	}
}

// Flaps come from link-downs deltas between consecutive snapshots — which
// also counts flaps that came and went between two reads (the port reads
// running both times).
func TestProcessInterface_LinkDownsDeltaFlaps(t *testing.T) {
	m := newPortMonitor()
	settings := PortMonitorSettings{Enabled: true, FlapThreshold: 3, FlapWindow: 5 * time.Minute}
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	m.rememberFirstRun("dev-1")
	if evs, st := m.processInterface("dev-1", linkRow("ether1", true, 5), settings, t0); len(evs) != 0 || st.FlapCountWindow != 0 {
		t.Fatalf("baseline read: events %+v, flaps %d", evs, st.FlapCountWindow)
	}
	m.markDeviceProcessed("dev-1")

	evs, st := m.processInterface("dev-1", linkRow("ether1", true, 6), settings, t0.Add(15*time.Second))
	if len(evs) != 0 || st.FlapCountWindow != 1 {
		t.Fatalf("one link-down: events %+v, flaps %d", evs, st.FlapCountWindow)
	}
	evs, st = m.processInterface("dev-1", linkRow("ether1", true, 8), settings, t0.Add(30*time.Second))
	if countKind(evs, "port_link_flap") != 1 || st.FlapCountWindow != 3 {
		t.Fatalf("third flap must fire once: events %+v, flaps %d", evs, st.FlapCountWindow)
	}
	if countKind(evs, "port_link_down") != 0 {
		t.Errorf("a port that reads running must not report link down: %+v", evs)
	}

	// The same snapshot seen again (network health runs slower than Tier 0)
	// adds nothing.
	evs, st = m.processInterface("dev-1", linkRow("ether1", true, 8), settings, t0.Add(30*time.Second))
	if len(evs) != 0 || st.FlapCountWindow != 3 {
		t.Fatalf("re-read snapshot: events %+v, flaps %d", evs, st.FlapCountWindow)
	}

	// The window ages out: the count drops, and the next storm fires again.
	_, st = m.processInterface("dev-1", linkRow("ether1", true, 8), settings, t0.Add(6*time.Minute))
	if st.FlapCountWindow != 0 {
		t.Fatalf("flaps after the window = %d, want 0", st.FlapCountWindow)
	}
	evs, _ = m.processInterface("dev-1", linkRow("ether1", true, 11), settings, t0.Add(6*time.Minute+15*time.Second))
	if countKind(evs, "port_link_flap") != 1 {
		t.Fatalf("a new storm after the window must fire again: %+v", evs)
	}
}

// Link-downs counted across a gap longer than the flap window — a polling
// pause, an outage — cannot be placed in any window: they re-baseline the
// counter instead of firing a critical flap for hours of history.
func TestProcessInterface_LinkDownsAcrossAGapAreNotFlaps(t *testing.T) {
	m := newPortMonitor()
	settings := PortMonitorSettings{Enabled: true, FlapThreshold: 3, FlapWindow: 5 * time.Minute}
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	m.rememberFirstRun("sw")
	m.processInterface("sw", linkRow("ether2", true, 10), settings, t0)
	m.markDeviceProcessed("sw")
	m.processInterface("sw", linkRow("ether2", true, 10), settings, t0.Add(time.Minute))

	evs, st := m.processInterface("sw", linkRow("ether2", true, 13), settings, t0.Add(6*time.Hour+time.Minute))
	if countKind(evs, "port_link_flap") != 0 || st.FlapCountWindow != 0 {
		t.Fatalf("after a 6 h gap: events %+v, flaps %d — want none", evs, st.FlapCountWindow)
	}
	// Counting resumes from the new baseline.
	_, st = m.processInterface("sw", linkRow("ether2", true, 14), settings, t0.Add(6*time.Hour+2*time.Minute))
	if st.FlapCountWindow != 1 {
		t.Fatalf("flaps after the gap = %d, want 1 from the new baseline", st.FlapCountWindow)
	}
}

// A counter that went backwards (reboot) counts as 0 and re-baselines.
func TestProcessInterface_LinkDownsCounterReset(t *testing.T) {
	m := newPortMonitor()
	settings := PortMonitorSettings{Enabled: true, FlapThreshold: 2, FlapWindow: 5 * time.Minute}
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	m.rememberFirstRun("dev-1")
	m.processInterface("dev-1", linkRow("ether1", true, 40), settings, t0)
	m.markDeviceProcessed("dev-1")

	evs, st := m.processInterface("dev-1", linkRow("ether1", true, 1), settings, t0.Add(15*time.Second))
	if len(evs) != 0 || st.FlapCountWindow != 0 {
		t.Fatalf("reset: events %+v, flaps %d, want none", evs, st.FlapCountWindow)
	}
	evs, st = m.processInterface("dev-1", linkRow("ether1", true, 2), settings, t0.Add(30*time.Second))
	if len(evs) != 0 || st.FlapCountWindow != 1 {
		t.Fatalf("delta from the new baseline: events %+v, flaps %d, want 1 flap", evs, st.FlapCountWindow)
	}
}

// After a restart the counter has no baseline: the first read sets it, and
// only a visible link loss counts until then.
func TestProcessInterface_LinkDownsBaselineAfterRestart(t *testing.T) {
	m := newPortMonitor()
	m.prev["dev-1"] = map[string]*portState{"ether1": {running: true}} // as RestoreFromDB leaves it
	settings := PortMonitorSettings{Enabled: true, FlapThreshold: 2, FlapWindow: 5 * time.Minute}
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	m.rememberFirstRun("dev-1") // restored: not a first run
	_, st := m.processInterface("dev-1", linkRow("ether1", true, 900), settings, t0)
	if st.FlapCountWindow != 0 {
		t.Fatalf("first read after restart counted %d flaps from a lifetime counter", st.FlapCountWindow)
	}
	m.markDeviceProcessed("dev-1")
	_, st = m.processInterface("dev-1", linkRow("ether1", true, 901), settings, t0.Add(15*time.Second))
	if st.FlapCountWindow != 1 {
		t.Fatalf("flaps = %d, want 1 once baselined", st.FlapCountWindow)
	}
}

// loop-protect state comes from the merged Tier 4a row; a row without one
// keeps the last known status, so a restart cannot re-fire a known loop.
func TestProcessInterface_LoopProtectFromEthernet(t *testing.T) {
	m := newPortMonitor()
	m.prev["dev-1"] = map[string]*portState{"ether3": {running: true, loopProtectStatus: "on-loop"}}
	settings := portMonitorDefaults
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	noEth := ifRow(routeros.InterfaceStats{Name: "ether3", Type: "ether", Running: true})
	withEth := func(status string) routeros.InterfaceSnapshot {
		r := noEth
		r.Ethernet, r.HasEthernet = routeros.EthernetStats{Name: "ether3", LoopProtectStatus: status}, true
		return r
	}

	m.rememberFirstRun("dev-1")
	evs, st := m.processInterface("dev-1", noEth, settings, t0)
	m.markDeviceProcessed("dev-1")
	if len(evs) != 0 || st.LoopProtectStatus != "on-loop" {
		t.Fatalf("no ethernet row yet: events %+v, status %q, want the restored on-loop kept", evs, st.LoopProtectStatus)
	}
	if evs, _ := m.processInterface("dev-1", withEth("on-loop"), settings, t0.Add(time.Minute)); countKind(evs, "port_loop_protect") != 0 {
		t.Fatalf("a loop already known must not re-fire: %+v", evs)
	}
	if _, st := m.processInterface("dev-1", withEth("on"), settings, t0.Add(2*time.Minute)); st.LoopProtectStatus != "on" {
		t.Fatalf("status = %q, want on", st.LoopProtectStatus)
	}
	evs, st = m.processInterface("dev-1", withEth("on-loop"), settings, t0.Add(3*time.Minute))
	if countKind(evs, "port_loop_protect") != 1 || st.LoopProtectStatus != "on-loop" {
		t.Fatalf("loop-protect trip: events %+v, status %q", evs, st.LoopProtectStatus)
	}
}

func TestPruneDevices_RemovesStale(t *testing.T) {
	m := newPortMonitor()
	settings := portMonitorDefaults
	now := time.Now()

	// Populate state for two devices.
	for _, id := range []string{"dev-1", "dev-2"} {
		m.rememberFirstRun(id)
		m.processInterface(id, ifRow(routeros.InterfaceStats{Name: "ether1", Running: true}), settings, now)
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
		name   string
		filter []string
		ifName string
		ifType string
		want   bool
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
			got := s.includeInterface(ifRow(routeros.InterfaceStats{Name: tc.ifName, Type: tc.ifType}))
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
