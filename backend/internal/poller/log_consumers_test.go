package poller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// growingLog is a device log buffer that gains lines over a test.
type growingLog struct {
	f    *fakeRunner
	rows []map[string]string
}

func (g *growingLog) add(tm, topics, msg string) {
	g.rows = append(g.rows, logRow(fmt.Sprintf("*%X", len(g.rows)+1), tm, topics, msg))
	g.f.byCmd["/log/print"] = rep(g.rows...)
}

// A loop logged at any time after startup is recorded, however long the
// device's log stayed quiet before: the consumers' fingerprint maps are
// pruned after 30 min idle, and a first-run guard keyed on them used to drop
// the first batch after every prune — on a bridge-quiet device about every
// third read (C3) or nearly every read (C1).
func TestBridgeLoopsRecordedAfterQuietPeriods(t *testing.T) {
	for _, class := range []devclass.Class{devclass.C3, devclass.C1} {
		t.Run(string(class), func(t *testing.T) {
			db := pollerTestDB(t)
			classFleet(t, db, map[string]devclass.Class{"sw": class})
			run := bridgeDevice()
			g := &growingLog{f: run}
			g.add("12:00:00", "system,info", "router started")
			n := NewNetworkHealthPoller(db, nil, ws.NewHub(), time.Minute)
			clock := &testClock{t: at(12, 0, 0)}
			n.now = clock.now
			n.clock = newTierClock(NewScheduleRegistry())
			n.clock.phase = noJitter
			n.UseCadence(NewCadence(db, &config.Config{TopologyInterval: time.Minute, NetworkHealthInterval: time.Minute}))
			n.runner = scriptedRunners(map[string]*fakeRunner{"sw": run})
			inject := map[time.Time]string{
				at(12, 7, 30): "bridge: loop detected on ether5",
				at(12, 50, 0): "bridge: loop detected on ether7",
			}
			for ts := at(12, 0, 0); !ts.After(at(13, 0, 0)); ts = ts.Add(5 * time.Second) {
				clock.set(ts)
				if m, ok := inject[ts]; ok {
					g.add(ts.Format("15:04:05"), "bridge,info", m)
				}
				n.poll(context.Background())
			}
			var c int
			if err := db.QueryRow(`SELECT COUNT(*) FROM loop_events WHERE event_type = 'loop_detected'`).Scan(&c); err != nil {
				t.Fatal(err)
			}
			if c != 2 {
				t.Errorf("recorded %d of the 2 loops logged after startup", c)
			}
		})
	}
}

// An AP whose legacy wireless package is swapped for wifi (same RouterOS
// version, so the version check never re-probes): the first trapping
// registration read has the device re-probed, the new menu is read from then
// on, a new client joins and the client still there is not reported gone.
func TestWifiTrackerReprobesAfterPackageSwap(t *testing.T) {
	const x, y = "02:00:00:00:A0:01", "02:00:00:00:A0:02"
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"ap": devclass.C3})
	run := &fakeRunner{byCmd: map[string]fakeReply{
		routeros.WirelessProbeCommand:                  {done: map[string]string{"ret": "1"}},
		routeros.DHCPServerProbeCommand:                {done: map[string]string{"ret": "0"}},
		routeros.BridgeProbeCommand:                    {done: map[string]string{"ret": "1"}},
		"/log/print":                                   rep(),
		"/interface/wireless/registration-table/print": rep(map[string]string{"mac-address": x, "interface": "wlan1", "signal-strength": "-60"}),
	}}
	wt := NewWifiTracker(db, nil, ws.NewHub(), 30*time.Second)
	clock := &testClock{t: at(12, 0, 0)}
	wt.now = clock.now
	wt.clock = newTierClock(NewScheduleRegistry())
	wt.clock.phase = noJitter
	wt.UseCadence(NewCadence(db, nil))
	wt.runner = scriptedRunners(map[string]*fakeRunner{"ap": run})
	wt.poll(context.Background())
	if joins := wifiEvents(t, db, "join"); len(joins) != 1 {
		t.Fatalf("joins = %v, want x", joins)
	}

	delete(run.byCmd, routeros.WirelessProbeCommand)
	delete(run.byCmd, "/interface/wireless/registration-table/print")
	run.byCmd[routeros.WifiProbeCommand] = fakeReply{done: map[string]string{"ret": "1"}}
	setRegistrations(run, x, y)
	for ts := at(12, 10, 0); !ts.After(at(14, 0, 0)); ts = ts.Add(30 * time.Second) {
		clock.set(ts)
		wt.poll(context.Background())
	}
	if n := run.countCalls("/interface/wireless/registration-table/print"); n != 2 {
		t.Errorf("trapping legacy reads = %d, want the first read and the one after it only", n)
	}
	if n := run.countCalls("/interface/wifi/registration-table/print"); n < 200 {
		t.Errorf("wifi registration reads = %d, want the new menu read from the re-probe on", n)
	}
	if joins := wifiEvents(t, db, "join"); joins[y] == "" {
		t.Errorf("joins = %v, want y from the new menu", joins)
	}
	if left := wifiEvents(t, db, "leave"); left[x] != "" {
		t.Errorf("leaves = %v: x never left", left)
	}
}

// A roam logged after a quiet hour on the controller is recorded from the
// log: the same pruned-map first-run guard used to drop it for good (the
// registration snapshots never record a roam).
func TestWifiRoamRecordedAfterQuietHour(t *testing.T) {
	const x = "02:00:00:00:A0:01"
	for _, c := range []struct {
		class devclass.Class
		roam  time.Time
	}{{devclass.C3, at(12, 10, 0)}, {devclass.C3, at(12, 50, 0)}, {devclass.C1, at(12, 40, 0)}} {
		t.Run(fmt.Sprintf("%s at %s", c.class, c.roam.Format("15:04")), func(t *testing.T) {
			db := pollerTestDB(t)
			classFleet(t, db, map[string]devclass.Class{"ap": c.class})
			run := wifiController(x)
			g := &growingLog{f: run}
			g.add("11:59:00", "wireless,info", x+"@apA(lab) connected, signal strength -60")
			wt := NewWifiTracker(db, nil, ws.NewHub(), 30*time.Second)
			clock := &testClock{t: at(12, 0, 0)}
			wt.now = clock.now
			wt.clock = newTierClock(NewScheduleRegistry())
			wt.clock.phase = noJitter
			wt.UseCadence(NewCadence(db, nil))
			wt.runner = scriptedRunners(map[string]*fakeRunner{"ap": run})
			for ts := at(12, 0, 0); !ts.After(at(13, 0, 0)); ts = ts.Add(30 * time.Second) {
				clock.set(ts)
				if ts.Equal(c.roam) {
					g.add(ts.Format("15:04:05"), "wireless,info", x+"@apA(lab) roamed to "+x+"@apB(lab), signal strength -60")
				}
				wt.poll(context.Background())
			}
			if src := wifiEvents(t, db, "roam")[x]; src != "log" {
				t.Errorf("roam row source = %q, want one from the log", src)
			}
		})
	}
}
