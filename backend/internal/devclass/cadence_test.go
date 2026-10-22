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
