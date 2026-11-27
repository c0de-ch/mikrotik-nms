package devclass

import (
	"testing"
	"time"
)

func TestCadenceTable(t *testing.T) {
	tests := []struct {
		c    Class
		t    Tier
		want time.Duration
	}{
		{C1, TierCounters, 60 * time.Second},
		{C2, TierCounters, 30 * time.Second},
		{C3, TierCounters, 15 * time.Second},
		{C1, TierCPU, 60 * time.Second},
		{C3, TierCPU, 60 * time.Second},
		{C1, TierBridgePorts, 5 * time.Minute},
		{C2, TierBridgePorts, 2 * time.Minute},
		{C3, TierBridgePorts, 2 * time.Minute},
		{C1, TierLogs, 5 * time.Minute},
		{C2, TierLogs, 2 * time.Minute},
		{C1, TierBridgeVLAN, 15 * time.Minute},
		{C3, TierBridgeVLAN, 5 * time.Minute},
		{C1, TierBridgeHosts, 15 * time.Minute},
		{C2, TierBridgeHosts, 10 * time.Minute},
		{C3, TierBridgeHosts, 5 * time.Minute},
		{C2, TierRelations, 15 * time.Minute},
		{C3, TierRelations, 5 * time.Minute},
		{C1, TierNeighbors, 5 * time.Minute},
		{C3, TierNeighbors, 2 * time.Minute},
		{C1, TierRoutes, 5 * time.Minute},
		{C3, TierRoutes, 60 * time.Second},
		{C2, TierEthernet, 5 * time.Minute},
		{C1, TierARP, 30 * time.Minute},
		{C3, TierARP, 5 * time.Minute},
		{C1, TierDHCPLeases, 0},
		{C2, TierDHCPLeases, 0},
		{C3, TierDHCPLeases, 5 * time.Minute},
		{C1, TierWifiRegistrations, 5 * time.Minute},
		{C2, TierWifiRegistrations, 60 * time.Second},
		{C3, TierWifiRegistrations, 30 * time.Second},
		{C3, TierInventory, 60 * time.Minute},
		// The hygiene audit: every 6 h, whatever the class.
		{C1, TierAudit, 6 * time.Hour},
		{C2, TierAudit, 6 * time.Hour},
		{C3, TierAudit, 6 * time.Hour},
		// Unprobed and invalid classes run the C1 plan.
		{C0Unknown, TierCounters, 60 * time.Second},
		{Class("bogus"), TierBridgePorts, 5 * time.Minute},
		{C3, Tier(99), 0},
	}
	for _, tt := range tests {
		if got := Cadence(tt.c, tt.t); got != tt.want {
			t.Errorf("Cadence(%s, %s) = %v, want %v", tt.c, tt.t, got, tt.want)
		}
	}
}

func TestEveryTierHasARowPerClass(t *testing.T) {
	for _, c := range []Class{C1, C2, C3} {
		for _, tier := range Tiers() {
			if tier.String() == "unknown" {
				t.Fatalf("tier %d has no name", tier)
			}
			if tier == TierDHCPLeases && c != C3 {
				continue // deliberately not run on C1/C2
			}
			if Cadence(c, tier) <= 0 {
				t.Errorf("Cadence(%s, %s) is unset", c, tier)
			}
		}
	}
	// C1 must never be polled faster than C2, nor C2 faster than C3.
	for _, tier := range Tiers() {
		c1, c2, c3 := Cadence(C1, tier), Cadence(C2, tier), Cadence(C3, tier)
		if c3 > 0 && c2 > 0 && c2 < c3 {
			t.Errorf("%s: C2 (%v) faster than C3 (%v)", tier, c2, c3)
		}
		if c2 > 0 && c1 > 0 && c1 < c2 {
			t.Errorf("%s: C1 (%v) faster than C2 (%v)", tier, c1, c2)
		}
	}
}

func TestProfilesAndEffectiveInterval(t *testing.T) {
	for in, want := range map[string]Profile{"": ProfileAuto, "auto": ProfileAuto, " Light ": ProfileLight} {
		got, ok := ParseProfile(in)
		if !ok || got != want {
			t.Fatalf("ParseProfile(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	if _, ok := ParseProfile("heavy"); ok {
		t.Fatal("unknown profile must fail")
	}
	if Profile("").Valid() || !ProfileLight.Valid() {
		t.Fatal("Valid: empty is not a stored profile, light is")
	}
	if got := EffectiveInterval(C3, ProfileAuto, TierCounters); got != 15*time.Second {
		t.Fatalf("auto C3 counters = %v", got)
	}
	if got := EffectiveInterval(C3, ProfileLight, TierCounters); got != 60*time.Second {
		t.Fatalf("light profile must use the C1 cadence, got %v", got)
	}
	if got := EffectiveInterval(C3, ProfileLight, TierDHCPLeases); got != 0 {
		t.Fatalf("light profile drops C3-only reads, got %v", got)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	if !Due(time.Time{}, now, time.Minute) {
		t.Fatal("never run is due")
	}
	if Due(now.Add(-30*time.Second), now, time.Minute) {
		t.Fatal("30 s into a 60 s interval is not due")
	}
	if !Due(now.Add(-time.Minute), now, time.Minute) {
		t.Fatal("exactly one interval later is due")
	}
	if Due(time.Time{}, now, 0) || Due(now.Add(-time.Hour), now, -1) {
		t.Fatal("a non-positive interval is never due (the read is off)")
	}
}

func TestNextRunJitterBounds(t *testing.T) {
	end := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	interval := 60 * time.Second
	lo, hi := end.Add(54*time.Second), end.Add(66*time.Second)
	seen := map[time.Time]bool{}
	for i := 0; i < 500; i++ {
		id := "device-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		next := NextRun(end, interval, Phase(id))
		if next.Before(lo) || next.After(hi) {
			t.Fatalf("NextRun for %s = %v outside ±10%% [%v, %v]", id, next, lo, hi)
		}
		if NextRun(end, interval, Phase(id)) != next {
			t.Fatal("NextRun must be deterministic for a phase")
		}
		seen[next] = true
	}
	if len(seen) < 100 {
		t.Fatalf("jitter should spread devices out, only %d distinct offsets", len(seen))
	}
	// The extremes of the phase range hit exactly ±10 %.
	if got := NextRun(end, interval, 0); got != lo {
		t.Fatalf("phase 0 = %v, want %v", got, lo)
	}
	if got := NextRun(end, interval, jitterSteps-1); got != hi {
		t.Fatalf("phase max = %v, want %v", got, hi)
	}
	if got := NextRun(end, interval, (jitterSteps-1)/2); got != end.Add(interval) {
		t.Fatalf("middle phase has no jitter, got %v", got)
	}
	if got := NextRun(end, 0, 7); got != end {
		t.Fatalf("zero interval returns end, got %v", got)
	}
}

// The post-poll CPU sample (1c) runs every 10 min on every class.
func TestCPUPostTier(t *testing.T) {
	for _, c := range []Class{C1, C2, C3, C0Unknown} {
		if got := Cadence(c, TierCPUPost); got != 10*time.Minute {
			t.Errorf("Cadence(%s, cpu_post) = %v, want 10 min", c, got)
		}
	}
	if TierCPUPost.String() != "cpu_post" || TierCPU.String() != "cpu" {
		t.Errorf("tier names = %q / %q", TierCPU, TierCPUPost)
	}
}

// A slot grid gives every device one fixed second of every interval: the
// CPU sample of a device lands in each minute exactly once, never twice and
// never in none, and a fleet is spread over the minute instead of the
// interval drifting by its jitter.
func TestSlotAt(t *testing.T) {
	const iv = 60 * time.Second
	now := time.Date(2026, 10, 16, 12, 0, 30, 0, time.UTC)
	offsets := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		phase := Phase("device-" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)))
		slot := SlotAt(now, iv, phase)
		if slot.After(now) || now.Sub(slot) >= iv {
			t.Fatalf("SlotAt = %v, want the slot holding %v", slot, now)
		}
		off := slot.Sub(slot.Truncate(iv))
		if off < 0 || off >= 45*time.Second {
			t.Fatalf("slot offset %v outside the first three quarters of the interval", off)
		}
		if next := SlotAt(slot.Add(iv), iv, phase); !next.Equal(slot.Add(iv)) {
			t.Fatalf("the next slot = %v, want exactly one interval after %v", next, slot)
		}
		if again := SlotAt(slot.Add(iv-time.Millisecond), iv, phase); !again.Equal(slot) {
			t.Fatalf("a slot lasts until the next one starts: %v", again)
		}
		offsets[off] = true
	}
	if len(offsets) < 100 {
		t.Fatalf("slots should spread devices over the interval, only %d distinct offsets", len(offsets))
	}
	if got := SlotAt(now, 0, 7); !got.Equal(now) {
		t.Fatalf("zero interval returns t, got %v", got)
	}
}

// A tier's first read after start-up waits its lead, never more than half its
// interval or 10 minutes; the reads that must come first do not wait at all.
func TestFirstRunDelay(t *testing.T) {
	for _, tt := range []struct {
		interval, bound time.Duration
	}{
		{0, 0}, {-time.Minute, 0},
		{30 * time.Second, 15 * time.Second},
		{5 * time.Minute, 150 * time.Second},
		{15 * time.Minute, 450 * time.Second},
		{20 * time.Minute, 10 * time.Minute},
		{6 * time.Hour, 10 * time.Minute},
	} {
		if got := FirstRunBound(tt.interval); got != tt.bound {
			t.Errorf("FirstRunBound(%v) = %v, want %v", tt.interval, got, tt.bound)
		}
	}

	for _, tier := range []Tier{TierCounters, TierCPU, TierInventory, TierAudit, TierLogs, TierWifiRegistrations, TierEthernet, TierARP, TierDHCPLeases} {
		for _, c := range []Class{C1, C2, C3} {
			if got := FirstRunDelay(tier, Cadence(c, tier)); got != 0 {
				t.Errorf("FirstRunDelay(%s, %s cadence) = %v: it is read at once", tier, c, got)
			}
		}
	}
	for _, c := range []Class{C1, C2, C3} {
		for _, tier := range Tiers() {
			iv := Cadence(c, tier)
			if got := FirstRunDelay(tier, iv); got < 0 || got > FirstRunBound(iv) {
				t.Errorf("FirstRunDelay(%s, %v) = %v, outside [0, %v]", tier, iv, got, FirstRunBound(iv))
			}
		}
	}
	if got := FirstRunDelay(Tier(99), time.Hour); got != 0 {
		t.Errorf("an unknown tier waits %v", got)
	}

	// On a C1 device the first reads come one group a minute; on a strong
	// device the same leads fold into the first half-interval.
	for _, tt := range []struct {
		c    Class
		tier Tier
		want time.Duration
	}{
		{C1, TierBridgePorts, time.Minute},
		{C1, TierNeighbors, 2 * time.Minute},
		{C1, TierRoutes, 2 * time.Minute},
		{C1, TierBridgeHosts, 3 * time.Minute},
		{C1, TierBridgeVLAN, 3 * time.Minute},
		{C1, TierRelations, 4 * time.Minute},
		{C1, TierCPUPost, 4 * time.Minute},
		{C3, TierRoutes, 30 * time.Second},       // every minute: half of it
		{C3, TierNeighbors, time.Minute},         // every 2 min
		{C3, TierBridgePorts, time.Minute},       // every 2 min
		{C3, TierBridgeHosts, 150 * time.Second}, // every 5 min
		{C3, TierRelations, 150 * time.Second},
	} {
		if got := FirstRunDelay(tt.tier, Cadence(tt.c, tt.tier)); got != tt.want {
			t.Errorf("FirstRunDelay(%s) on %s = %v, want %v", tt.tier, tt.c, got, tt.want)
		}
	}
}

// The point of the leads: on the C1 plan, the 5-minute reads start in three
// different minutes of the five-minute cycle, and none of the 15-minute reads
// or the post-poll sample starts in one of those — so, every later read
// counting from the end of the one before, they do not fall together.
func TestFirstRunLeadsSpreadTheC1Plan(t *testing.T) {
	minuteOf := func(tier Tier) int {
		return int(FirstRunDelay(tier, Cadence(C1, tier)) / time.Minute)
	}
	fiveMinute := map[int][]Tier{} // minute of the five-minute cycle → the 5-minute tiers starting in it
	for _, tier := range Tiers() {
		if Cadence(C1, tier) == 5*time.Minute {
			m := minuteOf(tier) % 5
			fiveMinute[m] = append(fiveMinute[m], tier)
		}
	}
	if len(fiveMinute) < 3 {
		t.Errorf("the C1 5-minute reads start in %d minutes of five: %v", len(fiveMinute), fiveMinute)
	}
	for _, tier := range []Tier{TierBridgeHosts, TierBridgeVLAN, TierRelations, TierCPUPost} {
		if iv := Cadence(C1, tier); iv%(5*time.Minute) != 0 {
			t.Fatalf("%s runs every %v on C1, not a multiple of 5 min: rethink the leads", tier, iv)
		}
		if others := fiveMinute[minuteOf(tier)%5]; len(others) > 0 {
			t.Errorf("%s starts in minute %d, with the 5-minute reads %v", tier, minuteOf(tier), others)
		}
	}
}

func TestBudgets(t *testing.T) {
	if b := BudgetOf(C1); b.CommandsPerMin != 12 || b.BurstCommands != 6 || b.CostSecPerMin != 2.0 || b.RowsPerMin != 1500 {
		t.Fatalf("C1 budget %+v", b)
	}
	if b := BudgetOf(C3); b.CommandsPerMin != 120 || b.BurstCommands != 30 {
		t.Fatalf("C3 budget %+v", b)
	}
	if BudgetOf(C0Unknown) != BudgetOf(C1) || BudgetOf("x") != BudgetOf(C1) {
		t.Fatal("unprobed and invalid classes get the C1 budget")
	}
}
