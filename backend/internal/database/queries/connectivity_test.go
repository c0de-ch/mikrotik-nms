package queries

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPingTargetCRUDRoundtrip(t *testing.T) {
	db := testDB(t)

	target := &PingTarget{
		ID:         uuid.NewString(),
		Kind:       "client",
		Address:    "192.168.1.50",
		MACAddress: "80:6D:97:5C:C7:C9",
		Label:      "taneda-cadabi-t4",
		Enabled:    true,
	}
	if err := CreatePingTarget(db, target); err != nil {
		t.Fatalf("CreatePingTarget: %v", err)
	}

	got, err := GetPingTarget(db, target.ID)
	if err != nil {
		t.Fatalf("GetPingTarget: %v", err)
	}
	if got.Kind != "client" || got.MACAddress != "80:6D:97:5C:C7:C9" || !got.Enabled {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at not populated by DB default")
	}

	got.Enabled = false
	got.Label = "renamed"
	if err := UpdatePingTarget(db, got); err != nil {
		t.Fatalf("UpdatePingTarget: %v", err)
	}
	if err := UpdatePingTargetAddress(db, got.ID, "192.168.1.51"); err != nil {
		t.Fatalf("UpdatePingTargetAddress: %v", err)
	}

	again, err := GetPingTarget(db, got.ID)
	if err != nil {
		t.Fatalf("GetPingTarget after update: %v", err)
	}
	if again.Enabled || again.Label != "renamed" || again.Address != "192.168.1.51" {
		t.Errorf("update not persisted: %+v", again)
	}

	enabled, err := ListEnabledPingTargets(db)
	if err != nil {
		t.Fatalf("ListEnabledPingTargets: %v", err)
	}
	if len(enabled) != 0 {
		t.Errorf("disabled target listed as enabled: %d", len(enabled))
	}

	if err := DeletePingTarget(db, got.ID); err != nil {
		t.Fatalf("DeletePingTarget: %v", err)
	}
	if err := DeletePingTarget(db, got.ID); err != sql.ErrNoRows {
		t.Errorf("second delete err = %v, want sql.ErrNoRows", err)
	}
	if _, err := GetPingTarget(db, got.ID); err != sql.ErrNoRows {
		t.Errorf("get after delete err = %v, want sql.ErrNoRows", err)
	}
}

func TestPingSampleInsertQueryAndLatest(t *testing.T) {
	db := testDB(t)

	target := &PingTarget{ID: uuid.NewString(), Kind: "internet", Address: "1.1.1.1", DeviceID: "dev1", Enabled: true}
	if err := CreatePingTarget(db, target); err != nil {
		t.Fatalf("CreatePingTarget: %v", err)
	}

	avg := 12.5
	older := &PingSample{
		TargetID: target.ID, DeviceID: "dev1", Address: "1.1.1.1",
		Sent: 5, Received: 5, LossPct: 0, RTTAvgMs: &avg,
		RecordedAt: time.Now().UTC().Add(-time.Minute),
	}
	if err := InsertPingSample(db, older); err != nil {
		t.Fatalf("InsertPingSample(older): %v", err)
	}
	newer := &PingSample{
		TargetID: target.ID, DeviceID: "dev1", Address: "1.1.1.1",
		Sent: 0, Error: "probing device offline",
	}
	if err := InsertPingSample(db, newer); err != nil {
		t.Fatalf("InsertPingSample(newer): %v", err)
	}
	if newer.ID == 0 || newer.RecordedAt.IsZero() {
		t.Errorf("InsertPingSample did not backfill id/recorded_at: %+v", newer)
	}

	samples, err := GetPingSamples(db, target.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("GetPingSamples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(samples))
	}
	if samples[0].Error != "probing device offline" {
		t.Errorf("samples not newest-first: %+v", samples[0])
	}
	if samples[1].RTTAvgMs == nil || *samples[1].RTTAvgMs != 12.5 {
		t.Errorf("rtt_avg_ms roundtrip failed: %+v", samples[1].RTTAvgMs)
	}
	if samples[0].RTTAvgMs != nil {
		t.Errorf("error sample should have nil rtt_avg_ms, got %v", *samples[0].RTTAvgMs)
	}

	latest, err := GetLatestPingSamples(db)
	if err != nil {
		t.Fatalf("GetLatestPingSamples: %v", err)
	}
	ls, ok := latest[target.ID]
	if !ok {
		t.Fatal("no latest sample for target")
	}
	if ls.ID != newer.ID {
		t.Errorf("latest sample id = %d, want %d", ls.ID, newer.ID)
	}

	// Retention: everything older than now+1h goes away.
	n, err := DeleteOldPingSamples(db, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("DeleteOldPingSamples: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d samples, want 2", n)
	}
}

func TestClientSignalSamplesRoundtrip(t *testing.T) {
	db := testDB(t)

	sig := -62
	s := &ClientSignalSample{
		MACAddress: "80:6D:97:5C:C7:C9",
		APName:     "ap-office",
		SSID:       "main",
		Band:       "5ghz-ac",
		SignalDBm:  &sig,
		TxRate:     "867Mbps",
		RxRate:     "867Mbps",
	}
	if err := InsertClientSignalSample(db, s); err != nil {
		t.Fatalf("InsertClientSignalSample: %v", err)
	}

	got, err := GetClientSignalSamples(db, "80:6D:97:5C:C7:C9", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("GetClientSignalSamples: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d signal samples, want 1", len(got))
	}
	if got[0].SignalDBm == nil || *got[0].SignalDBm != -62 || got[0].APName != "ap-office" {
		t.Errorf("signal roundtrip mismatch: %+v", got[0])
	}

	n, err := DeleteOldClientSignalSamples(db, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("DeleteOldClientSignalSamples: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d signal samples, want 1", n)
	}
}

func TestResolveClientProbe(t *testing.T) {
	db := testDB(t)

	mac := "80:6D:97:5C:C7:C9"
	target := &PingTarget{ID: uuid.NewString(), Kind: "client", MACAddress: mac, Enabled: true}

	// No mac_lookup entry at all.
	if _, _, _, reason := ResolveClientProbe(db, target); reason == "" {
		t.Error("expected errReason for unknown client")
	}

	// Known MAC but no IP.
	if err := UpsertMACLookup(db, &MACLookup{MACAddress: mac, HostName: "taneda-cadabi-t4", Source: "arp"}); err != nil {
		t.Fatalf("UpsertMACLookup: %v", err)
	}
	if _, _, host, reason := ResolveClientProbe(db, target); reason == "" || host != "taneda-cadabi-t4" {
		t.Errorf("no-IP case: host=%q reason=%q", host, reason)
	}

	// IP known, but no online device.
	if err := UpsertMACLookup(db, &MACLookup{MACAddress: mac, IPAddress: "192.168.1.50", Source: "arp"}); err != nil {
		t.Fatalf("UpsertMACLookup: %v", err)
	}
	if _, addr, _, reason := ResolveClientProbe(db, target); reason == "" || addr != "192.168.1.50" {
		t.Errorf("no-device case: addr=%q reason=%q", addr, reason)
	}

	// Online devices: priority target.device_id > mac_lookup.device_id > any online.
	mkDevice := func(id, status string) {
		t.Helper()
		if err := CreateDevice(db, &Device{ID: id, Address: id + ".example", Username: "u", PasswordEnc: "p", APIPort: 8728, Status: status}); err != nil {
			t.Fatalf("CreateDevice(%s): %v", id, err)
		}
	}
	mkDevice("dev-any", "online")
	mkDevice("dev-lookup", "online")
	mkDevice("dev-target", "offline")

	if err := UpsertMACLookup(db, &MACLookup{MACAddress: mac, IPAddress: "192.168.1.50", Source: "arp", DeviceID: "dev-lookup"}); err != nil {
		t.Fatalf("UpsertMACLookup: %v", err)
	}

	// Target's own device is offline → fall through to mac_lookup's device.
	target.DeviceID = "dev-target"
	devID, addr, _, reason := ResolveClientProbe(db, target)
	if reason != "" || devID != "dev-lookup" || addr != "192.168.1.50" {
		t.Errorf("lookup-device case: dev=%q addr=%q reason=%q", devID, addr, reason)
	}

	// Target's own device online → it wins.
	mkDevice("dev-target2", "online")
	target.DeviceID = "dev-target2"
	if devID, _, _, reason = ResolveClientProbe(db, target); reason != "" || devID != "dev-target2" {
		t.Errorf("target-device case: dev=%q reason=%q", devID, reason)
	}
}
