package poller

import (
	"context"
	"database/sql"
	"io"
	"log"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/hygiene/hygienetest"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// The call-plan harness: every scheduled device-reading loop of the poller
// manager, wired as Manager.Start wires them, run against a small fleet of
// fake RouterOS devices on a simulated clock.
//
// What is real: the manager's own wiring (NewManager), every loop's tick,
// the connection pool and the RouterOS sessions — each device is a
// routerostest endpoint on a loopback address, so what the harness records
// is what went over the wire, whichever helper sent it.
//
// What is simulated: time. Each loop keeps the start delay and the sleep
// between two wake-ups that its Run method has, on a clock the harness
// advances from one wake-up to the next; a command takes no time. Nothing
// here sleeps to let time pass, and what is sent never depends on the wall
// clock or on which goroutine runs first: one loop wakes at a time, and each
// device has its own session, which takes one command at a time.
//
// Two sets of golden files read the same hour (fleetHourOf): the call plans
// (callplan_test.go: what each device was sent) and the DB state
// (dbstate_test.go: what the loops stored). The hour is simulated once per
// test binary — under the race detector it is the slowest test there is.

// fleetHour is the simulated hour as the tests read it, captured before the
// test it ran in ends and takes the database and the fake devices with it.
type fleetHour struct {
	// done: the hour ran to its end and everything below was captured.
	done bool
	// ranIn is the test the hour ran in, skipped whether that test was
	// skipped instead (a platform without the loopback addresses).
	ranIn   string
	skipped bool

	hour    time.Duration
	devices []*planDevice
	calls   map[string][]planCall
	// problems: what checkOutcome found wrong with the hour.
	problems []string
	// state is what the loops stored, normalised (dbstate_test.go).
	state *dbState
}

var (
	fleetOnce sync.Once
	fleet     fleetHour
)

// fleetHourOf returns the fleet's hour, simulating it in t if no test of
// this binary has yet. A test that finds the hour failed or skipped in
// another test fails or skips too, and says where to look.
func fleetHourOf(t *testing.T) *fleetHour {
	t.Helper()
	fleetOnce.Do(func() {
		fleet.ranIn = t.Name()
		defer func() { fleet.skipped = t.Skipped() }()
		wallFrom := time.Now()
		h := newCallPlan(t)
		before := h.captureSeed()
		// One minute past the hour: the hourly inventory pass comes round
		// then, and with it the only command of the pool's own (its
		// keep-alive).
		h.run(h.hour + time.Minute)
		fleet.hour = h.hour
		fleet.devices = h.devices
		fleet.calls = make(map[string][]planCall, len(h.devices))
		for _, d := range h.devices {
			fleet.calls[d.id] = d.calls()
		}
		fleet.problems = h.checkOutcome()
		fleet.state = h.dumpState(before, wallFrom, time.Now())
		fleet.done = true
	})
	if !fleet.done {
		if fleet.skipped {
			t.Skipf("the fleet's hour was skipped in %s", fleet.ranIn)
		}
		t.Fatalf("the fleet's hour did not run to its end in %s: see that test's failure", fleet.ranIn)
	}
	return &fleet
}

// callPlanStart is when the simulated NMS starts: a fixed instant on a full
// minute, so minute n of the plan is a calendar minute.
//
// It lies in the past on purpose. Some rows the pollers store are stamped by
// SQLite (CURRENT_TIMESTAMP) — the wall clock — and every staleness rule
// compares them with the simulated now: seen from an earlier instant such a
// row is always current, whenever the test runs. The rules that expire rows
// are therefore not exercised here; in an hour of healthy devices none of
// them decides what a device is sent.
var callPlanStart = time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

// planMember is one device of the simulated fleet.
type planMember struct {
	id      string // device id and identity
	fixture string // its hygienetest personality
	host    string // loopback address its fake listens on (devices.address is unique)
	// Where it hangs in the network: its own port towards parent, and the
	// parent's port towards it. The router has no parent.
	uplink, parent, parentPort string
	// routeVia is the interface its default route leaves through.
	routeVia string
}

// planFleet is the fleet: a router, the 48-port switch behind it, and a
// smaller switch and two access points behind that.
var planFleet = []planMember{
	{id: "rtr", fixture: "router_clean", host: "127.0.0.31", routeVia: "ether1"},
	{id: "sw48", fixture: "switch48_clean", host: "127.0.0.32", uplink: "sfp-sfpplus1", parent: "rtr", parentPort: "sfp-sfpplus1", routeVia: "vlan-mgmt"},
	{id: "sw24", fixture: "switch24_clean", host: "127.0.0.33", uplink: "sfp-sfpplus1", parent: "sw48", parentPort: "sfp-sfpplus2", routeVia: "vlan-mgmt"},
	{id: "ap-ax", fixture: "ap_clean", host: "127.0.0.34", uplink: "ether1", parent: "sw48", parentPort: "ether1", routeVia: "bridge"},
	{id: "ap-ac", fixture: "ap_small_clean", host: "127.0.0.35", uplink: "ether1", parent: "sw48", parentPort: "ether2", routeVia: "bridge"},
}

const (
	// planWatchedMAC is the wireless client the fleet's one watch follows: it
	// is registered on ap-ax (the fixture's registration table) and has a
	// lease and an ARP entry on the router.
	planWatchedMAC = "02:00:5E:10:00:01"
	// planInternetTarget is the address of the fleet's one internet ping
	// target, and planWAN the router's next hop: public as far as the role
	// classifier is concerned (documentation range, not RFC 1918).
	planInternetTarget = "198.51.100.53"
	planWAN            = "198.51.100.1"
)

// planDevice is a fleet member at run time.
type planDevice struct {
	planMember
	p    hygienetest.Personality
	fake *routerostest.Device
	// dials counts the liveness dials that reached the device in the hour.
	dials int
}

// planLoop is one of the manager's loops: when it wakes next, and what a
// wake-up does. tick returns the next wake-up.
type planLoop struct {
	name string
	next time.Time
	tick func(now time.Time) time.Time
}

// callPlan is a fleet with its manager, ready to run.
type callPlan struct {
	t       *testing.T
	db      *sql.DB
	clock   *testClock
	pool    *routeros.Pool
	m       *Manager
	devices []*planDevice
	loops   []*planLoop
	// hour is how long the plan covers (the liveness dials are counted in it).
	hour time.Duration
}

func (h *callPlan) device(id string) *planDevice {
	for _, d := range h.devices {
		if d.id == id {
			return d
		}
	}
	h.t.Fatalf("call plan: no device %q in the fleet", id)
	return nil
}

// resourceRow is what the personality's /system/resource/print answers.
func resourceRow(t *testing.T, p hygienetest.Personality) map[string]string {
	t.Helper()
	rows := p.Replies["/system/resource/print"].Rows
	if len(rows) != 1 {
		t.Fatalf("call plan: personality %s answers /system/resource/print with %d rows, want 1", p.Name, len(rows))
	}
	return rows[0]
}

// newCallPlan starts the fake devices, stores the fleet the way a running
// NMS knows it — inventory read, class probed, online — and wires the loops.
// The process it simulates is one that has just been restarted: no session
// is open and nothing has been read yet.
func newCallPlan(t *testing.T) *callPlan {
	t.Helper()
	h := &callPlan{t: t, db: pollerTestDB(t), clock: &testClock{t: callPlanStart}, pool: routeros.NewPool(), hour: time.Hour}
	t.Cleanup(h.pool.CloseAll)

	// The loops log every pass; an hour of it is noise in a test run.
	orig := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(orig) })
	// The loops built by NewManager publish their schedule process-wide.
	t.Cleanup(func() { Schedule().Retain(map[string]bool{}) })

	for _, mb := range planFleet {
		fake, err := routerostest.NewDeviceOn(t, mb.host)
		if err != nil {
			if runtime.GOOS == "linux" {
				t.Fatalf("call plan: fake device on %s: %v", mb.host, err)
			}
			t.Skipf("call plan: the fleet needs one loopback address per device and %s is not local here (%v); "+
				"every address of 127.0.0.0/8 is on Linux", mb.host, err)
		}
		fake.UseClock(h.clock.now)
		h.devices = append(h.devices, &planDevice{planMember: mb, p: withPortTables(hygienetest.Load(t, mb.fixture)), fake: fake})
	}

	rtr := h.device("rtr")
	for _, d := range h.devices {
		// What the fixture cannot know: the device's name and its place in
		// this fleet — the neighbours it hears and where its default route
		// points. The router's leaves through its WAN port; everything else
		// routes via the router, a managed address, which is what makes a
		// switch a switch and an access point an access point for the role
		// classifier.
		gateway := rtr.fake.Host
		if d == rtr {
			gateway = planWAN
		}
		d.p = d.p.
			With("/system/identity/print", map[string]string{"name": d.id}).
			With("/ip/neighbor/print", h.neighborRows(d)...).
			With("/ip/route/print", map[string]string{
				".id": "*80000001", "dst-address": "0.0.0.0/0", "gateway": gateway,
				"immediate-gw": gateway + "%" + d.routeVia, "distance": "1", "scope": "30", "routing-table": "main",
				"active": "true", "static": "true", "dynamic": "false", "disabled": "false",
			})
		d.p.ScriptAll(d.fake)
		// Its counters, CPU, memory and uptime move with the clock.
		d.animate(h.clock.now)

		res := resourceRow(t, d.p)
		facts := devclass.ParseResourceFacts(res)
		if probed := devclass.Classify(facts); probed != d.p.Class {
			t.Fatalf("call plan: personality %s says class %s, but its resource print probes as %s", d.p.Name, d.p.Class, probed)
		}
		if err := queries.CreateDevice(h.db, &queries.Device{
			ID: d.id, Identity: d.id, Address: d.fake.Host, APIPort: d.fake.Port,
			Username: d.p.NMSUser, PasswordEnc: "call-plan",
			Platform: res["platform"], Board: res["board-name"], ROSVersion: res["version"], Architecture: res["architecture-name"],
			Status: "online",
		}); err != nil {
			t.Fatalf("call plan: create %s: %v", d.id, err)
		}
		if err := queries.SetDeviceClass(h.db, d.id, d.p.Class, facts, callPlanStart.Add(-time.Hour)); err != nil {
			t.Fatalf("call plan: class of %s: %v", d.id, err)
		}
	}

	// One internet target and one client watch, both probing from the router
	// (the only kind of device the API lets a probe be put on).
	for _, target := range []*queries.PingTarget{
		{ID: "internet", Kind: "internet", Label: "internet", Address: planInternetTarget, DeviceID: "rtr", Enabled: true},
		{ID: "watch", Kind: "client", Label: "laptop", MACAddress: planWatchedMAC, DeviceID: "rtr", Enabled: true},
	} {
		if err := queries.CreatePingTarget(h.db, target); err != nil {
			t.Fatalf("call plan: ping target %s: %v", target.ID, err)
		}
	}

	h.wire()
	return h
}

// neighborRows is what d hears on MNDP: the device it hangs off, and the
// devices that hang off it.
func (h *callPlan) neighborRows(d *planDevice) []map[string]string {
	row := func(localPort string, n *planDevice, remotePort string) map[string]string {
		res, far := resourceRow(h.t, n.p), planPersonality{n.p}
		return map[string]string{
			"interface": localPort, "address": n.fake.Host, "mac-address": far.mac(far.portIndex(remotePort)),
			"identity": n.id, "platform": "MikroTik", "version": res["version"], "board": n.p.Board,
			"interface-name": remotePort, "discovered-by": "mndp", "age": "12s", "unpack": "none",
		}
	}
	var rows []map[string]string
	if d.parent != "" {
		rows = append(rows, row(d.uplink, h.device(d.parent), d.parentPort))
	}
	for _, child := range h.devices {
		if child.parent == d.id {
			rows = append(rows, row(child.parentPort, child, child.uplink))
		}
	}
	return rows
}

// wire builds the manager and the loops Manager.Start builds, with the same
// wiring, and gives each its place on the simulated clock: the start delay,
// the tick and the sleep of its run function. What Start does that is left
// out: the retention job, the firmware release feed and the alert engine
// (database or internet only, they send a device nothing), the live traffic
// stream, speed tests and the flow collector (on demand or off in this
// scenario). TestCallPlanWiringIsPinned fails when Start or one of the run
// functions changes, so the two are kept alike by hand but not by luck.
func (h *callPlan) wire() {
	cfg := &config.Config{
		Pollers:        config.PollersLocal,
		HealthInterval: 30 * time.Second, TopologyInterval: 60 * time.Second, NetworkHealthInterval: 60 * time.Second,
		FirmwareInterval: 24 * time.Hour, RetentionInterval: time.Hour,
	}
	hub := ws.NewHub()
	m := NewManager(h.db, h.pool, hub, cfg)
	h.m = m
	now := h.clock.now
	inline := func(f func()) { f() }
	ctx := context.Background()

	// The seams of the loops NewManager built: the clock, zero jitter (the
	// plan shows the nominal cadences; devclass.Phase would shift each device
	// by its own ±10 %), reads run inline instead of on a goroutine, and no
	// real-time stagger between two devices of one topology tick.
	m.now, m.stagger = now, 0
	m.inventory.phase, m.topoClock.phase = noJitter, noJitter
	m.portStats.now, m.portStats.phase, m.portStats.hostClock.phase = now, noJitter, noJitter
	m.load.started = callPlanStart
	m.cpu.now, m.cpu.phase, m.cpu.postClock.phase, m.cpu.spawn = now, noJitter, noJitter, inline
	// "When did the device's last command end" is the routeros observer's
	// answer in production — on the wall clock. Here the fake device says
	// when it last heard from the NMS, on the simulated one.
	m.cpu.lastCommandEnd = func(id string) time.Time { return h.device(id).fake.LastExchangeAt() }
	m.audit.now, m.audit.clock.phase, m.audit.spawn = now, noJitter, inline
	// The address the NMS's sessions come from, as the personalities' session
	// tables list it (a routing-table lookup of the test host otherwise).
	m.audit.self = func(*queries.Device, time.Time) []netip.Addr {
		return []netip.Addr{netip.MustParseAddr(hygienetest.NMSAddress)}
	}
	// What Start does first: every command and pooled dial is counted against
	// its device (device_minutes.nms_*) — here on the simulated clock.
	routeros.SetObserver(simObserver{rec: m.load, now: now})
	h.t.Cleanup(func() { routeros.SetObserver(nil) })

	// The loops Start builds — see Manager.Start.
	logs := NewLogReader()
	wifi := NewWifiTracker(h.db, h.pool, hub, 30*time.Second)
	wifi.UseLogReader(logs)
	wifi.UseCapabilities(m.caps)
	wifi.UseCadence(m.cadence)
	wifi.now, wifi.clock.phase = now, noJitter

	// Start hands this one a DNS resolver for the clients' names; names are
	// not device I/O, and the test must not query a resolver.
	clients := NewClientDiscoveryPoller(h.db, h.pool, nil, 15*time.Minute)
	clients.UseCadence(m.cadence)
	clients.now, clients.clock.phase = now, noJitter

	health := NewNetworkHealthPoller(h.db, h.pool, hub, cfg.NetworkHealthInterval)
	health.UseFloor(func() time.Duration { return m.floorOf(devclass.TierBridgePorts, now()) })
	health.UseLogReader(logs)
	health.UseInterfaceSnapshots(m.portStats)
	health.UseCadence(m.cadence)
	health.now, health.clock.phase = now, noJitter

	connectivity := NewConnectivityPoller(h.db, h.pool, hub, defaultConnectivityInterval)
	connectivity.UseWifiReadings(wifi)

	// What the Run methods do before their first wake-up.
	m.portStats.startupRollup()
	wifi.restoreState()
	if err := health.ports.RestoreFromDB(h.db); err != nil {
		h.t.Fatalf("call plan: restore port state: %v", err)
	}

	// Each loop's Run, as (start delay, one wake-up, the sleep after it). On
	// a tie the loops wake in the order Start launches them.
	every := func(d time.Duration, f func()) func(time.Time) time.Time {
		return func(t time.Time) time.Time { f(); return t.Add(d) }
	}
	// The sleep of the two loops that ask their tick when to look again.
	until := func(wake, t time.Time, floor, ceil time.Duration) time.Time {
		return t.Add(min(max(wake.Sub(t), floor), ceil))
	}
	h.add("cpu", cpuStartupDelay, func(t time.Time) time.Time {
		return until(m.cpu.safeTick(t), t, 200*time.Millisecond, pollerTick)
	})
	h.add("liveness", 0, func(t time.Time) time.Time {
		h.livenessTick(ctx, t)
		return t.Add(cfg.HealthInterval)
	})
	h.add("info", 8*time.Second, every(pollerTick, func() { m.refreshDueInfo(ctx, now()) }))
	h.add("topology", 5*time.Second, every(pollerTick, func() { m.safePollTopology(ctx) }))
	h.add("counters", portStatsStartupDelay, func(t time.Time) time.Time {
		base := PortStatsInterval(h.db)
		return until(m.portStats.safeCycle(ctx, base), t, time.Second, base)
	})
	h.add("hosts", portHostsStartupDelay, every(pollerTick, func() { m.portStats.safeHostsTick(ctx) }))
	h.add("wifi", 15*time.Second, every(pollerTick, func() { wifi.safePoll(ctx) }))
	h.add("clients", 10*time.Second, every(pollerTick, func() { clients.safePoll(ctx) }))
	h.add("health", 20*time.Second, every(pollerTick, func() { health.safePoll(ctx) }))
	h.add("connectivity", 15*time.Second, func(t time.Time) time.Time {
		connectivity.safePoll(ctx)
		return t.Add(connectivity.currentInterval())
	})
	h.add("audit", auditLoopDelay, func(t time.Time) time.Time {
		return until(m.audit.safeTick(t), t, 200*time.Millisecond, pollerTick)
	})
}

// simObserver hands the routeros observer's events to the device-load
// recorder on the simulated clock. The observer stamps them with the wall
// clock; here a command takes no simulated time, so it ends at the moment it
// is sent — the minute the call plan counts it in — and the wall time it is
// charged is a model of its cost: a millisecond per reply row, at least one.
type simObserver struct {
	rec *LoadRecorder
	now func() time.Time
}

func (o simObserver) Command(ev routeros.CommandEvent) {
	ev.Wall = time.Duration(max(ev.Rows, 1)) * time.Millisecond
	ev.Start = o.now().Add(-ev.Wall)
	o.rec.Command(ev)
}

func (o simObserver) Session(ev routeros.SessionEvent) {
	ev.At = o.now()
	o.rec.Session(ev)
}

func (h *callPlan) add(name string, delay time.Duration, tick func(time.Time) time.Time) {
	h.loops = append(h.loops, &planLoop{name: name, next: callPlanStart.Add(delay), tick: tick})
}

// livenessTick is one pass of the health loop: a TCP dial to every device's
// API port. It waits until each fake has accepted its dial — the accept is
// the device's side of it and happens on another goroutine — so the count is
// what reached the devices, not what the harness expects.
func (h *callPlan) livenessTick(ctx context.Context, now time.Time) {
	before := make([]int, len(h.devices))
	for i, d := range h.devices {
		before[i] = d.fake.Conns()
	}
	h.m.pollAllDevices(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for i, d := range h.devices {
		for d.fake.Conns() == before[i] {
			if time.Now().After(deadline) {
				h.t.Fatalf("call plan: the liveness dial of %s at +%s did not reach the device", d.id, now.Sub(callPlanStart))
			}
			runtime.Gosched()
		}
		if got := d.fake.Conns() - before[i]; got != 1 {
			h.t.Fatalf("call plan: %s got %d connections in one liveness pass, want 1", d.id, got)
		}
		if now.Sub(callPlanStart) < h.hour {
			d.dials++
		}
	}
}

// run advances the clock from wake-up to wake-up until d has passed since
// the start: each time, the loop that is due first (on a tie, the one Start
// launches first) wakes at exactly its time and says when it wakes next.
func (h *callPlan) run(d time.Duration) {
	end := callPlanStart.Add(d)
	for {
		due := h.loops[0]
		for _, l := range h.loops[1:] {
			if l.next.Before(due.next) {
				due = l
			}
		}
		if !due.next.Before(end) {
			return
		}
		h.clock.set(due.next)
		next := due.tick(due.next)
		if !next.After(due.next) {
			h.t.Fatalf("call plan: the %s loop asked to wake again at once (at +%s)", due.name, due.next.Sub(callPlanStart))
		}
		due.next = next
	}
}
