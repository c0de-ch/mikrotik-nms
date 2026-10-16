package poller

import (
	"database/sql"
	"net/netip"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

func loginTestDB(t *testing.T) (*sql.DB, *queries.Device) {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dev := &queries.Device{ID: "d1", Address: "192.0.2.100", Identity: "sw1", Username: "nms", APIPort: 8728, Status: "online"}
	if err := queries.CreateDevice(db, dev); err != nil {
		t.Fatalf("create device: %v", err)
	}
	return db, dev
}

func findingsByKind(t *testing.T, db *sql.DB) map[string]queries.DeviceFinding {
	t.Helper()
	rows, err := queries.ListDeviceFindings(db, "d1", 0)
	if err != nil {
		t.Fatalf("list findings: %v", err)
	}
	out := make(map[string]queries.DeviceFinding)
	for _, f := range rows {
		out[f.Kind] = f
	}
	return out
}

func TestLoginWatchRecordsForeignAPILogins(t *testing.T) {
	db, dev := loginTestDB(t)
	w := NewLoginWatch(db)
	w.outboundFn = func(string, int) (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.1"), true }
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	active := rep(map[string]string{"name": "other", "address": "192.0.2.50", "via": "api", "when": "jan/01 09:59:00"})
	f := &fakeRunner{byCmd: map[string]fakeReply{"/user/active/print": active}}
	entries := []routeros.LogEntry{
		{ID: "*1", Time: "09:58:00", Topics: "system,info,account", Message: "user nms logged in from 192.0.2.1 via api"},       // ours
		{ID: "*2", Time: "09:59:00", Topics: "system,info,account", Message: "user other logged in from 192.0.2.50 via api"},    // not ours
		{ID: "*3", Time: "09:59:10", Topics: "system,info,account", Message: "user admin logged in from 192.0.2.60 via winbox"}, // not API
		{ID: "*4", Time: "09:59:20", Topics: "system,error,critical", Message: "login failure for user admin from 192.0.2.70 via ssh"},
	}

	// First sight of the buffer: nothing is recorded, nothing is confirmed.
	w.Process(dev, f, entries, true, now)
	if len(findingsByKind(t, db)) != 0 || len(f.calls) != 0 {
		t.Fatalf("first sight must be silent: findings=%v calls=%v", findingsByKind(t, db), f.calls)
	}

	w.Process(dev, f, entries, false, now)
	got := findingsByKind(t, db)
	par, ok := got[FindingParallelAPIPoller]
	if !ok || par.Severity != "warn" || par.Detail != "user other from 192.0.2.50 via api" || par.Count != 1 {
		t.Fatalf("parallel poller finding = %+v", par)
	}
	if fail, ok := got[FindingLoginFailure]; !ok || fail.Severity != "info" || fail.Detail != "user admin from 192.0.2.70 via ssh" {
		t.Fatalf("login failure finding = %+v", fail)
	}
	if len(got) != 2 || f.countCalls("/user/active/print") != 1 {
		t.Fatalf("findings=%d active reads=%d", len(got), f.countCalls("/user/active/print"))
	}

	// The same poller logging in again within the hour bumps the count
	// without another /user/active read.
	again := []routeros.LogEntry{{ID: "*5", Time: "10:00:30", Topics: "system,info,account", Message: "user other logged in from 192.0.2.50 via api"}}
	w.Process(dev, f, again, false, now.Add(time.Minute))
	if par := findingsByKind(t, db)[FindingParallelAPIPoller]; par.Count != 2 || f.countCalls("/user/active/print") != 1 {
		t.Fatalf("repeat: count=%d active reads=%d", par.Count, f.countCalls("/user/active/print"))
	}

	// An hour later it is confirmed again; the session is gone → info.
	f.byCmd["/user/active/print"] = rep()
	w.Process(dev, f, again, false, now.Add(2*time.Hour))
	if par := findingsByKind(t, db)[FindingParallelAPIPoller]; par.Severity != "info" || par.Count != 3 || f.countCalls("/user/active/print") != 2 {
		t.Fatalf("reconfirm: %+v active reads=%d", par, f.countCalls("/user/active/print"))
	}
}

func TestLoginWatchSelfAddressesFromSetting(t *testing.T) {
	db, dev := loginTestDB(t)
	if err := queries.SetSetting(db, "nms_source_addresses", "198.51.100.7, 2001:db8::7"); err != nil {
		t.Fatal(err)
	}
	w := NewLoginWatch(db)
	w.outboundFn = func(string, int) (netip.Addr, bool) { return netip.Addr{}, false } // no route knowledge
	f := &fakeRunner{byCmd: map[string]fakeReply{"/user/active/print": rep()}}
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	entries := []routeros.LogEntry{
		{ID: "*1", Time: "09:59:00", Topics: "system,info,account", Message: "user nms logged in from 198.51.100.7 via api-ssl"},
		{ID: "*2", Time: "09:59:01", Topics: "system,info,account", Message: "user nms logged in from 2001:db8::7 via api"},
		{ID: "*3", Time: "09:59:02", Topics: "system,info,account", Message: "user nms logged in from 2001:db8::8 via api"},
	}
	w.Process(dev, f, entries, false, now)
	got := findingsByKind(t, db)
	if par, ok := got[FindingParallelAPIPoller]; !ok || par.Detail != "user nms from 2001:db8::8 via api" || len(got) != 1 {
		t.Fatalf("findings = %+v", got)
	}
}
