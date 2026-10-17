package queries

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

func seedDevice(t *testing.T, db *sql.DB, addr string) *Device {
	t.Helper()
	d := &Device{ID: uuid.NewString(), Address: addr, Identity: "dev-" + addr, Username: "u", PasswordEnc: "p", APIPort: 8728, Status: "unknown", Tags: "[]"}
	if err := CreateDevice(db, d); err != nil {
		t.Fatalf("create device: %v", err)
	}
	return d
}

func TestDeviceClassDefaultsAndJSON(t *testing.T) {
	db := testDB(t)
	seeded := seedDevice(t, db, "10.0.0.1")

	d, err := GetDevice(db, seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.DeviceClass != "" || d.ClassOverride != "" || d.PollProfile != "auto" || d.PollingPausedUntil != nil || d.CPUCount != nil {
		t.Fatalf("fresh device polling fields: %+v", d)
	}
	if d.ProbedClass() != devclass.C0Unknown || d.EffectiveClass() != devclass.C1 || d.Profile() != devclass.ProfileAuto {
		t.Fatalf("unprobed device must be budgeted as C1 on auto: probed=%s eff=%s profile=%s", d.ProbedClass(), d.EffectiveClass(), d.Profile())
	}
	if d.PollingPaused(time.Now()) {
		t.Fatal("not paused")
	}

	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["effective_class"] != "C1" || m["polling_paused"] != false || m["poll_profile"] != "auto" || m["device_class"] != "" {
		t.Fatalf("json: %s", b)
	}
	if _, leaked := m["PasswordEnc"]; leaked {
		t.Fatalf("password must stay out of JSON: %s", b)
	}
	if _, leaked := m["password_enc"]; leaked {
		t.Fatalf("password must stay out of JSON: %s", b)
	}
	// Pointer and value marshal the same way (value receiver).
	b2, _ := json.Marshal(*d)
	if string(b) != string(b2) {
		t.Fatal("value and pointer JSON differ")
	}
}

func TestSetDeviceClassKeepsMaxFrequency(t *testing.T) {
	db := testDB(t)
	seeded := seedDevice(t, db, "10.0.0.2")
	probed := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	facts := devclass.ResourceFacts{Architecture: "arm64", CPUCount: 4, CPUFrequencyMHz: 1400, BoardName: "RB5009UG+S+"}
	if err := SetDeviceClass(db, seeded.ID, devclass.Classify(facts), facts, probed); err != nil {
		t.Fatal(err)
	}
	d, _ := GetDevice(db, seeded.ID)
	if d.DeviceClass != "C3" || d.CPUCount == nil || *d.CPUCount != 4 || d.CPUMHzMax == nil || *d.CPUMHzMax != 1400 {
		t.Fatalf("after probe: %+v", d)
	}
	if d.ClassProbedAt == nil || !d.ClassProbedAt.Equal(probed) {
		t.Fatalf("class_probed_at = %v", d.ClassProbedAt)
	}

	// A later probe under frequency scaling must not lower the maximum, and
	// a missing cpu-count must not blank the cached one.
	scaled := devclass.ResourceFacts{Architecture: "arm64", CPUFrequencyMHz: 600, BoardName: "RB5009UG+S+"}
	if err := SetDeviceClass(db, seeded.ID, devclass.C3, scaled, probed.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	d, _ = GetDevice(db, seeded.ID)
	if *d.CPUMHzMax != 1400 || *d.CPUCount != 4 {
		t.Fatalf("max frequency / cpu count not kept: %+v", d)
	}
}

func TestPollingControls(t *testing.T) {
	db := testDB(t)
	seeded := seedDevice(t, db, "10.0.0.3")
	now := time.Now()

	until := now.Add(24 * time.Hour).Truncate(time.Second)
	if err := PauseDevicePolling(db, seeded.ID, until); err != nil {
		t.Fatal(err)
	}
	d, _ := GetDevice(db, seeded.ID)
	if !d.PollingPaused(now) || d.PollingPausedUntil == nil || !d.PollingPausedUntil.Equal(until) {
		t.Fatalf("paused until %v, want %v", d.PollingPausedUntil, until)
	}
	if d.PollingPaused(until.Add(time.Second)) {
		t.Fatal("a pause expires on its own")
	}
	if err := ResumeDevicePolling(db, seeded.ID); err != nil {
		t.Fatal(err)
	}
	d, _ = GetDevice(db, seeded.ID)
	if d.PollingPausedUntil != nil {
		t.Fatal("resume must clear the pause")
	}

	if err := SetDevicePollProfile(db, seeded.ID, devclass.ProfileLight); err != nil {
		t.Fatal(err)
	}
	if err := SetDeviceClassOverride(db, seeded.ID, devclass.C1); err != nil {
		t.Fatal(err)
	}
	facts := devclass.ResourceFacts{Architecture: "arm", CPUCount: 1, CPUFrequencyMHz: 800, TotalMemoryBytes: 512 << 20}
	if err := SetDeviceClass(db, seeded.ID, devclass.Classify(facts), facts, now); err != nil {
		t.Fatal(err)
	}
	d, _ = GetDevice(db, seeded.ID)
	if d.Profile() != devclass.ProfileLight || d.ProbedClass() != devclass.C2 || d.EffectiveClass() != devclass.C1 {
		t.Fatalf("profile=%s probed=%s effective=%s", d.Profile(), d.ProbedClass(), d.EffectiveClass())
	}
	// A stored override that is not gentler is ignored at read time.
	if err := SetDeviceClassOverride(db, seeded.ID, devclass.C3); err != nil {
		t.Fatal(err)
	}
	d, _ = GetDevice(db, seeded.ID)
	if d.EffectiveClass() != devclass.C2 {
		t.Fatalf("a more capable override must be clamped to the probed class, got %s", d.EffectiveClass())
	}
	if err := SetDeviceClassOverride(db, seeded.ID, ""); err != nil {
		t.Fatal(err)
	}
	d, _ = GetDevice(db, seeded.ID)
	if d.ClassOverride != "" {
		t.Fatal("empty override must clear the column")
	}
	// Listing reads the same columns.
	all, err := ListDevices(db)
	if err != nil || len(all) != 1 || all[0].Profile() != devclass.ProfileLight {
		t.Fatalf("list: %v %+v", err, all)
	}
}

func TestAuditLog(t *testing.T) {
	db := testDB(t)
	dev := seedDevice(t, db, "10.0.0.4")
	other := seedDevice(t, db, "10.0.0.5")

	for i, e := range []AuditEntry{
		{UserID: "u1", Username: "alice", Action: "polling_paused", DeviceID: dev.ID, Detail: "24 h"},
		{UserID: "u1", Username: "alice", Action: "polling_resumed", DeviceID: dev.ID},
		{UserID: "u2", Username: "bob", Action: "poll_profile_changed", DeviceID: other.ID, Detail: "auto → light"},
	} {
		e.At = time.Date(2026, 10, 16, 12, i, 0, 0, time.UTC)
		if _, err := InsertAuditLog(db, &e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListAuditLog(db, dev.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "polling_resumed" || got[1].Action != "polling_paused" || got[1].Detail != "24 h" || got[0].Username != "alice" {
		t.Fatalf("device audit (newest first): %+v", got)
	}
	all, err := ListAuditLog(db, "", 2)
	if err != nil || len(all) != 2 || all[0].DeviceID != other.ID {
		t.Fatalf("global audit with limit: %v %+v", err, all)
	}
	empty, err := ListAuditLog(db, "nope", 10)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("unknown device returns an empty (non-nil) list: %v %#v", err, empty)
	}
	// A zero At is stamped with now.
	id, err := InsertAuditLog(db, &AuditEntry{Action: "x"})
	if err != nil || id == 0 {
		t.Fatal(err)
	}
	rows, _ := ListAuditLog(db, "", 1)
	if rows[0].At.IsZero() || time.Since(rows[0].At) > time.Minute {
		t.Fatalf("at stamped: %v", rows[0].At)
	}
}
