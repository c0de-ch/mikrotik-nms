package devclass

import (
	"testing"
	"time"
)

func TestLiveCapsTable(t *testing.T) {
	tests := []struct {
		c    Class
		want LiveCaps
	}{
		{C1, LiveCaps{MinPeriod: 15 * time.Second, MaxIfaces: 4, Leases: 1, AdminOnly: true}},
		{C2, LiveCaps{MinPeriod: 5 * time.Second, MaxIfaces: 8, Leases: 2, FastCounters: 10 * time.Second}},
		{C3, LiveCaps{MinPeriod: time.Second, MaxIfaces: 16, Leases: 4, FastCounters: 5 * time.Second}},
		// Unprobed and invalid classes get the weakest ceilings.
		{C0Unknown, LiveCapsOf(C1)},
		{Class(""), LiveCapsOf(C1)},
		{Class("bogus"), LiveCapsOf(C1)},
	}
	for _, tt := range tests {
		if got := LiveCapsOf(tt.c); got != tt.want {
			t.Errorf("LiveCapsOf(%q) = %+v, want %+v", tt.c, got, tt.want)
		}
	}
}

// Every budgeted class must have a row: a missing one would read as the zero
// LiveCaps — no period floor, no interface or lease ceiling.
func TestLiveCapsEveryClassBounded(t *testing.T) {
	for _, c := range []Class{C1, C2, C3} {
		l := LiveCapsOf(c)
		if l.MinPeriod <= 0 || l.MaxIfaces <= 0 || l.Leases <= 0 {
			t.Errorf("%s has an unbounded live row: %+v", c, l)
		}
	}
	// Gentler classes never allow more than stronger ones.
	c1, c2, c3 := LiveCapsOf(C1), LiveCapsOf(C2), LiveCapsOf(C3)
	if c1.MinPeriod < c2.MinPeriod || c2.MinPeriod < c3.MinPeriod {
		t.Errorf("min periods not monotonic: %v %v %v", c1.MinPeriod, c2.MinPeriod, c3.MinPeriod)
	}
	if c1.MaxIfaces > c2.MaxIfaces || c2.MaxIfaces > c3.MaxIfaces || c1.Leases > c2.Leases || c2.Leases > c3.Leases {
		t.Errorf("interface/lease ceilings not monotonic: %+v %+v %+v", c1, c2, c3)
	}
	if c1.FastCounters != 0 {
		t.Errorf("C1 must get no fast-counters lease, got %v", c1.FastCounters)
	}
}

func TestEffectiveLiveCapsLightProfile(t *testing.T) {
	if got := EffectiveLiveCaps(C3, ProfileLight); got != LiveCapsOf(C1) {
		t.Errorf("light profile on C3 = %+v, want the C1 row", got)
	}
	if got := EffectiveLiveCaps(C3, ProfileAuto); got != LiveCapsOf(C3) {
		t.Errorf("auto profile on C3 = %+v, want the C3 row", got)
	}
	if got := EffectiveLiveCaps(C2, ""); got != LiveCapsOf(C2) {
		t.Errorf("empty profile on C2 = %+v, want the C2 row", got)
	}
}

func TestLiveCapsPeriod(t *testing.T) {
	tests := []struct {
		c         Class
		requested time.Duration
		want      time.Duration
	}{
		{C1, time.Second, 15 * time.Second},
		{C1, 30 * time.Second, 30 * time.Second},
		{C2, time.Second, 5 * time.Second},
		{C3, time.Second, time.Second},
		{C3, 0, time.Second},
		{C0Unknown, time.Second, 15 * time.Second},
	}
	for _, tt := range tests {
		if got := LiveCapsOf(tt.c).Period(tt.requested); got != tt.want {
			t.Errorf("%s Period(%v) = %v, want %v", tt.c, tt.requested, got, tt.want)
		}
	}
}
