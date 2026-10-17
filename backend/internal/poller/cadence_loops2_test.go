package poller

import (
	"bytes"
	"context"
	"database/sql"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// classFleet creates the devices of a loop test: id → class, paused ones
// held for a day.
func classFleet(t *testing.T, db *sql.DB, classes map[string]devclass.Class, paused ...string) {
	t.Helper()
	i := 0
	for id, c := range classes {
		i++
		mustCreateDevice(t, db, &queries.Device{ID: id, Identity: id, Address: "198.51.100." + string(rune('0'+i))})
		if c != "" {
			setClass(t, db, id, c)
		}
	}
	for _, id := range paused {
		if err := queries.PauseDevicePolling(db, id, time.Now().Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
}

func scriptedRunners(byID map[string]*fakeRunner) func(string) routeros.Runner {
	return func(id string) routeros.Runner {
		if r, ok := byID[id]; ok {
			return r
		}
		return nil
	}
}

// bridgeDevice answers the network-health reads of a device without bridges.
func bridgeDevice() *fakeRunner {
	return &fakeRunner{byCmd: map[string]fakeReply{
		"/interface/bridge/print":      rep(),
		"/interface/bridge/vlan/print": rep(),
		"/log/print":                   rep(),
	}}
}

// The network-health poller runs each device's bridge (2a–2c), VLAN (3a) and
// log reads on its class cadence — C3: 2 / 5 / 2 min, C1: 5 / 15 / 5 min —
// and never on a paused device.
func TestNetworkHealthPerClassCadenceAndPause(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"r3": devclass.C3, "s1": devclass.C1, "held": devclass.C3}, "held")
	runners := map[string]*fakeRunner{"r3": bridgeDevice(), "s1": bridgeDevice(), "held": bridgeDevice()}
	n := NewNetworkHealthPoller(db, nil, ws.NewHub(), time.Minute)
	clock := &testClock{t: at(12, 0, 0)}
	n.now = clock.now
	n.clock = newTierClock(NewScheduleRegistry())
	n.clock.phase = noJitter
	n.UseCadence(NewCadence(db, &config.Config{TopologyInterval: time.Minute, NetworkHealthInterval: time.Minute}))
	n.runner = scriptedRunners(runners)
	reads := func(id string) [3]int {
		r := runners[id]
		return [3]int{r.countCalls("/interface/bridge/print"), r.countCalls("/interface/bridge/vlan/print"), r.countCalls("/log/print")}
	}

	for _, st := range []struct {
		at     time.Duration
		r3, s1 [3]int
	}{
		{0, [3]int{1, 1, 1}, [3]int{1, 1, 1}},
		{time.Minute, [3]int{1, 1, 1}, [3]int{1, 1, 1}},
		{2 * time.Minute, [3]int{2, 1, 2}, [3]int{1, 1, 1}},
		{5 * time.Minute, [3]int{3, 2, 3}, [3]int{2, 1, 2}},
		{15 * time.Minute, [3]int{4, 3, 4}, [3]int{3, 2, 3}},
	} {
		clock.set(at(12, 0, 0).Add(st.at))
		n.poll(context.Background())
		if got := reads("r3"); got != st.r3 {
			t.Errorf("+%v: C3 bridge/vlan/log reads = %v, want %v", st.at, got, st.r3)
		}
		if got := reads("s1"); got != st.s1 {
			t.Errorf("+%v: C1 bridge/vlan/log reads = %v, want %v", st.at, got, st.s1)
		}
	}
	if len(runners["held"].calls) != 0 {
		t.Errorf("a paused device was read: %q", runners["held"].calls)
	}
}

// The prunes drop a device's bridge, VLAN and interface rows once they are
// older than 3× that device's cadence for them — a weak switch's on its own
// slow clock — but never sooner than 30 min (a device that drops off for a
// few minutes stays on the page), and never a paused device's.
func TestNetworkHealthPrunesPerDeviceAndNeverPaused(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"r3": devclass.C3, "s1": devclass.C1, "held": devclass.C3}, "held")
	now := time.Now().UTC().Truncate(time.Second)
	for _, id := range []string{"r3", "s1", "held"} {
		if err := queries.UpsertBridgeStatus(db, &queries.BridgeStatus{ID: id + ":bridge", DeviceID: id, BridgeName: "bridge"}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpsertBridgeVLAN(db, &queries.BridgeVLAN{ID: id + ":bridge:10", DeviceID: id, BridgeName: "bridge", VLANIDs: "10"}); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpsertInterfaceState(db, &queries.InterfaceState{ID: id + ":ether1", DeviceID: id, InterfaceName: "ether1"}); err != nil {
			t.Fatal(err)
		}
	}
	age := func(table string, d time.Duration) {
		if _, err := db.Exec(`UPDATE `+table+` SET last_polled = ?`, sqlStamp(now.Add(-d))); err != nil {
			t.Fatal(err)
		}
	}
	age("bridge_status", 20*time.Minute)   // C3 bridge ports 2 min, C1 5 min → both the 30 min floor
	age("bridge_vlans", 40*time.Minute)    // C3 5 min → the floor; C1 15 min → 45 min
	age("interface_state", 31*time.Minute) // every 60 s → the floor

	n := NewNetworkHealthPoller(db, nil, ws.NewHub(), time.Minute)
	n.UseCadence(NewCadence(db, &config.Config{TopologyInterval: time.Minute, NetworkHealthInterval: time.Minute}))
	devices, _ := queries.ListDevices(db)
	n.maintain(devices, now)

	left := func(table string) map[string]bool {
		rows, err := db.Query(`SELECT device_id FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			out[id] = true
		}
		return out
	}
	if got := left("bridge_status"); !got["r3"] || !got["s1"] || !got["held"] {
		t.Errorf("bridge rows left = %v, want all three (20 min is within the floor)", got)
	}
	if got := left("bridge_vlans"); got["r3"] || !got["s1"] || !got["held"] {
		t.Errorf("vlan rows left = %v, want s1 (slow cadence) and held (paused)", got)
	}
	if got := left("interface_state"); got["r3"] || got["s1"] || !got["held"] {
		t.Errorf("interface rows left = %v, want only the paused device's", got)
	}
}

// When a pause ends the device's rows are as old as the pause. A first visit
// that cannot read the device (no pooled connection yet) must not prune
// them: each tier gets a whole stale window from the pause's end to be read
// again — and after it, rows still not refreshed go as usual.
func TestNetworkHealthKeepsRowsAfterAPause(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"held": devclass.C3}, "held")
	start := time.Now().UTC().Truncate(time.Second)
	if err := queries.UpsertBridgeStatus(db, &queries.BridgeStatus{ID: "held:bridge", DeviceID: "held", BridgeName: "bridge"}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertBridgeVLAN(db, &queries.BridgeVLAN{ID: "held:bridge:10", DeviceID: "held", BridgeName: "bridge", VLANIDs: "10"}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertInterfaceState(db, &queries.InterfaceState{ID: "held:ether1", DeviceID: "held", InterfaceName: "ether1"}); err != nil {
		t.Fatal(err)
	}
	for _, tb := range []string{"bridge_status", "bridge_vlans", "interface_state"} {
		if _, err := db.Exec(`UPDATE `+tb+` SET last_polled = ?`, sqlStamp(start.Add(-48*time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	n := NewNetworkHealthPoller(db, nil, ws.NewHub(), time.Minute)
	clock := &testClock{t: start}
	n.now = clock.now
	n.clock = newTierClock(NewScheduleRegistry())
	n.UseCadence(NewCadence(db, &config.Config{TopologyInterval: time.Minute, NetworkHealthInterval: time.Minute}))
	n.runner = func(string) routeros.Runner { return nil } // no pooled connection
	count := func() int {
		total := 0
		for _, tb := range []string{"bridge_status", "bridge_vlans", "interface_state"} {
			var c int
			if err := db.QueryRow(`SELECT COUNT(*) FROM ` + tb + ` WHERE device_id = 'held'`).Scan(&c); err != nil {
				t.Fatal(err)
			}
			total += c
		}
		return total
	}

	n.poll(context.Background()) // paused: never pruned
	if err := queries.ResumeDevicePolling(db, "held"); err != nil {
		t.Fatal(err)
	}
	clock.set(start.Add(time.Minute))
	n.poll(context.Background())
	if got := count(); got != 3 {
		t.Fatalf("rows left right after the pause = %d, want all 3 kept", got)
	}
	clock.set(start.Add(20 * time.Minute))
	n.poll(context.Background())
	if got := count(); got != 3 {
		t.Fatalf("rows left 20 min after the pause = %d, want all 3 (within the 30 min window)", got)
	}
	clock.set(start.Add(46 * time.Minute)) // past every window, still unread
	n.poll(context.Background())
	if got := count(); got != 0 {
		t.Fatalf("rows left after the windows = %d, want none", got)
	}
}

// wifiController scripts an AP / controller with the RouterOS 7 wifi package
// and the given clients in its registration table.
func wifiController(macs ...string) *fakeRunner {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		routeros.WifiProbeCommand:       {done: map[string]string{"ret": "1"}},
		routeros.DHCPServerProbeCommand: {done: map[string]string{"ret": "0"}},
		routeros.BridgeProbeCommand:     {done: map[string]string{"ret": "1"}},
		"/log/print":                    rep(),
	}}
	setRegistrations(f, macs...)
	return f
}

func setRegistrations(f *fakeRunner, macs ...string) {
	rows := make([]map[string]string, 0, len(macs))
	for _, m := range macs {
		rows = append(rows, map[string]string{"mac-address": m, "interface": "wifi1", "ssid": "lab", "signal": "-60"})
	}
	f.byCmd["/interface/wifi/registration-table/print"] = rep(rows...)
}

func wifiEvents(t *testing.T, db *sql.DB, event string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT mac_address, source FROM wifi_history WHERE event = ?`, event)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var mac, src string
		_ = rows.Scan(&mac, &src)
		out[mac] = src
	}
	return out
}

// Each controller is read on its own cadence and its latest read stands for
// it until the next: a client of a slow C1 AP is not absent between its
// reads, a client gone from a C3 AP leaves after 10 of that AP's reads, and a
// paused controller is never read while nothing of it ages.
func TestWifiTrackerPerControllerCadenceAbsenceAndPause(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"ap3": devclass.C3, "ap1": devclass.C1, "held": devclass.C3})
	const x, y, z = "02:00:00:00:A0:01", "02:00:00:00:A0:02", "02:00:00:00:A0:03"
	runners := map[string]*fakeRunner{"ap3": wifiController(x), "ap1": wifiController(y), "held": wifiController(z)}
	wt := NewWifiTracker(db, nil, ws.NewHub(), 30*time.Second)
	clock := &testClock{t: at(12, 0, 0)}
	wt.now = clock.now
	wt.clock = newTierClock(NewScheduleRegistry())
	wt.clock.phase = noJitter
	wt.UseCadence(NewCadence(db, nil))
	wt.runner = scriptedRunners(runners)
	regReads := func(id string) int { return runners[id].countCalls("/interface/wifi/registration-table/print") }

	wt.poll(context.Background())
	if joins := wifiEvents(t, db, "join"); len(joins) != 3 {
		t.Fatalf("joins after the first reads = %v, want x, y and z", joins)
	}
	if err := queries.PauseDevicePolling(db, "held", at(12, 0, 0).Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	heldCalls := len(runners["held"].calls)

	setRegistrations(runners["ap3"]) // x is gone from the C3 AP
	for ts := at(12, 0, 30); !ts.After(at(13, 0, 0)); ts = ts.Add(30 * time.Second) {
		clock.set(ts)
		wt.poll(context.Background())
		if ts.Equal(at(12, 5, 0)) {
			if left := wifiEvents(t, db, "leave"); len(left) != 0 {
				t.Fatalf("leaves before 10 of the AP's reads: %v", left)
			}
		}
	}
	if got := regReads("ap3"); got != 121 {
		t.Errorf("C3 AP registration reads over an hour at 30 s = %d, want 121", got)
	}
	if got := regReads("ap1"); got != 13 {
		t.Errorf("C1 AP registration reads over an hour at 5 min = %d, want 13", got)
	}
	if len(runners["held"].calls) != heldCalls {
		t.Errorf("a paused controller was read: %q", runners["held"].calls[heldCalls:])
	}
	left := wifiEvents(t, db, "leave")
	if left[x] != "absence" || len(left) != 1 {
		t.Errorf("leaves = %v, want only x (absence) — y is on a slow AP, z on a paused one", left)
	}
}

// Client discovery reads ARP on the class cadence floored by
// client_discovery_interval (C3: 15 min, C2: 30 min), DHCP leases only where
// the plan has them (C3), and nothing on a paused device.
func TestClientDiscoveryPerClassCadenceAndPause(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"r3": devclass.C3, "s2": devclass.C2, "held": devclass.C3}, "held")
	device := func(mac string) *fakeRunner {
		return &fakeRunner{byCmd: map[string]fakeReply{
			"/ip/arp/print":               rep(map[string]string{"address": "192.0.2.50", "mac-address": mac}),
			"/ip/dhcp-server/lease/print": rep(),
			"/ip/dhcp-server/print":       rep(),
		}}
	}
	runners := map[string]*fakeRunner{"r3": device("02:00:00:00:B0:01"), "s2": device("02:00:00:00:B0:02"), "held": device("02:00:00:00:B0:03")}
	cdp := NewClientDiscoveryPoller(db, nil, nil, 15*time.Minute)
	clock := &testClock{t: at(12, 0, 0)}
	cdp.now = clock.now
	cdp.clock = newTierClock(NewScheduleRegistry())
	cdp.clock.phase = noJitter
	cdp.UseCadence(NewCadence(db, nil))
	cdp.runner = scriptedRunners(runners)
	reads := func(id string) [2]int {
		return [2]int{runners[id].countCalls("/ip/arp/print"), runners[id].countCalls("/ip/dhcp-server/lease/print")}
	}

	for _, st := range []struct {
		at     time.Duration
		r3, s2 [2]int
	}{
		{0, [2]int{1, 1}, [2]int{1, 0}},
		{5 * time.Minute, [2]int{1, 1}, [2]int{1, 0}},
		{15 * time.Minute, [2]int{2, 2}, [2]int{1, 0}},
		{30 * time.Minute, [2]int{3, 3}, [2]int{2, 0}},
	} {
		clock.set(at(12, 0, 0).Add(st.at))
		cdp.poll(context.Background())
		if got := reads("r3"); got != st.r3 {
			t.Errorf("+%v: C3 ARP/DHCP reads = %v, want %v", st.at, got, st.r3)
		}
		if got := reads("s2"); got != st.s2 {
			t.Errorf("+%v: C2 ARP/DHCP reads = %v, want %v", st.at, got, st.s2)
		}
	}
	if len(runners["held"].calls) != 0 {
		t.Errorf("a paused device was read: %q", runners["held"].calls)
	}
	if mac, err := queries.GetMACLookup(db, "02:00:00:00:B0:02"); err != nil || mac.IPAddress != "192.0.2.50" {
		t.Errorf("mac_lookup for the C2 device's client = %+v, %v", mac, err)
	}
}

// The info refresh (Tier 6) runs hourly per device, at once for a device not
// refreshed since startup, again every classProbeRetry for an online device
// that is still unclassified, and never for a paused one.
func TestInfoRefreshScheduleAndPause(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"c3": devclass.C3, "new": "", "held": devclass.C3}, "held")
	mustCreateDevice(t, db, &queries.Device{ID: "off", Identity: "off", Address: "198.51.100.9", Status: "offline"})
	clock := &testClock{t: at(12, 0, 0)}
	m := &Manager{db: db, cadence: NewCadence(db, nil), inventory: newTierClock(NewScheduleRegistry()), classes: newClassGate(), now: clock.now}
	m.inventory.phase = noJitter
	refreshed := map[string]int{}
	m.infoRefresh = func(dev queries.Device) {
		refreshed[dev.ID]++
		end := m.clock()
		m.inventory.ran(dev.ID, devclass.TierInventory, runInterval(m.cadence, dev, devclass.TierInventory, end), end)
	}
	for _, st := range []struct {
		at                 time.Duration
		c3, newDev, offDev int
	}{
		{0, 1, 1, 1},
		{time.Minute, 1, 1, 1},
		{5 * time.Minute, 1, 2, 1}, // still unclassified and online: probed again
		{time.Hour, 2, 3, 2},
	} {
		clock.set(at(12, 0, 0).Add(st.at))
		m.refreshDueInfo(context.Background(), clock.now())
		if refreshed["c3"] != st.c3 || refreshed["new"] != st.newDev || refreshed["off"] != st.offDev {
			t.Errorf("+%v: refreshes %v, want c3 %d, new %d, off %d", st.at, refreshed, st.c3, st.newDev, st.offDev)
		}
	}
	if refreshed["held"] != 0 {
		t.Errorf("a paused device was refreshed %d times", refreshed["held"])
	}
}

// Connectivity probes and scheduled speed tests skip a paused probing device
// without writing a sample (paused time is a gap); an offline one still gets
// its error sample.
func TestConnectivityAndSpeedTestsSkipPausedDevices(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "held", Identity: "held", Address: "192.0.2.30"})
	mustCreateDevice(t, db, &queries.Device{ID: "down", Identity: "down", Address: "192.0.2.31", Status: "offline"})
	if err := queries.PauseDevicePolling(db, "held", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"held", "down"} {
		if err := queries.CreatePingTarget(db, &queries.PingTarget{ID: "t-" + d, Kind: "internet", Address: "198.51.100.1", DeviceID: d, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if err := queries.CreateSpeedTest(db, &queries.SpeedTest{ID: "s-" + d, DeviceID: d, URL: "http://198.51.100.1/x", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	NewConnectivityPoller(db, routeros.NewPool(false), ws.NewHub(), time.Minute, false).poll(context.Background())
	NewSpeedTestPoller(db, ws.NewHub(), false, time.Hour).poll(context.Background())

	count := func(table, col, id string) int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+col+` = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("ping_samples", "target_id", "t-held") != 0 || count("speed_samples", "test_id", "s-held") != 0 {
		t.Error("a paused probing device produced samples")
	}
	if count("ping_samples", "target_id", "t-down") != 1 || count("speed_samples", "test_id", "s-down") != 1 {
		t.Error("an offline probing device must still record its error sample")
	}
}

// The firmware check (scheduled and "Check All") skips a paused device and
// says so once.
func TestFirmwareCheckSkipsPausedDevice(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "fw-held", Identity: "fw-held", Address: "192.0.2.40"})
	if err := queries.PauseDevicePolling(db, "fw-held", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	RunFirmwareCheck(context.Background(), db, routeros.NewPool(false), ws.NewHub())
	RunFirmwareCheck(context.Background(), db, routeros.NewPool(false), ws.NewHub())
	if got := strings.Count(buf.String(), "firmware check skips fw-held"); got != 1 {
		t.Errorf("skip logged %d times, want once per pause:\n%s", got, buf.String())
	}
}
