package poller

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
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
		{
			// Regression: a multi-VLAN switch exposes the same bridge MAC on
			// several SVIs. Neighbors see the MAC at both the current dev.Address
			// and another live IP — that's not a move, it's a multi-homed device.
			// Without this guard the poller flaps the address each cycle.
			name:          "multi-homed device: current address still live, skip move",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors: []queries.Neighbor{
				{NeighborMAC: mac, NeighborAddress: "192.168.78.56", LastSeen: fresh},  // current dev.Address
				{NeighborMAC: mac, NeighborAddress: "192.168.78.232", LastSeen: fresh}, // also live
			},
			wantAddrs: map[string]string{},
		},
		{
			// A move should still commit when the old address has gone stale
			// (no fresh sighting), even if the same MAC is seen at a new IP.
			name:          "true move: only new address is live, old has gone quiet",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors: []queries.Neighbor{
				{NeighborMAC: mac, NeighborAddress: "192.168.78.56", LastSeen: stale},  // old: stale
				{NeighborMAC: mac, NeighborAddress: "192.168.78.232", LastSeen: fresh}, // new: live
			},
			wantAddrs: map[string]string{"d1": "192.168.78.232"},
		},
		{
			// An IPv6 link-local sighting is never a management move target (and
			// crashed the verify dial with "too many colons"). Old address quiet
			// so the multi-homed guard doesn't apply — the link-local filter is
			// what suppresses it.
			name:          "ipv6 link-local candidate skipped",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors:     []queries.Neighbor{{NeighborMAC: mac, NeighborAddress: "fe80::1afd:74ff:fe4c:419e", LastSeen: fresh}},
			wantAddrs:     map[string]string{},
		},
		{
			name:          "ipv4 link-local candidate skipped",
			devices:       []queries.Device{dev},
			macToDeviceID: map[string]string{mac: "d1"},
			neighbors:     []queries.Neighbor{{NeighborMAC: mac, NeighborAddress: "169.254.5.5", LastSeen: fresh}},
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
			moves := planMoves(tc.devices, tc.macToDeviceID, tc.ambiguous, tc.addrToDevice, tc.neighbors, seenSince(cutoff))
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

// --- audit MAC is the discovered MAC (deterministic), not an arbitrary one ---

func TestPlanMoves_AuditMACIsDiscoveredMAC(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-2 * time.Minute)
	dev := queries.Device{ID: "d1", Identity: "sw", Address: "192.168.78.10", Board: "CRS"}
	// Device owns two MACs; the sighting is on the SECOND one.
	macIdx := map[string]string{"AA:BB:CC:DD:EE:01": "d1", "AA:BB:CC:DD:EE:02": "d1"}
	neigh := []queries.Neighbor{{NeighborMAC: "aa:bb:cc:dd:ee:02", NeighborAddress: "192.168.78.50", LastSeen: now}}
	moves := planMoves([]queries.Device{dev}, macIdx, map[string]bool{}, map[string]string{"192.168.78.10": "d1"}, neigh, seenSince(cutoff))
	if len(moves) != 1 {
		t.Fatalf("want 1 move, got %d", len(moves))
	}
	if moves[0].MAC != "AA:BB:CC:DD:EE:02" {
		t.Fatalf("audit MAC = %s, want the discovered MAC AA:BB:CC:DD:EE:02", moves[0].MAC)
	}
}

func TestPlanMoves_BatchReservesTargetIP(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-2 * time.Minute)
	// Two devices, both sighted (via their own MACs) at the SAME new IP in one
	// batch. Only one move may be emitted (the other is blocked by the in-batch
	// reservation) to avoid a UNIQUE collision / IP swap at commit time.
	d1 := queries.Device{ID: "d1", Identity: "a", Address: "192.168.78.10"}
	d2 := queries.Device{ID: "d2", Identity: "b", Address: "192.168.78.11"}
	macIdx := map[string]string{"AA:AA:AA:AA:AA:01": "d1", "BB:BB:BB:BB:BB:02": "d2"}
	addr := map[string]string{"192.168.78.10": "d1", "192.168.78.11": "d2"}
	neigh := []queries.Neighbor{
		{NeighborMAC: "AA:AA:AA:AA:AA:01", NeighborAddress: "192.168.78.99", LastSeen: now},
		{NeighborMAC: "BB:BB:BB:BB:BB:02", NeighborAddress: "192.168.78.99", LastSeen: now},
	}
	moves := planMoves([]queries.Device{d1, d2}, macIdx, map[string]bool{}, addr, neigh, seenSince(cutoff))
	if len(moves) != 1 {
		t.Fatalf("want exactly 1 move (in-batch IP reservation), got %d: %+v", len(moves), moves)
	}
}

func TestInterfacesContainMAC(t *testing.T) {
	ifaces := []routeros.InterfaceInfo{{Name: "ether1", MACAddress: "04:F4:1C:85:97:B2"}, {Name: "bridge", MACAddress: "04:F4:1C:85:97:B2"}}
	if !interfacesContainMAC(ifaces, "04:f4:1c:85:97:b2") {
		t.Fatal("should match case-insensitively")
	}
	if interfacesContainMAC(ifaces, "AA:BB:CC:DD:EE:FF") {
		t.Fatal("should not match an absent MAC")
	}
	if interfacesContainMAC(ifaces, "") {
		t.Fatal("empty MAC should never match")
	}
}

func TestIsFollowableCandidate(t *testing.T) {
	cases := map[string]bool{
		"192.168.78.232":            true,  // routable v4
		"10.0.0.1":                  true,  // routable v4
		"2001:db8::1":               true,  // global v6
		"fe80::1afd:74ff:fe4c:419e": false, // v6 link-local (the prod crash source)
		"169.254.10.5":              false, // v4 link-local
		"127.0.0.1":                 false, // loopback
		"0.0.0.0":                   false, // unspecified
		"224.0.0.1":                 false, // multicast
		"not-an-ip":                 false, // not an IP literal
		"":                          false, // empty
	}
	for addr, want := range cases {
		if got := isFollowableCandidate(addr); got != want {
			t.Errorf("isFollowableCandidate(%q) = %v, want %v", addr, got, want)
		}
	}
}

// --- test helpers ---

// seenSince is planMoves' recency rule for one fixed cutoff.
func seenSince(cutoff time.Time) func(queries.Neighbor) bool {
	return func(n queries.Neighbor) bool { return !n.LastSeen.Before(cutoff) }
}

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

func assertAddr(t *testing.T, db *sql.DB, id, want string) {
	t.Helper()
	d, err := queries.GetDevice(db, id)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.Address != want {
		t.Fatalf("device %s address = %s, want %s", id, d.Address, want)
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

// --- reconcile: detect and propose, never dial ---

// followFixture is a device polled at oldAddr ("" = the fake endpoint's
// address) on the fake endpoint's port, with a fresh neighbor sighting of its
// MAC at newAddr, auto-follow switched on.
func followFixture(t *testing.T, oldAddr, newAddr string) (*sql.DB, *routerostest.Device, *Manager) {
	t.Helper()
	db := pollerTestDB(t)
	fake := routerostest.NewDevice(t)
	if oldAddr == "" {
		oldAddr = fake.Host
	}
	mustCreateDevice(t, db, &queries.Device{
		ID: "d1", Identity: "ap02", Address: oldAddr, Board: "cAP ax", ROSVersion: "7.16",
		Username: "admin", PasswordEnc: "secret", APIPort: fake.Port,
	})
	mustUpsertIface(t, db, "d1", "ether1", "04:F4:1C:85:97:B2")
	if err := queries.UpsertNeighbor(db, &queries.Neighbor{
		ID: "d1:ether1:x", DeviceID: "d1", LocalInterface: "ether1",
		NeighborAddress: newAddr, NeighborMAC: "04:F4:1C:85:97:B2",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetSetting(db, "auto_follow_ip", "true"); err != nil {
		t.Fatal(err)
	}
	m := NewManager(db, routeros.NewPool(false), ws.NewHub(), &config.Config{TopologyInterval: time.Minute})
	return db, fake, m
}

func pendingProposals(t *testing.T, db *sql.DB) []queries.AddressProposal {
	t.Helper()
	ps, err := queries.ListAddressProposals(db, queries.ProposalPending, 0)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// A sighting at a new address becomes one pending proposal — no login at the
// new address (no verify session in the pool), none at the old one (the fake
// endpoint polled there sees no connection), no address change — and a
// repeat sighting bumps it without a second event.
func TestReconcileAddresses_ProposesWithoutDialing(t *testing.T) {
	db, fake, m := followFixture(t, "", "192.0.2.232")
	old := fake.Host
	t0 := time.Now()
	m.now = func() time.Time { return t0 }
	m.reconcileAddresses(context.Background())

	assertAddr(t, db, "d1", old)
	if fake.Conns() != 0 || m.pool.Get("d1:verify") != nil {
		t.Fatalf("reconcile reached a device (%d connections)", fake.Conns())
	}
	ps := pendingProposals(t, db)
	if len(ps) != 1 {
		t.Fatalf("want 1 pending proposal, got %+v", ps)
	}
	p := ps[0]
	if p.DeviceID != "d1" || p.OldAddress != old || p.NewAddress != "192.0.2.232" || p.MAC != "04:F4:1C:85:97:B2" || p.DeviceIdentity != "ap02" {
		t.Fatalf("proposal = %+v", p)
	}
	if n := countLoopEvents(t, db); n != 1 {
		t.Fatalf("want 1 proposal event, got %d", n)
	}

	m.now = func() time.Time { return t0.Add(time.Minute) }
	m.reconcileAddresses(context.Background())
	ps = pendingProposals(t, db)
	if len(ps) != 1 || !ps[0].LastSeen.Equal(t0.Add(time.Minute).Truncate(time.Second).UTC()) {
		t.Fatalf("repeat sighting should bump last_seen of the one proposal: %+v", ps)
	}
	if n := countLoopEvents(t, db); n != 1 {
		t.Fatalf("a repeat sighting must not add events, got %d", n)
	}
	if fake.Conns() != 0 || m.pool.Get("d1:verify") != nil {
		t.Fatal("reconcile reached a device")
	}
}

// Pending proposals close on their own: an address edit supersedes them, a
// day without a sighting expires them — even with the feature switched off.
func TestReconcileAddresses_SettlesProposals(t *testing.T) {
	db, _, m := followFixture(t, "192.0.2.56", "192.0.2.232")
	t0 := time.Now()
	if _, err := queries.UpsertAddressProposal(db, "d1", "192.0.2.56", "192.0.2.232", "", t0.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetSetting(db, "auto_follow_ip", "false"); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return t0 }
	m.reconcileAddresses(context.Background())
	all, _ := queries.ListAddressProposals(db, "", 0)
	if len(all) != 1 || all[0].Status != queries.ProposalExpired {
		t.Fatalf("a proposal unseen for 25 h should expire: %+v", all)
	}

	if _, err := queries.UpsertAddressProposal(db, "d1", "192.0.2.56", "192.0.2.240", "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE devices SET address = '192.0.2.99' WHERE id = 'd1'`); err != nil {
		t.Fatal(err)
	}
	m.reconcileAddresses(context.Background())
	if ps := pendingProposals(t, db); len(ps) != 0 {
		t.Fatalf("an address edit should supersede the proposal: %+v", ps)
	}
	all, _ = queries.ListAddressProposals(db, queries.ProposalSuperseded, 0)
	if len(all) != 1 || all[0].NewAddress != "192.0.2.240" {
		t.Fatalf("superseded = %+v", all)
	}
}

// A newer sighting at a different address supersedes the older proposal.
func TestUpsertAddressProposal_NewerSightingSupersedes(t *testing.T) {
	db, _, _ := followFixture(t, "192.0.2.56", "192.0.2.232")
	now := time.Now()
	if c, err := queries.UpsertAddressProposal(db, "d1", "192.0.2.56", "192.0.2.232", "", now); err != nil || !c {
		t.Fatalf("first upsert created=%v err=%v", c, err)
	}
	if c, err := queries.UpsertAddressProposal(db, "d1", "192.0.2.56", "192.0.2.240", "", now); err != nil || !c {
		t.Fatalf("second upsert created=%v err=%v", c, err)
	}
	ps := pendingProposals(t, db)
	if len(ps) != 1 || ps[0].NewAddress != "192.0.2.240" {
		t.Fatalf("only the newest sighting should stay pending: %+v", ps)
	}
}

// --- VerifyAndCommitAddressMove (run on approval) ---

func TestVerifyAndCommitAddressMove_Stale(t *testing.T) {
	db, fake, _ := followFixture(t, "192.0.2.56", "127.0.0.1")
	_, err := VerifyAndCommitAddressMove(db, routeros.NewPool(false), ws.NewHub(),
		AddressMove{DeviceID: "d1", OldAddr: "192.0.2.1", NewAddr: "127.0.0.1"})
	var me *MoveError
	if !errors.As(err, &me) || me.Category != MoveStale {
		t.Fatalf("want stale, got %v", err)
	}
	if fake.Conns() != 0 || countLoopEvents(t, db) != 0 {
		t.Fatal("a stale move must not dial or audit")
	}
}

func TestVerifyAndCommitAddressMove_NoCredentials(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "ap", Address: "192.0.2.56", Board: "cAP ax", Username: "admin"})
	_, err := VerifyAndCommitAddressMove(db, routeros.NewPool(false), ws.NewHub(),
		AddressMove{DeviceID: "d1", OldAddr: "192.0.2.56", NewAddr: "192.0.2.232"})
	var me *MoveError
	if !errors.As(err, &me) || me.Category != MoveNoCredentials {
		t.Fatalf("want no_credentials, got %v", err)
	}
	assertAddr(t, db, "d1", "192.0.2.56")
	if n := countLoopEvents(t, db); n != 1 {
		t.Fatalf("want 1 rejection event, got %d", n)
	}
}

func TestVerifyAndCommitAddressMove_DialFailure(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "ap", Address: "192.0.2.56", Board: "cAP ax",
		Username: "admin", PasswordEnc: "secret", APIPort: 1})
	_, err := VerifyAndCommitAddressMove(db, routeros.NewPool(false), ws.NewHub(),
		AddressMove{DeviceID: "d1", OldAddr: "192.0.2.56", NewAddr: "127.0.0.1"})
	var me *MoveError
	if !errors.As(err, &me) || me.Category != MoveDialFailed {
		t.Fatalf("want dial_failed, got %v", err)
	}
	assertAddr(t, db, "d1", "192.0.2.56")
}

func TestVerifyAndCommitAddressMove_IdentityMismatch(t *testing.T) {
	db, fake, _ := followFixture(t, "192.0.2.56", "127.0.0.1")
	fake.Reply("/system/resource/print", map[string]string{"board-name": "CRS326", "version": "7.16"})
	_, err := VerifyAndCommitAddressMove(db, routeros.NewPool(false), ws.NewHub(),
		AddressMove{DeviceID: "d1", OldAddr: "192.0.2.56", NewAddr: "127.0.0.1", MAC: "04:F4:1C:85:97:B2"})
	var me *MoveError
	if !errors.As(err, &me) || me.Category != MoveIdentityMismatch {
		t.Fatalf("want identity_mismatch, got %v", err)
	}
	assertAddr(t, db, "d1", "192.0.2.56")
}

func TestVerifyAndCommitAddressMove_MACMismatch(t *testing.T) {
	db, fake, _ := followFixture(t, "192.0.2.56", "127.0.0.1")
	fake.Reply("/system/resource/print", map[string]string{"board-name": "cAP ax", "version": "7.16"})
	fake.Reply("/interface/print", map[string]string{"name": "ether1", "type": "ether", "mac-address": "AA:AA:AA:AA:AA:AA"})
	_, err := VerifyAndCommitAddressMove(db, routeros.NewPool(false), ws.NewHub(),
		AddressMove{DeviceID: "d1", OldAddr: "192.0.2.56", NewAddr: "127.0.0.1", MAC: "04:F4:1C:85:97:B2"})
	var me *MoveError
	if !errors.As(err, &me) || me.Category != MoveMACMismatch {
		t.Fatalf("want mac_mismatch, got %v", err)
	}
	assertAddr(t, db, "d1", "192.0.2.56")
}

func TestVerifyAndCommitAddressMove_Commits(t *testing.T) {
	db, fake, _ := followFixture(t, "192.0.2.56", "127.0.0.1")
	fake.Reply("/system/resource/print", map[string]string{"board-name": "cAP ax", "version": "7.16"})
	fake.Reply("/interface/print", map[string]string{"name": "ether1", "type": "ether", "mac-address": "04:f4:1c:85:97:b2"})
	summary, err := VerifyAndCommitAddressMove(db, routeros.NewPool(false), ws.NewHub(),
		AddressMove{DeviceID: "d1", OldAddr: "192.0.2.56", NewAddr: "127.0.0.1", MAC: "04:F4:1C:85:97:B2"})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if summary == "" {
		t.Fatal("empty summary")
	}
	assertAddr(t, db, "d1", "127.0.0.1")
	if n := countLoopEvents(t, db); n != 1 {
		t.Fatalf("want 1 change event, got %d", n)
	}
}

// A committed move forgets the device's pooled dial spec: Close would keep
// it, and the next GetLive would redial the OLD address with the stored
// credential — whoever holds that address now.
func TestVerifyAndCommitAddressMove_ForgetsTheOldDialSpec(t *testing.T) {
	db, fake, _ := followFixture(t, "192.0.2.56", "127.0.0.1")
	fake.Reply("/system/resource/print", map[string]string{"board-name": "cAP ax", "version": "7.16"})
	pool := routeros.NewPool(false)
	fake.Dial(t, pool, "d1") // the pooled session the pollers use
	if _, err := VerifyAndCommitAddressMove(db, pool, ws.NewHub(),
		AddressMove{DeviceID: "d1", OldAddr: "192.0.2.56", NewAddr: "127.0.0.1"}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	conns := fake.Conns()
	if c := pool.GetLive("d1"); c != nil {
		t.Fatal("GetLive redialled the remembered spec after the move")
	}
	if fake.Conns() != conns {
		t.Error("a dial was made with the forgotten spec")
	}
}

// A dismissed move is not proposed again for DismissedProposalQuiet, a
// failed one for FailedProposalQuiet; a different new address still is.
func TestUpsertAddressProposal_DecisionsStayQuiet(t *testing.T) {
	db, _, _ := followFixture(t, "192.0.2.56", "192.0.2.232")
	now := time.Now()
	for _, tc := range []struct {
		status string
		quiet  time.Duration
		addr   string
	}{
		{queries.ProposalDismissed, queries.DismissedProposalQuiet, "192.0.2.232"},
		{queries.ProposalFailed, queries.FailedProposalQuiet, "192.0.2.233"},
	} {
		if c, err := queries.UpsertAddressProposal(db, "d1", "192.0.2.56", tc.addr, "", now); err != nil || !c {
			t.Fatalf("%s: first upsert created=%v err=%v", tc.status, c, err)
		}
		ps := pendingProposals(t, db)
		if len(ps) != 1 {
			t.Fatalf("%s: pending %+v", tc.status, ps)
		}
		if _, err := queries.DecideAddressProposal(db, ps[0].ID, tc.status, "admin", "", now); err != nil {
			t.Fatal(err)
		}
		if c, err := queries.UpsertAddressProposal(db, "d1", "192.0.2.56", tc.addr, "", now.Add(time.Minute)); err != nil || c {
			t.Errorf("%s: re-proposed a minute later (created=%v err=%v)", tc.status, c, err)
		}
		if c, err := queries.UpsertAddressProposal(db, "d1", "192.0.2.56", tc.addr, "", now.Add(tc.quiet+time.Minute)); err != nil || !c {
			t.Errorf("%s: not re-proposed after the quiet period (created=%v err=%v)", tc.status, c, err)
		}
		// close it again for the next case
		ps = pendingProposals(t, db)
		for _, p := range ps {
			_, _ = queries.DecideAddressProposal(db, p.ID, queries.ProposalExpired, "", "", now)
		}
	}
}
