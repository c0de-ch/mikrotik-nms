package poller

import (
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// The hysteresis: the first classification applies at once; a change only
// when a probe at least 24 h after the first disagreeing one still agrees
// with it, every probe in between agreeing too; a probe without facts says
// nothing.
func TestClassGateHysteresis(t *testing.T) {
	t0 := at(12, 0, 0)
	steps := []struct {
		name           string
		at             time.Duration
		current, probe devclass.Class
		want           classVerdict
		started        bool
	}{
		{"first classification is immediate", 0, devclass.C0Unknown, devclass.C3, classApply, false},
		{"an agreeing probe confirms", time.Hour, devclass.C3, devclass.C3, classConfirm, false},
		{"a change starts a candidate", 2 * time.Hour, devclass.C3, devclass.C2, classHold, true},
		{"the candidate holds under 24 h", 25 * time.Hour, devclass.C3, devclass.C2, classHold, false},
		{"a probe without facts says nothing", 25*time.Hour + time.Minute, devclass.C3, devclass.C0Unknown, classHold, false},
		{"24 h after the first: applied", 26 * time.Hour, devclass.C3, devclass.C2, classApply, false},
		{"the next change starts over", 27 * time.Hour, devclass.C2, devclass.C1, classHold, true},
		{"a probe back on the stored class drops it", 30 * time.Hour, devclass.C2, devclass.C2, classConfirm, false},
		{"so 24 h after the first C1 is not enough", 51 * time.Hour, devclass.C2, devclass.C1, classHold, true},
		{"a different candidate restarts the clock", 60 * time.Hour, devclass.C2, devclass.C3, classHold, true},
		{"…and needs its own 24 h", 75*time.Hour + time.Minute, devclass.C2, devclass.C3, classHold, false},
		{"…which it gets", 84 * time.Hour, devclass.C2, devclass.C3, classApply, false},
	}
	g := newClassGate()
	for _, st := range steps {
		got, started := g.judge("sw", st.current, st.probe, t0.Add(st.at))
		if got != st.want || (started != nil) != st.started {
			t.Errorf("%s: verdict %d (started %v), want %d (started %v)", st.name, got, started != nil, st.want, st.started)
		}
	}
	g.forget(map[string]bool{})
	if len(g.pending) != 0 {
		t.Error("forget must drop deleted devices' candidates")
	}
}

// probeClass stores the class: the first one at once with an audit row, a
// change only after the hysteresis; the facts carry the highest frequency
// ever seen, so a frequency-scaled print of an unknown board is not demoted.
func TestProbeClassStoresThroughHysteresis(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "r", Identity: "r", Address: "192.0.2.60"})
	m := &Manager{db: db, classes: newClassGate()}
	dev := func() queries.Device {
		d, err := queries.GetDevice(db, "r")
		if err != nil {
			t.Fatal(err)
		}
		return *d
	}
	// An unknown arm64 board: 2 cores, so the frequency decides C2 / C3.
	print := func(mhz string) map[string]string {
		return map[string]string{"architecture-name": "arm64", "board-name": "Mystery", "cpu-count": "2",
			"cpu-frequency": mhz, "total-memory": "1073741824"}
	}
	t0 := at(12, 0, 0)

	m.probeClass(dev(), print("1200"), t0)
	if d := dev(); d.DeviceClass != "C3" || d.CPUMHzMax == nil || *d.CPUMHzMax != 1200 {
		t.Fatalf("first probe: class %q, max MHz %v", d.DeviceClass, d.CPUMHzMax)
	}
	// Scaled down to 600 MHz: the max seen keeps it C3, no candidate.
	m.probeClass(dev(), print("600"), t0.Add(time.Hour))
	if d := dev(); d.DeviceClass != "C3" || len(m.classes.pending) != 0 {
		t.Fatalf("scaled print: class %q, pending %v", d.DeviceClass, m.classes.pending)
	}

	// The device really became weaker (1 GHz-class core, say a VM resize):
	// the max seen no longer applies, so feed a print that classifies C1.
	weak := map[string]string{"architecture-name": "mipsbe", "board-name": "Mystery", "cpu-count": "1", "total-memory": "67108864"}
	m.probeClass(dev(), weak, t0.Add(2*time.Hour))
	if d := dev(); d.DeviceClass != "C3" {
		t.Fatalf("a change applied without the hysteresis: %q", d.DeviceClass)
	}
	m.probeClass(dev(), weak, t0.Add(26*time.Hour))
	if d := dev(); d.DeviceClass != "C1" || d.ClassProbedAt == nil || !d.ClassProbedAt.Equal(t0.Add(26*time.Hour)) {
		t.Fatalf("after 24 h of agreeing probes: class %q probed %v", d.DeviceClass, d.ClassProbedAt)
	}

	audit, err := queries.ListAuditLog(db, "r", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 2 || audit[0].Action != "device_class_probed" || audit[0].Username != "" ||
		!strings.HasPrefix(audit[0].Detail, "C3 → C1 (") || !strings.HasPrefix(audit[1].Detail, "C0 → C3 (") {
		t.Errorf("audit = %+v, want the two class changes by the system", audit)
	}
}
