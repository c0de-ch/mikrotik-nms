package queries

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
)

func TestPingTargetCRUDRoundtrip(t *testing.T) {
	db := testDB(t)

	target := &PingTarget{
		ID:         uuid.NewString(),
		Kind:       "client",
		Address:    "192.168.1.50",
		MACAddress: "80:6D:97:00:00:01",
		Label:      "laptop01",
		Enabled:    true,
	}
	if err := CreatePingTarget(db, target); err != nil {
		t.Fatalf("CreatePingTarget: %v", err)
	}

	got, err := GetPingTarget(db, target.ID)
	if err != nil {
		t.Fatalf("GetPingTarget: %v", err)
	}
	if got.Kind != "client" || got.MACAddress != "80:6D:97:00:00:01" || !got.Enabled {
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

func TestPingTargetSrcFieldsRoundtrip(t *testing.T) {
	db := testDB(t)

	target := &PingTarget{
		ID:           uuid.NewString(),
		Kind:         "internet",
		Address:      "1.1.1.1",
		DeviceID:     "dev1",
		SrcAddress:   "192.168.88.1",
		SrcInterface: "vlan88",
		Enabled:      true,
	}
	if err := CreatePingTarget(db, target); err != nil {
		t.Fatalf("CreatePingTarget: %v", err)
	}

	got, err := GetPingTarget(db, target.ID)
	if err != nil {
		t.Fatalf("GetPingTarget: %v", err)
	}
	if got.SrcAddress != "192.168.88.1" || got.SrcInterface != "vlan88" {
		t.Errorf("src fields roundtrip mismatch: %+v", got)
	}

	got.SrcAddress = "2001:db8::1"
	got.SrcInterface = ""
	if err := UpdatePingTarget(db, got); err != nil {
		t.Fatalf("UpdatePingTarget: %v", err)
	}
	again, err := GetPingTarget(db, got.ID)
	if err != nil {
		t.Fatalf("GetPingTarget after update: %v", err)
	}
	if again.SrcAddress != "2001:db8::1" || again.SrcInterface != "" {
		t.Errorf("src fields update not persisted: %+v", again)
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
		MACAddress: "80:6D:97:00:00:01",
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

	got, err := GetClientSignalSamples(db, "80:6D:97:00:00:01", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 10)
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

func TestSpeedTestCRUDAndSamples(t *testing.T) {
	db := testDB(t)

	test := &SpeedTest{
		ID:       uuid.NewString(),
		DeviceID: "dev1",
		URL:      "https://speed.example.com/100MB.bin",
		Label:    "wan via dev1",
		Enabled:  true,
	}
	if err := CreateSpeedTest(db, test); err != nil {
		t.Fatalf("CreateSpeedTest: %v", err)
	}

	got, err := GetSpeedTest(db, test.ID)
	if err != nil {
		t.Fatalf("GetSpeedTest: %v", err)
	}
	if got.URL != test.URL || got.DeviceID != "dev1" || !got.Enabled {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at not populated by DB default")
	}

	got.Enabled = false
	got.Label = "renamed"
	if err := UpdateSpeedTest(db, got); err != nil {
		t.Fatalf("UpdateSpeedTest: %v", err)
	}
	enabled, err := ListEnabledSpeedTests(db)
	if err != nil {
		t.Fatalf("ListEnabledSpeedTests: %v", err)
	}
	if len(enabled) != 0 {
		t.Errorf("disabled test listed as enabled: %d", len(enabled))
	}
	all, err := ListSpeedTests(db)
	if err != nil {
		t.Fatalf("ListSpeedTests: %v", err)
	}
	if len(all) != 1 || all[0].Label != "renamed" {
		t.Errorf("update not persisted: %+v", all)
	}

	// Samples: a successful one and a newer failed one (nil mbps).
	mbps := 87.5
	older := &SpeedSample{
		TestID: test.ID, DeviceID: "dev1", Mbps: &mbps, Bytes: 104857600, DurationMs: 9586,
		RecordedAt: time.Now().UTC().Add(-time.Minute),
	}
	if err := InsertSpeedSample(db, older); err != nil {
		t.Fatalf("InsertSpeedSample(older): %v", err)
	}
	newer := &SpeedSample{TestID: test.ID, DeviceID: "dev1", Error: "device is offline"}
	if err := InsertSpeedSample(db, newer); err != nil {
		t.Fatalf("InsertSpeedSample(newer): %v", err)
	}
	if newer.ID == 0 || newer.RecordedAt.IsZero() {
		t.Errorf("InsertSpeedSample did not backfill id/recorded_at: %+v", newer)
	}

	samples, err := GetSpeedSamples(db, test.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("GetSpeedSamples: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(samples))
	}
	if samples[0].Error != "device is offline" || samples[0].Mbps != nil {
		t.Errorf("samples not newest-first or error sample has mbps: %+v", samples[0])
	}
	if samples[1].Mbps == nil || *samples[1].Mbps != 87.5 || samples[1].Bytes != 104857600 {
		t.Errorf("mbps/bytes roundtrip failed: %+v", samples[1])
	}

	latest, err := GetLatestSpeedSamples(db)
	if err != nil {
		t.Fatalf("GetLatestSpeedSamples: %v", err)
	}
	ls, ok := latest[test.ID]
	if !ok {
		t.Fatal("no latest sample for test")
	}
	if ls.ID != newer.ID {
		t.Errorf("latest sample id = %d, want %d", ls.ID, newer.ID)
	}

	// Retention: everything older than now+1h goes away.
	n, err := DeleteOldSpeedSamples(db, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("DeleteOldSpeedSamples: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d samples, want 2", n)
	}

	if err := DeleteSpeedTest(db, test.ID); err != nil {
		t.Fatalf("DeleteSpeedTest: %v", err)
	}
	if err := DeleteSpeedTest(db, test.ID); err != sql.ErrNoRows {
		t.Errorf("second delete err = %v, want sql.ErrNoRows", err)
	}
	if _, err := GetSpeedTest(db, test.ID); err != sql.ErrNoRows {
		t.Errorf("get after delete err = %v, want sql.ErrNoRows", err)
	}
}

func TestTracerouteRunRoundtrip(t *testing.T) {
	db := testDB(t)

	target := &PingTarget{ID: uuid.NewString(), Kind: "internet", Address: "8.8.8.8", DeviceID: "dev1", Enabled: true}
	if err := CreatePingTarget(db, target); err != nil {
		t.Fatalf("CreatePingTarget: %v", err)
	}

	last := 8.2
	older := &TracerouteRun{
		TargetID: target.ID,
		Address:  "8.8.8.8",
		Hops: []edgeproto.TracerouteHop{
			{Hop: 1, Address: "192.168.1.1", LossPct: 0, Sent: 1, LastMs: &last, AvgMs: &last, BestMs: &last, WorstMs: &last},
			{Hop: 2, Address: "", LossPct: 100, Sent: 1, Status: "timeout"},
		},
		RecordedAt: time.Now().UTC().Add(-time.Minute),
	}
	if err := InsertTracerouteRun(db, older); err != nil {
		t.Fatalf("InsertTracerouteRun(older): %v", err)
	}
	newer := &TracerouteRun{TargetID: target.ID, Address: "8.8.8.8", Error: "probing device offline"}
	if err := InsertTracerouteRun(db, newer); err != nil {
		t.Fatalf("InsertTracerouteRun(newer): %v", err)
	}
	if newer.ID == 0 || newer.RecordedAt.IsZero() {
		t.Errorf("InsertTracerouteRun did not backfill id/recorded_at: %+v", newer)
	}
	if newer.Hops == nil {
		t.Error("InsertTracerouteRun left Hops nil — struct not publishable as-is")
	}

	runs, err := GetTracerouteRuns(db, target.ID, 10)
	if err != nil {
		t.Fatalf("GetTracerouteRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if runs[0].Error != "probing device offline" || len(runs[0].Hops) != 0 || runs[0].Hops == nil {
		t.Errorf("runs not newest-first or error run hops not empty slice: %+v", runs[0])
	}
	if len(runs[1].Hops) != 2 {
		t.Fatalf("hops JSON roundtrip: got %d hops, want 2", len(runs[1].Hops))
	}
	h := runs[1].Hops[0]
	if h.Hop != 1 || h.Address != "192.168.1.1" || h.LastMs == nil || *h.LastMs != 8.2 {
		t.Errorf("hop 1 roundtrip mismatch: %+v", h)
	}
	if to := runs[1].Hops[1]; to.Status != "timeout" || to.LastMs != nil || to.LossPct != 100 {
		t.Errorf("timeout hop roundtrip mismatch: %+v", to)
	}

	// limit applies
	one, err := GetTracerouteRuns(db, target.ID, 1)
	if err != nil {
		t.Fatalf("GetTracerouteRuns(limit=1): %v", err)
	}
	if len(one) != 1 || one[0].ID != newer.ID {
		t.Errorf("limit/order wrong: %+v", one)
	}

	n, err := DeleteOldTracerouteRuns(db, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("DeleteOldTracerouteRuns: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d runs, want 2", n)
	}
}

func TestResolveClientProbe(t *testing.T) {
	db := testDB(t)

	mac := "80:6D:97:00:00:01"
	target := &PingTarget{ID: uuid.NewString(), Kind: "client", MACAddress: mac, Enabled: true}

	// No mac_lookup entry at all.
	if _, _, _, reason := ResolveClientProbe(db, target); reason == "" {
		t.Error("expected errReason for unknown client")
	}

	// Known MAC but no IP.
	if err := UpsertMACLookup(db, &MACLookup{MACAddress: mac, HostName: "laptop01", Source: "arp"}); err != nil {
		t.Fatalf("UpsertMACLookup: %v", err)
	}
	if _, _, host, reason := ResolveClientProbe(db, target); reason == "" || host != "laptop01" {
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

// A watch without a chosen device probes from a strong device where one can
// be expected to reach the client; a weak device is used only when nothing
// better is known to see it.
func TestResolveClientProbePrefersAStrongDeviceOnTheClientsNetwork(t *testing.T) {
	db := testDB(t)
	const mac = "02:00:00:00:C0:01"
	mk := func(id, address string, class devclass.Class) {
		t.Helper()
		if err := CreateDevice(db, &Device{ID: id, Identity: id, Address: address, Username: "u", PasswordEnc: "p", APIPort: 8728, Status: "online"}); err != nil {
			t.Fatalf("CreateDevice(%s): %v", id, err)
		}
		if class != "" {
			if err := SetDeviceClass(db, id, class, devclass.ResourceFacts{}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	// By identity: a strong device on another network, a strong one on the
	// client's network, the weak switch that reported the client, an
	// unclassified device.
	mk("a-far", "198.51.100.1", devclass.C3)
	mk("b-rtr", "192.0.2.1", devclass.C3)
	mk("c-sw", "192.0.2.5", devclass.C1)
	mk("d-new", "192.0.2.9", "")
	seenBy := func(deviceID, ip string) {
		t.Helper()
		if _, err := db.Exec(`DELETE FROM mac_lookup WHERE mac_address = ?`, mac); err != nil {
			t.Fatal(err)
		}
		if err := UpsertMACLookup(db, &MACLookup{MACAddress: mac, IPAddress: ip, Source: "arp", DeviceID: deviceID}); err != nil {
			t.Fatal(err)
		}
	}
	watch := &PingTarget{ID: uuid.NewString(), Kind: "client", MACAddress: mac, Enabled: true}
	resolve := func(want, why string) {
		t.Helper()
		got, _, _, reason := ResolveClientProbe(db, watch)
		if reason != "" || got != want {
			t.Errorf("%s: probing device = %q (reason %q), want %q", why, got, reason, want)
		}
	}

	seenBy("c-sw", "192.0.2.50")
	resolve("b-rtr", "the client was reported by a weak switch and a strong device is on its network")

	// The strong device on the client's network cannot be used: the weak
	// device that sees the client stays, not a strong one elsewhere.
	if err := PauseDevicePolling(db, "b-rtr", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	resolve("c-sw", "the strong device on the client's network is paused")
	if err := ResumeDevicePolling(db, "b-rtr"); err != nil {
		t.Fatal(err)
	}
	if err := SetDevicePollProfile(db, "b-rtr", devclass.ProfileLight); err != nil {
		t.Fatal(err)
	}
	resolve("c-sw", "the strong device on the client's network is on the light profile")
	if err := SetDevicePollProfile(db, "b-rtr", devclass.ProfileAuto); err != nil {
		t.Fatal(err)
	}
	if err := SetDeviceClassOverride(db, "b-rtr", devclass.C1); err != nil {
		t.Fatal(err)
	}
	resolve("c-sw", "the strong device on the client's network is overridden to C1")
	if err := SetDeviceClassOverride(db, "b-rtr", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE devices SET status = 'offline' WHERE id = 'b-rtr'`); err != nil {
		t.Fatal(err)
	}
	resolve("c-sw", "the strong device on the client's network is offline")
	if _, err := db.Exec(`UPDATE devices SET status = 'online' WHERE id = 'b-rtr'`); err != nil {
		t.Fatal(err)
	}

	// A strong device that reported the client itself is used wherever it is.
	seenBy("a-far", "192.0.2.50")
	resolve("a-far", "the client was reported by a strong device")

	// Nobody is on record as seeing the client: a strong device on its
	// network, else any strong device, else any device.
	seenBy("", "192.0.2.50")
	resolve("b-rtr", "no reporting device, a strong device on the client's network")
	seenBy("", "203.0.113.7")
	resolve("a-far", "no reporting device and no strong device on the client's network")
	seenBy("", "2001:db8::7")
	resolve("a-far", "an IPv6 client without a reporting device")
	for _, id := range []string{"a-far", "b-rtr"} {
		if err := SetDeviceClassOverride(db, id, devclass.C2); err != nil {
			t.Fatal(err)
		}
	}
	resolve("a-far", "no strong device at all: the first online device, as before")

	// A device chosen for the watch stands, whatever its class.
	seenBy("b-rtr", "192.0.2.50")
	watch.DeviceID = "c-sw"
	resolve("c-sw", "the watch is pinned to a weak device")
}
