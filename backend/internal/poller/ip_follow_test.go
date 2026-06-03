package poller

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

func pollerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// --- pure matcher: planMoves ---

func TestPlanMoves(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-2 * time.Minute)
	fresh := now
	stale := now.Add(-1 * time.Hour)

	const mac = "04:F4:1C:85:97:B2"
	dev := queries.Device{ID: "d1", Identity: "ap02", Address: "192.168.78.56", Board: "cAP ax"}

	tests := []struct {
		name          string
		devices       []queries.Device
		macToDeviceID map[string]string
		ambiguous     map[string]bool
		addrToDevice  map[string]string
		neighbors     []queries.Neighbor
		wantAddrs     map[string]string // deviceID -> new addr
	}{
		{
			name:          "detects simple move (MAC uppercased)",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors:     []queries.Neighbor{{NeighborMAC: "04:f4:1c:85:97:b2", NeighborAddress: "192.168.78.232", LastSeen: fresh}},
			wantAddrs:     map[string]string{"d1": "192.168.78.232"},
		},
		{
			name:          "no change when address already matches",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors:     []queries.Neighbor{{NeighborMAC: mac, NeighborAddress: "192.168.78.56", LastSeen: fresh}},
			wantAddrs:     map[string]string{},
		},
		{
			name:          "multiple MACs per device, match on any",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{"AA:BB:CC:DD:EE:01": "d1", "AA:BB:CC:DD:EE:02": "d1"},
			neighbors:     []queries.Neighbor{{NeighborMAC: "AA:BB:CC:DD:EE:02", NeighborAddress: "192.168.78.232", LastSeen: fresh}},
			wantAddrs:     map[string]string{"d1": "192.168.78.232"},
		},
		{
			name:          "same device, multiple sightings, one move",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors: []queries.Neighbor{
				{NeighborMAC: mac, NeighborAddress: "192.168.78.232", LastSeen: fresh, LocalInterface: "ether1"},
				{NeighborMAC: mac, NeighborAddress: "192.168.78.232", LastSeen: fresh, LocalInterface: "ether2"},
			},
			wantAddrs: map[string]string{"d1": "192.168.78.232"},
		},
		{
			name:          "conflicting new addresses skipped",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors: []queries.Neighbor{
				{NeighborMAC: mac, NeighborAddress: "192.168.78.232", LastSeen: fresh},
				{NeighborMAC: mac, NeighborAddress: "192.168.78.240", LastSeen: fresh},
			},
			wantAddrs: map[string]string{},
		},
		{
			name:          "ambiguous MAC skipped",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			ambiguous:     map[string]bool{mac: true},
			neighbors:     []queries.Neighbor{{NeighborMAC: mac, NeighborAddress: "192.168.78.232", LastSeen: fresh}},
			wantAddrs:     map[string]string{},
		},
		{
			name:          "stale neighbor ignored",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors:     []queries.Neighbor{{NeighborMAC: mac, NeighborAddress: "192.168.78.232", LastSeen: stale}},
			wantAddrs:     map[string]string{},
		},
		{
			name:          "empty MAC or address ignored",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors: []queries.Neighbor{
				{NeighborMAC: "", NeighborAddress: "192.168.78.232", LastSeen: fresh},
				{NeighborMAC: mac, NeighborAddress: "", LastSeen: fresh},
			},
			wantAddrs: map[string]string{},
		},
		{
			name:          "target IP already managed by another device",
			devices:       []queries.Device{dev, {ID: "d2", Identity: "other", Address: "192.168.78.232", Board: "RB5009"}},
			macToDeviceID: map[string]string{mac: "d1"},
			addrToDevice:  map[string]string{"192.168.78.56": "d1", "192.168.78.232": "d2"},
			neighbors:     []queries.Neighbor{{NeighborMAC: mac, NeighborAddress: "192.168.78.232", LastSeen: fresh}},
			wantAddrs:     map[string]string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ambiguous == nil {
				tc.ambiguous = map[string]bool{}
			}
			if tc.addrToDevice == nil {
				tc.addrToDevice = map[string]string{}
				for _, d := range tc.devices {
					tc.addrToDevice[d.Address] = d.ID
				}
			}
			moves := planMoves(tc.devices, tc.macToDeviceID, tc.ambiguous, tc.addrToDevice, tc.neighbors, cutoff)
			if len(moves) != len(tc.wantAddrs) {
				t.Fatalf("got %d moves %+v, want %d (%v)", len(moves), moves, len(tc.wantAddrs), tc.wantAddrs)
			}
			for _, mv := range moves {
				want, ok := tc.wantAddrs[mv.DeviceID]
				if !ok {
					t.Fatalf("unexpected move for %s -> %s", mv.DeviceID, mv.NewAddr)
				}
				if mv.NewAddr != want {
					t.Fatalf("device %s: got new addr %s, want %s", mv.DeviceID, mv.NewAddr, want)
				}
			}
		})
	}
}

// --- pure anti-spoof comparator: verifyIdentity ---

func TestVerifyIdentity(t *testing.T) {
	dev := queries.Device{Board: "cAP ax", ROSVersion: "7.23 (stable)"}
	tests := []struct {
		name string
		dev  queries.Device
		res  *routeros.SystemResource
		want bool
	}{
		{"board exact match", dev, &routeros.SystemResource{Board: "cAP ax", Version: "7.23 (stable)"}, true},
		{"board case-insensitive", dev, &routeros.SystemResource{Board: "CAP AX", Version: "7.23 (stable)"}, true},
		{"board mismatch", dev, &routeros.SystemResource{Board: "RB5009", Version: "7.23 (stable)"}, false},
		{"empty live board", dev, &routeros.SystemResource{Board: "", Version: "7.23 (stable)"}, false},
		{"empty stored board", queries.Device{Board: ""}, &routeros.SystemResource{Board: "cAP ax"}, false},
		{"version mismatch when both set", dev, &routeros.SystemResource{Board: "cAP ax", Version: "7.22.3 (stable)"}, false},
		{"version empty on live falls back to board", dev, &routeros.SystemResource{Board: "cAP ax", Version: ""}, true},
		{"nil resource", dev, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyIdentity(tc.dev, tc.res); got != tc.want {
				t.Fatalf("verifyIdentity = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- feature flag parsing ---

func TestAutoFollowEnabled(t *testing.T) {
	tests := []struct {
		value string
		set   bool
		want  bool
	}{
		{"", false, false}, // missing row
		{"true", true, true},
		{"1", true, true},
		{" true ", true, true},
		{"false", true, false},
		{"0", true, false},
		{"TRUE", true, false},
		{"yes", true, false},
		{"garbage", true, false},
	}
	for _, tc := range tests {
		t.Run("value="+tc.value, func(t *testing.T) {
			db := pollerTestDB(t)
			if tc.set {
				if err := queries.SetSetting(db, "auto_follow_ip", tc.value); err != nil {
					t.Fatalf("set setting: %v", err)
				}
			}
			if got := autoFollowEnabled(db); got != tc.want {
				t.Fatalf("autoFollowEnabled(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// --- index building flags ambiguous MACs ---

func TestBuildDeviceMACIndex_FlagsAmbiguous(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "a", Address: "192.168.78.10", APIPort: 8728})
	mustCreateDevice(t, db, &queries.Device{ID: "d2", Identity: "b", Address: "192.168.78.11", APIPort: 8728})
	// Shared MAC across two devices => ambiguous.
	mustUpsertIface(t, db, "d1", "bridge", "AA:BB:CC:DD:EE:FF")
	mustUpsertIface(t, db, "d1", "ether1", "11:11:11:11:11:11")
	mustUpsertIface(t, db, "d2", "bridge", "AA:BB:CC:DD:EE:FF")
	mustUpsertIface(t, db, "d2", "ether1", "22:22:22:22:22:22")

	devices, err := queries.ListDevices(db)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	_, ambiguous, addrToDevice := buildDeviceMACIndex(db, devices)

	if !ambiguous["AA:BB:CC:DD:EE:FF"] {
		t.Fatalf("expected shared MAC to be flagged ambiguous")
	}
	if ambiguous["11:11:11:11:11:11"] || ambiguous["22:22:22:22:22:22"] {
		t.Fatalf("unique MACs should not be ambiguous")
	}
	if addrToDevice["192.168.78.10"] != "d1" || addrToDevice["192.168.78.11"] != "d2" {
		t.Fatalf("addrToDevice mapping wrong: %v", addrToDevice)
	}
}

// --- default-OFF: reconcile never commits without the gate ---

func TestReconcileAddresses_DisabledByDefault(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{
		ID: "d1", Identity: "ap02", Address: "192.168.78.56", Board: "cAP ax",
		Username: "admin", PasswordEnc: "secret", APIPort: 8728, Status: "online",
	})
	mustUpsertIface(t, db, "d1", "ether1", "04:F4:1C:85:97:B2")
	// A fresh neighbor advertising d1's MAC at a NEW ip (would be a move if enabled).
	if err := queries.UpsertNeighbor(db, &queries.Neighbor{
		ID: "d1:ether1:04:F4:1C:85:97:B2", DeviceID: "d1", LocalInterface: "ether1",
		NeighborAddress: "192.168.78.232", NeighborMAC: "04:F4:1C:85:97:B2",
	}); err != nil {
		t.Fatalf("upsert neighbor: %v", err)
	}

	m := NewManager(db, routeros.NewPool(false), ws.NewHub(), &config.Config{TopologyInterval: time.Minute})
	m.reconcileAddresses(context.Background())

	got, err := queries.GetDevice(db, "d1")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if got.Address != "192.168.78.56" {
		t.Fatalf("address changed while feature disabled: %s", got.Address)
	}
	if n := countLoopEvents(t, db); n != 0 {
		t.Fatalf("expected no audit events while disabled, got %d", n)
	}
}

// --- test helpers ---

func mustCreateDevice(t *testing.T, db *sql.DB, d *queries.Device) {
	t.Helper()
	if d.APIPort == 0 {
		d.APIPort = 8728
	}
	if d.Status == "" {
		d.Status = "online"
	}
	if err := queries.CreateDevice(db, d); err != nil {
		t.Fatalf("create device %s: %v", d.ID, err)
	}
}

func mustUpsertIface(t *testing.T, db *sql.DB, deviceID, name, mac string) {
	t.Helper()
	if err := queries.UpsertInterface(db, &queries.Interface{
		ID: deviceID + ":" + name, DeviceID: deviceID, Name: name, Type: "ether", MACAddress: mac,
	}); err != nil {
		t.Fatalf("upsert interface %s/%s: %v", deviceID, name, err)
	}
}

func countLoopEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM loop_events").Scan(&n); err != nil {
		t.Fatalf("count loop_events: %v", err)
	}
	return n
}
