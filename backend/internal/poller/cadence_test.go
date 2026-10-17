package poller

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// stubCadence is a CadenceSource with fixed intervals per class and tier and a
// fixed set of paused devices.
type stubCadence struct {
	iv     map[devclass.Tier]time.Duration // every device
	byDev  map[string]map[devclass.Tier]time.Duration
	paused map[string]bool
}

func (s *stubCadence) Interval(dev queries.Device, tier devclass.Tier, _ time.Time) time.Duration {
	if m, ok := s.byDev[dev.ID]; ok {
		if v, ok := m[tier]; ok {
			return v
		}
	}
	return s.iv[tier]
}

func (s *stubCadence) Paused(dev queries.Device, _ time.Time) bool { return s.paused[dev.ID] }

// noJitter pins a device's phase to the middle of the jitter range, where
// devclass.NextRun is exactly end + interval.
func noJitter(string) uint64 { return 1000 }

func TestNoJitterPhase(t *testing.T) {
	end := at(12, 0, 0)
	if got := devclass.NextRun(end, time.Minute, noJitter("x")); !got.Equal(end.Add(time.Minute)) {
		t.Fatalf("noJitter: NextRun = %v, want exactly one interval later", got)
	}
}

// classDevice is a device probed as class c, on profile p, with override ov.
func classDevice(id string, c devclass.Class, p devclass.Profile, ov devclass.Class) queries.Device {
	return queries.Device{ID: id, Identity: id, Status: "online", DeviceClass: string(c), PollProfile: string(p), ClassOverride: string(ov)}
}

// The production cadence: the class plan floored by the fleet settings and
// env vars — the floor can slow a device down, never speed it past its class.
func TestClassCadenceFloors(t *testing.T) {
	db := pollerTestDB(t)
	now := at(12, 0, 0)
	cfg := &config.Config{TopologyInterval: 60 * time.Second, NetworkHealthInterval: 60 * time.Second}
	cs := NewCadence(db, cfg)

	c1 := classDevice("c1", devclass.C1, devclass.ProfileAuto, "")
	c2 := classDevice("c2", devclass.C2, devclass.ProfileAuto, "")
	c3 := classDevice("c3", devclass.C3, devclass.ProfileAuto, "")
	light := classDevice("light", devclass.C3, devclass.ProfileLight, "")
	overridden := classDevice("ov", devclass.C3, devclass.ProfileAuto, devclass.C1)
	unprobed := queries.Device{ID: "new", Status: "online"}

	tests := []struct {
		name string
		dev  queries.Device
		tier devclass.Tier
		want time.Duration
	}{
		{"C3 counters at the 15 s floor", c3, devclass.TierCounters, 15 * time.Second},
		{"C2 counters slower than the floor", c2, devclass.TierCounters, 30 * time.Second},
		{"C1 counters one a minute", c1, devclass.TierCounters, time.Minute},
		{"ethernet 5 min for all", c3, devclass.TierEthernet, 5 * time.Minute},
		{"C3 neighbors over the 60 s topology env", c3, devclass.TierNeighbors, 2 * time.Minute},
		{"C3 routes at the topology env", c3, devclass.TierRoutes, time.Minute},
		{"C1 bridge ports", c1, devclass.TierBridgePorts, 5 * time.Minute},
		{"C1 logs follow the bridge-port cadence", c1, devclass.TierLogs, 5 * time.Minute},
		{"C3 FDB at the 300 s port_hosts floor", c3, devclass.TierBridgeHosts, 5 * time.Minute},
		{"C2 FDB 10 min", c2, devclass.TierBridgeHosts, 10 * time.Minute},
		{"C3 ARP floored by client_discovery_interval", c3, devclass.TierARP, 15 * time.Minute},
		{"C1 ARP 30 min", c1, devclass.TierARP, 30 * time.Minute},
		{"C3 DHCP leases floored", c3, devclass.TierDHCPLeases, 15 * time.Minute},
		{"no DHCP leases on C2", c2, devclass.TierDHCPLeases, 0},
		{"C3 wifi registrations at the 30 s floor", c3, devclass.TierWifiRegistrations, 30 * time.Second},
		{"C1 wifi registrations 5 min", c1, devclass.TierWifiRegistrations, 5 * time.Minute},
		{"inventory hourly", c3, devclass.TierInventory, time.Hour},
		{"light profile runs the C1 plan", light, devclass.TierCounters, time.Minute},
		{"light profile drops C3-only reads", light, devclass.TierDHCPLeases, 0},
		{"a gentler override wins", overridden, devclass.TierNeighbors, 5 * time.Minute},
		{"an unprobed device is budgeted as C1", unprobed, devclass.TierCounters, time.Minute},
	}
	for _, tt := range tests {
		if got := cs.Interval(tt.dev, tt.tier, now); got != tt.want {
			t.Errorf("%s: Interval(%s) = %v, want %v", tt.name, tt.tier, got, tt.want)
		}
	}

	// Raised floors slow everyone down, the class still decides above them.
	for k, v := range map[string]string{"port_stats_interval": "45", "client_discovery_interval": "3600", "info_interval": "7200"} {
		if err := queries.SetSetting(db, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if got := cs.Interval(c3, devclass.TierCounters, now); got != 15*time.Second {
		t.Errorf("floors are cached for %v: counters = %v, want the cached 15 s", floorTTL, got)
	}
	later := now.Add(floorTTL)
	for _, tt := range []struct {
		dev  queries.Device
		tier devclass.Tier
		want time.Duration
	}{
		{c3, devclass.TierCounters, 45 * time.Second},
		{c1, devclass.TierCounters, time.Minute},
		{c1, devclass.TierARP, time.Hour},
		{c3, devclass.TierInventory, 2 * time.Hour},
	} {
		if got := cs.Interval(tt.dev, tt.tier, later); got != tt.want {
			t.Errorf("after raising the floors: %s %s = %v, want %v", tt.dev.ID, tt.tier, got, tt.want)
		}
	}
	envFloor := NewCadence(db, &config.Config{TopologyInterval: 10 * time.Minute, NetworkHealthInterval: time.Minute})
	if got := envFloor.Interval(c3, devclass.TierNeighbors, now); got != 10*time.Minute {
		t.Errorf("topology env floor 10 min: neighbors = %v", got)
	}
}

// A pause stops every scheduled tier (runInterval) without changing the
// cadence the staleness rules measure against (Interval).
func TestRunIntervalPaused(t *testing.T) {
	now := at(12, 0, 0)
	until := now.Add(time.Hour)
	dev := classDevice("sw", devclass.C2, devclass.ProfileAuto, "")
	dev.PollingPausedUntil = &until
	cs := NewCadence(nil, nil)
	for _, tier := range devclass.Tiers() {
		if got := runInterval(cs, dev, tier, now); got != 0 {
			t.Errorf("paused: runInterval(%s) = %v, want 0", tier, got)
		}
	}
	if got := cs.Interval(dev, devclass.TierCounters, now); got != 30*time.Second {
		t.Errorf("paused: Interval = %v, want the C2 cadence", got)
	}
	if got := runInterval(cs, dev, devclass.TierCounters, until); got != 30*time.Second {
		t.Errorf("at the pause end: runInterval = %v, want the cadence back", got)
	}
}

// The tier clock: due when not run since startup, then at NextRun(end,
// interval, phase) — so within ±10 % of the interval, steady per device —
// with the interval in force at the check and no catch-up; retries and
// non-running tiers are published to the registry.
func TestTierClockDueJitterRetryAndRegistry(t *testing.T) {
	reg := NewScheduleRegistry()
	k := newTierClock(reg)
	const iv = time.Minute
	now := at(12, 0, 0)

	if !k.due("sw", devclass.TierNeighbors, iv, now) {
		t.Fatal("a tier not run since startup is due")
	}
	if got := reg.NextRuns("sw")["neighbors"]; !got.Equal(now) {
		t.Errorf("registry before the first run = %v, want now", got)
	}
	end := now.Add(3 * time.Second) // the run took 3 s
	k.ran("sw", devclass.TierNeighbors, iv, end)
	next := devclass.NextRun(end, iv, devclass.Phase("sw"))
	if lo, hi := end.Add(54*time.Second), end.Add(66*time.Second); next.Before(lo) || next.After(hi) {
		t.Fatalf("next = %v outside ±10 %% of the interval after the END of the run", next)
	}
	if k.due("sw", devclass.TierNeighbors, iv, next.Add(-time.Millisecond)) {
		t.Error("due before its next run")
	}
	if !k.due("sw", devclass.TierNeighbors, iv, next) {
		t.Error("not due at its next run")
	}
	if got := reg.NextRuns("sw")["neighbors"]; !got.Equal(next) {
		t.Errorf("registry = %v, want %v", got, next)
	}
	// No catch-up: an hour late is due once, and the next run counts from
	// the end of that run.
	late := end.Add(time.Hour)
	if !k.due("sw", devclass.TierNeighbors, iv, late) {
		t.Fatal("overdue tier must be due")
	}
	k.ran("sw", devclass.TierNeighbors, iv, late)
	if k.due("sw", devclass.TierNeighbors, iv, late.Add(30*time.Second)) {
		t.Error("an overdue tier must not catch up with extra runs")
	}

	// A shorter interval (profile or override change) applies at once.
	if !k.due("sw", devclass.TierNeighbors, 10*time.Second, late.Add(30*time.Second)) {
		t.Error("an interval change must apply at the next check")
	}

	// A run that could not start is retried at the retry time.
	k.retryAt("ap", devclass.TierWifiRegistrations, now.Add(noClientRetry))
	if k.due("ap", devclass.TierWifiRegistrations, iv, now.Add(noClientRetry-time.Second)) ||
		!k.due("ap", devclass.TierWifiRegistrations, iv, now.Add(noClientRetry)) {
		t.Error("retry time not honoured")
	}

	// A tier that does not run (paused, not in the plan) is never due and
	// leaves the registry.
	if k.due("sw", devclass.TierNeighbors, 0, late.Add(time.Hour)) {
		t.Error("interval 0 must never be due")
	}
	if _, ok := reg.NextRuns("sw")["neighbors"]; ok {
		t.Error("a tier that does not run must leave the registry")
	}

	k.retain(map[string]bool{"ap": true})
	if !k.lastEnd("sw", devclass.TierNeighbors).IsZero() {
		t.Error("retain must forget other devices")
	}
}

// Fleet devices spread out: a fleet on one cadence does not run in lockstep.
func TestTierClockSpreadsDevices(t *testing.T) {
	k := newTierClock(NewScheduleRegistry())
	end := at(12, 0, 0)
	seen := map[time.Time]bool{}
	for i := 0; i < 50; i++ {
		id := "dev-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		k.ran(id, devclass.TierCounters, time.Minute, end)
		k.mu.Lock()
		seen[k.nextLocked(tierKey{id, devclass.TierCounters}, time.Minute, end)] = true
		k.mu.Unlock()
	}
	if len(seen) < 25 {
		t.Errorf("50 devices finishing together get only %d distinct next runs", len(seen))
	}
}

func TestSchedulePollingView(t *testing.T) {
	reg := NewScheduleRegistry()
	now := at(12, 0, 0)
	dev := classDevice("sw", devclass.C2, devclass.ProfileLight, "")
	reg.set("sw", devclass.TierCounters, now.Add(42*time.Second+300*time.Millisecond))
	reg.set("sw", devclass.TierBridgePorts, now.Add(90*time.Second))

	p := reg.Polling(dev, now)
	if p.Class != devclass.C2 || p.Profile != devclass.ProfileLight || p.PausedUntil != nil {
		t.Errorf("view = %+v", p)
	}
	if got := p.NextRuns["counters"]; !got.Equal(now.Add(42 * time.Second)) {
		t.Errorf("counters next = %v, want truncated to the second", got)
	}
	if len(p.NextRuns) != 2 {
		t.Errorf("next runs = %v", p.NextRuns)
	}

	until := now.Add(time.Hour)
	dev.PollingPausedUntil = &until
	p = reg.Polling(dev, now)
	if p.PausedUntil == nil || !p.PausedUntil.Equal(until) || len(p.NextRuns) != 0 {
		t.Errorf("paused view = %+v, want the pause end and no next runs", p)
	}

	reg.Retain(map[string]bool{})
	if len(reg.NextRuns("sw")) != 0 {
		t.Error("Retain must drop deleted devices")
	}
}

// A skipped paused device is logged once per pause and consumer.
func TestPauseNoticesOncePerPause(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	p := &pauseNotices{seen: make(map[string]time.Time)}
	until := at(13, 0, 0)
	dev := queries.Device{ID: "sw", Identity: "sw", PollingPausedUntil: &until}
	p.skip("firmware check", dev)
	p.skip("firmware check", dev)
	p.skip("speed tests", dev)
	later := at(18, 0, 0)
	dev.PollingPausedUntil = &later
	p.skip("firmware check", dev)
	if got := strings.Count(buf.String(), "skips sw"); got != 3 {
		t.Errorf("logged %d lines, want one per pause and consumer:\n%s", got, buf.String())
	}
}
