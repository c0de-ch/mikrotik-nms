package poller

import (
	"database/sql"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/mikrotik-nms/backend/internal/alerts"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/hygiene"
	"github.com/mikrotik-nms/backend/internal/hygiene/hygienetest"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// sentCommand is one command a test device received, and when.
type sentCommand struct {
	at      time.Time
	command string
}

// auditHarness is an Auditor on a fake clock: devices that answer like a
// hygienetest personality, a settable CPU sample per device, reads run
// inline, no start-up spread and no jitter.
type auditHarness struct {
	t     *testing.T
	a     *Auditor
	db    *sql.DB
	clock *testClock
	cs    *stubCadence
	reg   *ScheduleRegistry

	mu      sync.Mutex
	devices map[string]hygienetest.Personality
	down    map[string]bool // no pooled session
	fail    map[string]map[string]error
	sent    map[string][]sentCommand
	cpu     map[string]CPUReading
	self    []netip.Addr
}

// harnessRunner is a device's link: the personality's answers, with every
// command timestamped by the fake clock.
type harnessRunner struct {
	h  *auditHarness
	id string
}

func (r harnessRunner) Run(command string, args ...string) (*ros.Reply, error) {
	r.h.mu.Lock()
	p := r.h.devices[r.id]
	err := r.h.fail[r.id][command]
	r.h.sent[r.id] = append(r.h.sent[r.id], sentCommand{at: r.h.clock.now(), command: strings.TrimSpace(command + " " + strings.Join(args, " "))})
	r.h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return p.Runner().Run(command, args...)
}

func newAuditHarness(t *testing.T) *auditHarness {
	t.Helper()
	db := pollerTestDB(t)
	h := &auditHarness{
		t: t, db: db, clock: &testClock{t: at(12, 0, 0)}, reg: NewScheduleRegistry(),
		cs:      &stubCadence{iv: map[devclass.Tier]time.Duration{devclass.TierAudit: 6 * time.Hour}, paused: map[string]bool{}},
		devices: map[string]hygienetest.Personality{}, down: map[string]bool{}, fail: map[string]map[string]error{},
		sent: map[string][]sentCommand{}, cpu: map[string]CPUReading{},
		self: []netip.Addr{netip.MustParseAddr(hygienetest.NMSAddress)},
	}
	// The device the others hang off: the far end of their trunks and the
	// gateway of the access points. It is offline, so nothing runs on it.
	mustCreateDevice(t, db, &queries.Device{ID: "core", Identity: "core", Address: "192.0.2.1", Status: "offline"})
	a := NewAuditor(db, nil, nil, nil, nil)
	a.UseCadence(h.cs)
	a.UseCollectorPort(2055)
	a.now = h.clock.now
	a.spawn = func(f func()) { f() }
	a.spread = func(string) time.Duration { return 0 }
	a.pollingAllowed = func() error { return nil }
	a.clock = newTierClock(h.reg)
	a.clock.phase = noJitter
	a.runner = func(id string) routeros.Runner {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.devices[id]; !ok || h.down[id] {
			return nil
		}
		return harnessRunner{h: h, id: id}
	}
	a.cpu = func(id string) (CPUReading, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		r, ok := h.cpu[id]
		return r, ok
	}
	a.ifaces = func(id string) (DeviceIfaces, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		p, ok := h.devices[id]
		if !ok {
			return DeviceIfaces{}, false
		}
		d := DeviceIfaces{At: h.clock.now(), EthernetAt: h.clock.now()}
		for _, port := range p.Ports {
			row := routeros.InterfaceSnapshot{InterfaceStats: routeros.InterfaceStats{Name: port.Name, Type: port.Type, Running: port.Running}}
			if port.Type == "ether" {
				row.HasEthernet = true
				row.Ethernet = routeros.EthernetStats{Name: port.Name, Switch: port.Switch, RxFlowControl: port.RxFlowControl,
					TxFlowControl: port.TxFlowControl, LoopProtect: port.LoopProtect}
			}
			d.Rows = append(d.Rows, row)
		}
		return d, true
	}
	a.rates = func(id string) map[string]int64 {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := map[string]int64{}
		for _, port := range h.devices[id].Ports {
			if port.RateKnown {
				out[port.Name] = port.Bps
			}
		}
		return out
	}
	a.capsOf = func(id string) (Caps, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		p, ok := h.devices[id]
		caps := Caps{Wifi: CapAbsent, CAPsMAN: CapAbsent}
		if p.WifiPackage {
			caps.Wifi = CapPresent
		}
		return caps, ok
	}
	a.self = func(*queries.Device, time.Time) []netip.Addr { return h.self }
	h.a = a
	return h
}

// add creates a managed device that answers like p, with what the scheduled
// reads would know about it: its bridge port table, its links, and — for an
// access point — the default route that makes it one. Its CPU is calm.
func (h *auditHarness) add(id string, p hygienetest.Personality) {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	mustCreateDevice(h.t, h.db, &queries.Device{ID: id, Identity: id, Address: "192.0.2." + string(rune('0'+n+1)), Username: p.NMSUser})
	if err := queries.UpdateDeviceInfo(h.db, id, "MikroTik", p.Board, p.Version, "", p.Architecture); err != nil {
		h.t.Fatal(err)
	}
	if err := queries.SetDeviceClass(h.db, id, p.Class, devclass.ResourceFacts{}, h.clock.now()); err != nil {
		h.t.Fatal(err)
	}
	h.mu.Lock()
	h.devices[id] = p
	h.cpu[id] = CPUReading{CPU: 12, At: h.clock.now()}
	h.mu.Unlock()
	h.syncPorts(id)
	if p.Role == devclass.RoleAPCPE {
		// Its default route points at another managed device (the core).
		if err := queries.ReplaceDeviceUplinks(h.db, id, []queries.DeviceUplink{{ID: id + ":gw", DeviceID: id, Kind: "default-route",
			Interface: "bridge", GatewayIP: "192.0.2.1"}}); err != nil {
			h.t.Fatal(err)
		}
	}
}

// syncPorts stores what the personality's ports mean for the stored tables.
func (h *auditHarness) syncPorts(id string) {
	h.t.Helper()
	h.mu.Lock()
	p := h.devices[id]
	h.mu.Unlock()
	if _, err := h.db.Exec(`DELETE FROM bridge_port_status WHERE device_id = ?`, id); err != nil {
		h.t.Fatal(err)
	}
	for _, port := range p.Ports {
		if port.Bridge != "" {
			bp := &queries.BridgePortStatus{ID: id + ":" + port.Bridge + ":" + port.Name, DeviceID: id, BridgeName: port.Bridge,
				PortInterface: port.Name, Horizon: port.Horizon, Disabled: ptr(port.BridgeDisabled)}
			if port.HWKnown {
				bp.HW = ptr(port.HW)
			}
			if port.HWOffloadKnown {
				bp.HWOffload = ptr(port.HWOffload)
			}
			if err := queries.UpsertBridgePortStatus(h.db, bp); err != nil {
				h.t.Fatal(err)
			}
		}
		if port.Trunk {
			if err := queries.UpsertLink(h.db, &queries.Link{ID: id + ":" + port.Name, DeviceAID: id, InterfaceA: port.Name,
				DeviceBID: "core", InterfaceB: "ether-" + id + "-" + port.Name, LinkType: "ethernet", DiscoveredBy: "mndp", Status: "up"}); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

// change replaces a device's personality (its configuration changed).
func (h *auditHarness) change(id string, p hygienetest.Personality) {
	h.mu.Lock()
	h.devices[id] = p
	h.mu.Unlock()
	h.syncPorts(id)
}

func (h *auditHarness) setCPU(id string, cpu int) {
	h.mu.Lock()
	h.cpu[id] = CPUReading{CPU: cpu, At: h.clock.now()}
	h.mu.Unlock()
}

// run moves the clock forward by d, ticking as the loop would: every
// pollerTick, and sooner when a pass asks for it. Every device's CPU sample
// stays as fresh as the sampler keeps it.
func (h *auditHarness) run(d time.Duration) { h.advance(d, pollerTick) }

// idle moves the clock forward by d through a stretch in which no pass is
// expected to make progress — the hours between two audits — looking in on
// the loop every five minutes instead of every five seconds. What becomes
// due meanwhile starts at one of those looks; follow it with run.
func (h *auditHarness) idle(d time.Duration) { h.advance(d, 5*time.Minute) }

func (h *auditHarness) advance(d, step time.Duration) {
	h.t.Helper()
	end := h.clock.now().Add(d)
	for {
		now := h.clock.now()
		h.mu.Lock()
		for id, r := range h.cpu {
			if !r.At.IsZero() {
				h.cpu[id] = CPUReading{CPU: r.CPU, At: now}
			}
		}
		h.mu.Unlock()
		wake := h.a.tick(now)
		next := now.Add(step)
		if step <= pollerTick {
			next = minTime(maxTime(wake, now.Add(200*time.Millisecond)), next)
		}
		if next.After(end) {
			h.clock.set(end)
			return
		}
		h.clock.set(next)
	}
}

func (h *auditHarness) commands(id string) []sentCommand {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sentCommand(nil), h.sent[id]...)
}

func (h *auditHarness) count(id string) int { return len(h.commands(id)) }

func (h *auditHarness) findings(id string) map[string]queries.DeviceFinding {
	h.t.Helper()
	rows, err := queries.ListDeviceFindings(h.db, id, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]queries.DeviceFinding{}
	for _, r := range rows {
		out[r.Kind] = r
	}
	return out
}

func (h *auditHarness) lastRun(id, kind string) *queries.AuditRun {
	h.t.Helper()
	r, err := queries.LatestAuditRun(h.db, id, kind)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *auditHarness) nextAudit(id string) (time.Time, bool) {
	at, ok := h.reg.NextRuns(id)["audit"]
	return at, ok
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// minGap is the shortest time between two consecutive commands.
func minGap(cmds []sentCommand) time.Duration {
	gap := time.Duration(1<<62 - 1)
	for i := 1; i < len(cmds); i++ {
		gap = min(gap, cmds[i].at.Sub(cmds[i-1].at))
	}
	return gap
}

var graphingRule = map[string]string{".id": "*1", "interface": "all", "allow-address": "0.0.0.0/0", "store-on-disk": "true", "disabled": "false"}

// ---- the scheduled audit ---------------------------------------------------------

// A weak device gets one audit command every 20 seconds at most, a capable
// one every 2; every command carries a proplist (or asks for a count); the
// next audit is six hours after the end of this one, and is published in
// the device's schedule.
func TestAuditIsPacedByClassAndRunsEverySixHours(t *testing.T) {
	h := newAuditHarness(t)
	h.add("sw", hygienetest.Load(t, "switch48_clean")) // class C1
	h.add("ap", hygienetest.Load(t, "ap_clean"))       // class C3

	// Nothing in the first two minutes after a start: the pool and the CPU
	// sampler come first.
	h.run(auditStartupDelay - time.Second)
	if h.count("sw")+h.count("ap") != 0 {
		t.Fatalf("commands before the start-up delay: sw %d, ap %d", h.count("sw"), h.count("ap"))
	}
	if next, ok := h.nextAudit("sw"); !ok || !next.Equal(at(12, 2, 0)) {
		t.Fatalf("published next audit = %v, %v; want 12:02:00", next, ok)
	}

	h.run(10 * time.Minute)
	sw, ap := h.commands("sw"), h.commands("ap")
	if len(sw) != 19 || len(ap) != 12 {
		t.Fatalf("audit commands: switch %d, access point %d; want 19 and 12", len(sw), len(ap))
	}
	if gap := minGap(sw); gap < 20*time.Second {
		t.Errorf("the C1 switch got two audit commands %v apart, want at least 20 s", gap)
	}
	if gap := minGap(ap); gap < 2*time.Second {
		t.Errorf("the C3 access point got two audit commands %v apart, want at least 2 s", gap)
	}
	if span := sw[len(sw)-1].at.Sub(sw[0].at); span != 18*20*time.Second {
		t.Errorf("the switch's audit took %v, want 19 reads 20 s apart (6 min)", span)
	}
	for _, c := range append(sw, ap...) {
		if !strings.Contains(c.command, "=.proplist=") && !strings.Contains(c.command, "=count-only=") {
			t.Errorf("%q carries neither a proplist nor count-only", c.command)
		}
	}

	// Each pass is on record, complete.
	for id, reads := range map[string]int{"sw": 19, "ap": 12} {
		r := h.lastRun(id, queries.AuditKindAudit)
		if r == nil || r.Status != queries.AuditComplete || r.Reads != reads || r.FailedReads != 0 || r.Reason != PassScheduled || r.Hash == "" {
			t.Errorf("%s: run = %+v", id, r)
		}
	}

	// Six hours after its end — not before — the next one.
	end := sw[len(sw)-1].at
	if next, ok := h.nextAudit("sw"); !ok || !next.Equal(end.Add(6*time.Hour)) {
		t.Fatalf("next audit = %v, %v; want %v", next, ok, end.Add(6*time.Hour))
	}
	h.clock.set(end.Add(6*time.Hour - 10*time.Second))
	h.run(5 * time.Second)
	if h.count("sw") != 19 {
		t.Fatalf("an audit started before the six hours were over (%d commands)", h.count("sw"))
	}
	h.run(10 * time.Minute)
	if h.count("sw") != 38 {
		t.Fatalf("after six hours: %d commands, want a second audit (38)", h.count("sw"))
	}
}

// A scheduled audit is not sent to a device whose CPU is at or above 80 %,
// that has no fresh CPU sample, is paused, offline, or has no pooled session
// — and it is tried again, not dropped.
func TestAuditStaysAwayFromABusyPausedOrUnreachableDevice(t *testing.T) {
	h := newAuditHarness(t)
	clean := hygienetest.Load(t, "switch48_clean")
	for _, id := range []string{"busy", "nosample", "paused", "offline", "nosession", "calm"} {
		h.add(id, clean)
	}
	h.setCPU("busy", 80)
	h.mu.Lock()
	delete(h.cpu, "nosample")
	h.down["nosession"] = true
	h.mu.Unlock()
	h.cs.paused["paused"] = true
	if _, err := h.db.Exec(`UPDATE devices SET status = 'offline' WHERE id = 'offline'`); err != nil {
		t.Fatal(err)
	}

	h.run(30 * time.Minute)
	for _, id := range []string{"busy", "nosample", "paused", "offline", "nosession"} {
		if n := h.count(id); n != 0 {
			t.Errorf("%s was sent %d audit commands", id, n)
		}
		if r := h.lastRun(id, queries.AuditKindAudit); r != nil {
			t.Errorf("%s: a pass was recorded: %+v", id, r)
		}
	}
	if h.count("calm") != 19 {
		t.Fatalf("the calm device was sent %d commands, want its audit (19)", h.count("calm"))
	}
	// A paused or offline device has no audit in its schedule; a waiting one
	// shows when it is looked at again.
	for _, id := range []string{"paused", "offline"} {
		if next, ok := h.nextAudit(id); ok {
			t.Errorf("%s: next audit published at %v", id, next)
		}
	}
	for _, id := range []string{"busy", "nosample", "nosession"} {
		if next, ok := h.nextAudit(id); !ok || !next.After(h.clock.now().Add(-time.Second)) {
			t.Errorf("%s: next audit = %v, %v; want a retry ahead", id, next, ok)
		}
	}

	// Every one of them is audited once its reason is gone.
	h.setCPU("busy", 35)
	h.setCPU("nosample", 20)
	h.mu.Lock()
	h.down["nosession"] = false
	h.mu.Unlock()
	h.cs.paused["paused"] = false
	if _, err := h.db.Exec(`UPDATE devices SET status = 'online' WHERE id = 'offline'`); err != nil {
		t.Fatal(err)
	}
	h.run(auditBusyRetry + 10*time.Minute)
	for _, id := range []string{"busy", "nosample", "paused", "offline", "nosession"} {
		if n := h.count(id); n != 19 {
			t.Errorf("%s: %d commands after its reason was gone, want its audit (19)", id, n)
		}
	}
}

// An audit that is under way steps aside when the CPU goes up, keeps what it
// has read, and goes on when the CPU is calm again.
func TestAuditStepsAsideWhenTheCPUGoesUpMidPass(t *testing.T) {
	h := newAuditHarness(t)
	h.add("sw", hygienetest.Load(t, "switch48_clean"))
	h.run(auditStartupDelay + 65*time.Second) // 12:02:00, :20, :40, 12:03:00
	before := h.count("sw")
	if before < 3 || before >= 19 {
		t.Fatalf("mid-pass: %d commands", before)
	}
	h.setCPU("sw", 93)
	h.run(10 * time.Minute)
	if h.count("sw") != before {
		t.Fatalf("%d commands were sent while the CPU was at 93 %%", h.count("sw")-before)
	}
	h.setCPU("sw", 20)
	h.run(10 * time.Minute)
	if h.count("sw") != 19 {
		t.Fatalf("after the CPU calmed down: %d commands, want the rest of the audit (19 in all)", h.count("sw"))
	}
	if r := h.lastRun("sw", queries.AuditKindAudit); r.Status != queries.AuditComplete || r.Reads != 19 {
		t.Fatalf("run = %+v", r)
	}
}

// An audit that stepped aside must not keep the diagnose pass waiting: when
// cpu_high opens, the audit ends with what it has read, and the diagnose
// pass — the one that reads a busy device — starts.
func TestWaitingAuditMakesWayForTheDiagnosePass(t *testing.T) {
	h := newAuditHarness(t)
	h.add("sw", hygienetest.Load(t, "switch48_clean").With("/tool/graphing/interface/print", graphingRule))
	h.run(auditStartupDelay + 65*time.Second)
	audited := h.count("sw")
	if audited < 3 || audited >= 19 {
		t.Fatalf("mid-pass: %d commands", audited)
	}
	h.setCPU("sw", 100)
	h.run(5 * time.Minute)
	if h.count("sw") != audited {
		t.Fatalf("the audit went on at 100 %% CPU")
	}
	opened := h.clock.now()
	openAlert(t, h.db, alerts.RuleCPUHigh, "sw", alerts.SeverityWarning, true, opened)
	h.run(8 * time.Minute)

	audit := h.lastRun("sw", queries.AuditKindAudit)
	if audit.Status != queries.AuditPartial || audit.Reads != audited || !strings.Contains(audit.Detail, "diagnose") {
		t.Fatalf("the waiting audit = %+v, want it ended as partial for the diagnose pass", audit)
	}
	diag := h.lastRun("sw", queries.AuditKindDiagnose)
	if diag == nil || diag.Status != queries.AuditComplete || diag.Reads != 11 || diag.StartedAt.Sub(opened) > alertScanEvery+pollerTick {
		t.Fatalf("the diagnose pass = %+v, want it started within a scan of the alert", diag)
	}
	if h.count("sw") != audited+11 {
		t.Fatalf("%d commands in all, want the audit's %d and the diagnose pass's 11", h.count("sw"), audited)
	}
	// What the audit had read before it stopped still counts.
	if g, ok := h.findings("sw")[hygiene.KindGraphing]; !ok || g.Count < 1 {
		t.Fatalf("findings = %v", h.findings("sw"))
	}
}

// The snapshot is stored when the configuration changes, and only then.
func TestAuditStoresTheSnapshotOnlyWhenItChanges(t *testing.T) {
	h := newAuditHarness(t)
	clean := hygienetest.Load(t, "switch48_clean")
	h.add("sw", clean)
	snapshots := func() int { return countRows(t, h.db, `SELECT COUNT(*) FROM device_audit WHERE device_id = 'sw'`) }

	h.run(15 * time.Minute)
	if snapshots() != 1 {
		t.Fatalf("after the first audit: %d snapshots", snapshots())
	}
	first, _ := queries.LatestDeviceAudit(h.db, "sw")
	if first.ROSVersion != clean.Version || len(first.Hash) != 64 || !strings.Contains(first.Snapshot, `"use-ip-firewall":"false"`) {
		t.Fatalf("snapshot = %+v", first)
	}

	// Six hours later nothing was configured — only counters moved.
	h.change("sw", clean.With("/interface/bridge/settings/print", map[string]string{"use-ip-firewall": "false",
		"use-ip-firewall-for-vlan": "false", "allow-fast-path": "true", "bridge-fast-path-active": "true",
		"bridge-fast-path-packets": "41230999", "bridge-fast-forward-packets": "880123"}))
	h.idle(5*time.Hour + 50*time.Minute)
	h.run(20 * time.Minute)
	if snapshots() != 1 {
		t.Fatalf("an unchanged configuration was stored again: %d snapshots", snapshots())
	}
	if n := countRows(t, h.db, `SELECT COUNT(*) FROM device_audit_runs WHERE device_id = 'sw' AND status = 'complete'`); n != 2 {
		t.Fatalf("%d complete passes on record, want 2", n)
	}

	// Somebody adds a graphing rule.
	h.change("sw", clean.With("/tool/graphing/interface/print", graphingRule))
	h.idle(5*time.Hour + 50*time.Minute)
	h.run(20 * time.Minute)
	if snapshots() != 2 {
		t.Fatalf("after the configuration changed: %d snapshots, want 2", snapshots())
	}
	second, _ := queries.LatestDeviceAudit(h.db, "sw")
	if second.Hash == first.Hash || !strings.Contains(second.Snapshot, `"interface":"all"`) || !strings.Contains(second.Findings, "graphing_enabled") {
		t.Fatalf("second snapshot = %+v", second)
	}
}

// A finding appears with what an operator needs to act on it, is counted
// when it is seen again, and is removed by the audit that finds the setting
// gone — while findings the audit does not own stay.
func TestAuditFindingsAppearAndAreRemovedWhenFixed(t *testing.T) {
	h := newAuditHarness(t)
	clean := hygienetest.Load(t, "switch48_clean")
	h.add("sw", clean.With("/tool/graphing/interface/print", graphingRule).
		With("/tool/sniffer/print", map[string]string{"running": "true"}))
	// The login watch's finding, and one the flow guard wrote.
	for _, f := range []*queries.DeviceFinding{
		{DeviceID: "sw", Kind: FindingParallelAPIPoller, Severity: "warn", Detail: "user x from 198.51.100.9 via api"},
		{DeviceID: "sw", Kind: "flow_export_on_switch", Severity: "warn", Detail: "flow datagrams from 192.0.2.1"},
	} {
		if err := queries.UpsertDeviceFinding(h.db, f); err != nil {
			t.Fatal(err)
		}
	}

	h.run(15 * time.Minute)
	got := h.findings("sw")
	g, ok := got[hygiene.KindGraphing]
	if !ok || g.Severity != "critical" || g.Source != queries.FindingSourceAudit || g.Count != 1 ||
		g.Title != "Graphing is enabled on a weak device" || g.Explanation == "" || len(g.Fix) == 0 ||
		!reflect.DeepEqual(g.Fix[0].Commands, []string{`/tool graphing interface remove [find interface="all"]`}) {
		t.Fatalf("graphing finding = %+v", g)
	}
	if s := got[hygiene.KindSnifferRunning]; s.Severity != "warn" {
		t.Fatalf("sniffer finding = %+v", s)
	}

	// The sniffer is stopped; graphing stays.
	h.change("sw", clean.With("/tool/graphing/interface/print", graphingRule))
	h.idle(5*time.Hour + 50*time.Minute)
	h.run(20 * time.Minute)
	got = h.findings("sw")
	if _, still := got[hygiene.KindSnifferRunning]; still {
		t.Fatal("the sniffer finding outlived the sniffer")
	}
	if g := got[hygiene.KindGraphing]; g.Count != 2 {
		t.Fatalf("graphing finding seen twice: %+v", g)
	}

	// Graphing is removed too.
	h.change("sw", clean)
	h.idle(5*time.Hour + 50*time.Minute)
	h.run(20 * time.Minute)
	got = h.findings("sw")
	if _, still := got[hygiene.KindGraphing]; still {
		t.Fatal("the graphing finding outlived the rule")
	}
	for _, kind := range []string{FindingParallelAPIPoller, "flow_export_on_switch"} {
		if _, ok := got[kind]; !ok {
			t.Errorf("the audit removed %s, which is not its finding", kind)
		}
	}
}

// A menu the device does not have is not an error; a menu the NMS account
// may not read is reported as exactly that — and the findings behind it are
// neither raised nor removed.
func TestAuditAbsentMenuIsFineDeniedMenuIsAFinding(t *testing.T) {
	h := newAuditHarness(t)
	clean := hygienetest.Load(t, "switch48_clean")
	// RouterOS 6: no device-mode menu. First pass: SNMP is on and open.
	ros6 := clean
	ros6.Replies = map[string]hygienetest.Reply{}
	for k, v := range clean.Replies {
		if k != "/system/device-mode/print" {
			ros6.Replies[k] = v
		}
	}
	open := ros6.With("/snmp/print", map[string]string{"enabled": "true"})
	h.add("sw", open)
	h.run(15 * time.Minute)
	r := h.lastRun("sw", queries.AuditKindAudit)
	if r.Status != queries.AuditComplete || r.FailedReads != 0 {
		t.Fatalf("an absent menu made the pass %s: %+v", r.Status, r)
	}
	got := h.findings("sw")
	if _, ok := got[hygiene.KindSNMPOpen]; !ok {
		t.Fatalf("findings = %v, want the open SNMP community", got)
	}
	if _, ok := got[hygiene.KindUnreadable]; ok {
		t.Fatal("an absent menu was reported as unreadable")
	}

	// Then the account loses the right to read the communities.
	h.change("sw", open.WithReply("/snmp/community/print", hygienetest.Reply{Trap: "not enough permissions (9)"}))
	h.idle(5*time.Hour + 50*time.Minute)
	h.run(20 * time.Minute)
	r = h.lastRun("sw", queries.AuditKindAudit)
	if r.Status != queries.AuditComplete {
		t.Fatalf("a refused read made the pass %s", r.Status)
	}
	got = h.findings("sw")
	u, ok := got[hygiene.KindUnreadable]
	if !ok || u.Severity != "info" || u.Detail != "/snmp/community" || !strings.Contains(u.Explanation, "not enough permissions") {
		t.Fatalf("unreadable finding = %+v", u)
	}
	// What could not be read is not "clean": the finding from before stands.
	if _, ok := got[hygiene.KindSNMPOpen]; !ok {
		t.Fatal("the open-SNMP finding was removed although the audit could not read the communities")
	}
	// And the refusal is part of the snapshot: the configuration as far as
	// the NMS can see it changed.
	snap, _ := queries.LatestDeviceAudit(h.db, "sw")
	if !strings.Contains(snap.Snapshot, `"snmp_communities":{"state":"denied"}`) {
		t.Fatalf("snapshot = %s", snap.Snapshot)
	}
}

// A read that fails makes the pass partial: what was read is judged, no
// snapshot is stored, and the audit is run again an hour later.
func TestAuditWithAFailedReadIsPartial(t *testing.T) {
	h := newAuditHarness(t)
	h.add("sw", hygienetest.Load(t, "switch48_clean").With("/tool/graphing/interface/print", graphingRule))
	h.mu.Lock()
	h.fail["sw"] = map[string]error{"/system/scheduler/print": errors.New("read tcp 192.0.2.1:8728: i/o timeout")}
	h.mu.Unlock()

	h.run(15 * time.Minute)
	r := h.lastRun("sw", queries.AuditKindAudit)
	if r.Status != queries.AuditPartial || r.FailedReads != 1 || r.Reads != 19 || !strings.Contains(r.Detail, "scheduler") {
		t.Fatalf("run = %+v", r)
	}
	if n := countRows(t, h.db, `SELECT COUNT(*) FROM device_audit WHERE device_id = 'sw'`); n != 0 {
		t.Fatalf("%d snapshots stored from an incomplete audit", n)
	}
	if _, ok := h.findings("sw")[hygiene.KindGraphing]; !ok {
		t.Fatal("what the pass did read was not judged")
	}
	end := h.commands("sw")[18].at
	if next, ok := h.nextAudit("sw"); !ok || !next.Equal(end.Add(time.Hour)) {
		t.Fatalf("next audit = %v, %v; want an hour after the partial one (%v)", next, ok, end.Add(time.Hour))
	}
	h.mu.Lock()
	h.fail["sw"] = nil
	h.mu.Unlock()
	h.run(time.Hour + 10*time.Minute)
	if r := h.lastRun("sw", queries.AuditKindAudit); r.Status != queries.AuditComplete {
		t.Fatalf("the retry: %+v", r)
	}
}

// A restart neither audits the whole fleet at once nor each device again:
// the schedule continues from the audits the database remembers, and a pass
// the restart cut off is closed.
func TestAuditScheduleSurvivesARestart(t *testing.T) {
	h := newAuditHarness(t)
	clean := hygienetest.Load(t, "switch48_clean")
	for _, id := range []string{"recent", "overdue", "never"} {
		h.add(id, clean)
	}
	now := h.clock.now()
	for id, ago := range map[string]time.Duration{"recent": 2 * time.Hour, "overdue": 9 * time.Hour} {
		runID, _ := queries.InsertAuditRun(h.db, &queries.AuditRun{DeviceID: id, Kind: queries.AuditKindAudit, Reason: PassScheduled, StartedAt: now.Add(-ago - 6*time.Minute)})
		_ = queries.FinishAuditRun(h.db, &queries.AuditRun{ID: runID, FinishedAt: now.Add(-ago), Status: queries.AuditComplete, Reads: 19})
	}
	stuck, _ := queries.InsertAuditRun(h.db, &queries.AuditRun{DeviceID: "never", Kind: queries.AuditKindDiagnose, Reason: alerts.RuleCPUHigh, StartedAt: now.Add(-3 * time.Hour)})
	// Three devices, three different start-up offsets.
	h.a.spread = func(id string) time.Duration {
		return map[string]time.Duration{"recent": 0, "overdue": 4 * time.Minute, "never": 8 * time.Minute}[id]
	}

	h.run(time.Second)
	if next, _ := h.nextAudit("recent"); !next.Equal(now.Add(4 * time.Hour)) {
		t.Errorf("audited 2 h ago: next audit %v, want in 4 h (%v)", next, now.Add(4*time.Hour))
	}
	if next, _ := h.nextAudit("overdue"); !next.Equal(now.Add(auditStartupDelay + 4*time.Minute)) {
		t.Errorf("overdue: next audit %v, want after the start-up delay and its offset", next)
	}
	if next, _ := h.nextAudit("never"); !next.Equal(now.Add(auditStartupDelay + 8*time.Minute)) {
		t.Errorf("never audited: next audit %v, want after the start-up delay and its offset", next)
	}
	if r, _ := queries.LatestAuditRun(h.db, "never", queries.AuditKindDiagnose); r.ID != stuck || r.Status != queries.AuditAborted || r.FinishedAt.IsZero() {
		t.Errorf("the pass the restart cut off = %+v", r)
	}

	h.run(30 * time.Minute)
	if h.count("recent") != 0 {
		t.Errorf("the device audited two hours ago was audited again after the restart (%d commands)", h.count("recent"))
	}
	if h.count("overdue") != 19 || h.count("never") != 19 {
		t.Errorf("overdue %d, never %d commands; want an audit each", h.count("overdue"), h.count("never"))
	}
	if first := h.commands("never")[0].at; first.Before(now.Add(auditStartupDelay + 8*time.Minute)) {
		t.Errorf("the never-audited device was read at %v, before its offset", first)
	}
}

// ---- the diagnose pass ------------------------------------------------------------

func openAlert(t *testing.T, db *sql.DB, rule, device, severity string, confirmed bool, at time.Time) int64 {
	t.Helper()
	id, err := queries.InsertAlert(db, &queries.Alert{Rule: rule, Key: device, DeviceID: device, Severity: severity,
		FirstAt: at, LastAt: at, Confirmed: confirmed, Payload: `{"summary":"x"}`})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// When cpu_high opens for a device, the diagnose pass runs — although, and
// because, its CPU is high: the shorter list of reads, one every 30 s on a
// weak device, and the graphing rule that pegs it is found within minutes.
func TestDiagnoseRunsWhenCPUHighOpens(t *testing.T) {
	h := newAuditHarness(t)
	h.add("sw", hygienetest.Load(t, "switch48_clean").With("/tool/graphing/interface/print", graphingRule))
	h.setCPU("sw", 100)

	h.run(10 * time.Minute)
	if h.count("sw") != 0 {
		t.Fatalf("a pegged device was sent %d commands without an alert", h.count("sw"))
	}
	opened := h.clock.now()
	alertID := openAlert(t, h.db, alerts.RuleCPUHigh, "sw", alerts.SeverityWarning, true, opened)

	h.run(8 * time.Minute)
	cmds := h.commands("sw")
	if len(cmds) != 11 {
		t.Fatalf("the diagnose pass sent %d commands, want 11", len(cmds))
	}
	if gap := minGap(cmds); gap < 30*time.Second {
		t.Errorf("two diagnose commands %v apart on a C1 device, want at least 30 s", gap)
	}
	if first := cmds[0]; first.at.Sub(opened) > alertScanEvery+pollerTick || !strings.HasPrefix(first.command, "/tool/graphing/interface/print") {
		t.Errorf("first command %q at +%v: the pass starts with graphing, within a scan of the alert", first.command, first.at.Sub(opened))
	}
	r := h.lastRun("sw", queries.AuditKindDiagnose)
	if r.Status != queries.AuditComplete || r.Reason != alerts.RuleCPUHigh || r.AlertID != alertID || r.Reads != 11 ||
		!strings.Contains(r.Findings, `"kind":"graphing_enabled"`) || !strings.Contains(r.Findings, `"severity":"critical"`) {
		t.Fatalf("run = %+v", r)
	}
	if found := r.FinishedAt.Sub(opened); found > 15*time.Minute {
		t.Errorf("the cause was found %v after the alert opened, want within 15 min", found)
	}
	if g, ok := h.findings("sw")[hygiene.KindGraphing]; !ok || g.Severity != "critical" {
		t.Fatalf("no critical graphing finding: %+v", h.findings("sw"))
	}
	// The scheduled audit still stays away.
	if r := h.lastRun("sw", queries.AuditKindAudit); r != nil {
		t.Fatalf("an audit ran on the pegged device: %+v", r)
	}

	// One pass per episode: the alert stays open for hours, nothing more is
	// sent.
	h.idle(5 * time.Hour)
	if h.count("sw") != 11 {
		t.Fatalf("%d more commands during the same episode", h.count("sw")-11)
	}
}

// A new episode within the hour of the last pass does not get one of its own
// until the hour is over; a device is never diagnosed more than once an hour.
func TestDiagnoseAtMostOncePerHour(t *testing.T) {
	h := newAuditHarness(t)
	h.add("r", hygienetest.Load(t, "ap_clean")) // class C3: a pass takes seconds
	h.setCPU("r", 95)
	first := openAlert(t, h.db, alerts.RuleCPUHigh, "r", alerts.SeverityWarning, true, h.clock.now())
	h.run(2 * time.Minute)
	if h.count("r") != 8 {
		t.Fatalf("first pass: %d commands, want 8", h.count("r"))
	}
	firstStart := h.commands("r")[0].at

	// The episode ends and another begins twenty minutes later.
	if err := queries.ResolveAlert(h.db, first, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	h.run(20 * time.Minute)
	second := openAlert(t, h.db, alerts.RuleCPUHigh, "r", alerts.SeverityCritical, true, h.clock.now())
	h.run(30 * time.Minute)
	if h.count("r") != 8 {
		t.Fatalf("a second pass ran %v after the first", h.commands("r")[8].at.Sub(firstStart))
	}
	// Still open when the hour is over: now it gets its pass.
	h.run(15 * time.Minute)
	cmds := h.commands("r")
	if len(cmds) != 16 {
		t.Fatalf("after the hour: %d commands, want a second pass (16)", len(cmds))
	}
	if gap := cmds[8].at.Sub(firstStart); gap < PassBudget {
		t.Fatalf("the second pass started %v after the first, want at least an hour", gap)
	}
	if r := h.lastRun("r", queries.AuditKindDiagnose); r.AlertID != second {
		t.Fatalf("run = %+v, want it for the second alert", r)
	}
}

// Which alerts start a pass: cpu_high, flow_export_cpu_high, and
// flow_export_on_switch once the device confirmed it — a source address
// alone starts nothing, and neither does any other rule.
func TestDiagnoseTriggers(t *testing.T) {
	h := newAuditHarness(t)
	clean := hygienetest.Load(t, "ap_clean")
	now := h.clock.now()
	for id, alert := range map[string]struct {
		rule, severity string
		confirmed      bool
	}{
		"watch":       {alerts.RuleFlowExportCPUHigh, alerts.SeverityWarning, true},
		"confirmed":   {alerts.RuleFlowExportOnSwitch, alerts.SeverityCritical, true},
		"unconfirmed": {alerts.RuleFlowExportOnSwitch, alerts.SeverityWarning, false},
		"memory":      {alerts.RuleMemLow, alerts.SeverityWarning, true},
		"unreachable": {alerts.RuleDeviceUnreachable, alerts.SeverityCritical, true},
		"paused":      {alerts.RuleCPUHigh, alerts.SeverityCritical, true},
		"nosession":   {alerts.RuleCPUHigh, alerts.SeverityCritical, true},
	} {
		h.add(id, clean)
		h.setCPU(id, 90) // no scheduled audit gets in the way
		openAlert(t, h.db, alert.rule, id, alert.severity, alert.confirmed, now)
	}
	h.cs.paused["paused"] = true
	h.mu.Lock()
	h.down["nosession"] = true
	h.mu.Unlock()

	h.run(5 * time.Minute)
	for id, want := range map[string]int{"watch": 8, "confirmed": 8, "unconfirmed": 0, "memory": 0, "unreachable": 0, "paused": 0, "nosession": 0} {
		if got := h.count(id); got != want {
			t.Errorf("%s: %d commands, want %d", id, got, want)
		}
	}
	// The device that had no session gets its pass when it has one; the
	// paused one when it is resumed.
	h.mu.Lock()
	h.down["nosession"] = false
	h.mu.Unlock()
	h.cs.paused["paused"] = false
	h.run(2 * time.Minute)
	for _, id := range []string{"nosession", "paused"} {
		if got := h.count(id); got != 8 {
			t.Errorf("%s: %d commands once it could be read, want 8", id, got)
		}
	}
}

// ---- admin runs ---------------------------------------------------------------------

func TestAdminPasses(t *testing.T) {
	h := newAuditHarness(t)
	clean := hygienetest.Load(t, "ap_clean")
	h.add("r", clean)
	h.add("paused", clean)
	h.add("down", clean)
	h.cs.paused["paused"] = true
	until := h.clock.now().Add(time.Hour)
	if err := queries.PauseDevicePolling(h.db, "paused", until); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.down["down"] = true
	h.mu.Unlock()
	h.setCPU("r", 97) // an admin's pass runs whatever the CPU says

	run, err := h.a.Request("r", queries.AuditKindAudit, "alice")
	if err != nil || run.ID == 0 || run.Status != queries.AuditRunning || run.RequestedBy != "alice" {
		t.Fatalf("audit now: %+v, %v", run, err)
	}
	if got := h.a.Running("r"); got != queries.AuditKindAudit {
		t.Fatalf("Running = %q", got)
	}
	// While it runs, nothing else starts on the device.
	if _, err := h.a.Request("r", queries.AuditKindDiagnose, "alice"); !errors.Is(err, ErrPassRunning) {
		t.Fatalf("a second pass while one runs: %v", err)
	}
	h.run(2 * time.Minute)
	if h.count("r") != 12 || h.a.Running("r") != "" {
		t.Fatalf("the admin's audit: %d commands, running %q", h.count("r"), h.a.Running("r"))
	}
	if r := h.lastRun("r", queries.AuditKindAudit); r.Status != queries.AuditComplete || r.Reason != PassAdmin || r.RequestedBy != "alice" {
		t.Fatalf("run = %+v", r)
	}

	// Once an hour per device and kind.
	var budget *PassBudgetError
	if _, err := h.a.Request("r", queries.AuditKindAudit, "bob"); !errors.As(err, &budget) || !budget.Next.Equal(run.StartedAt.Add(time.Hour)) {
		t.Fatalf("a second audit within the hour: %v", err)
	}
	// The diagnose pass has its own hour.
	if _, err := h.a.Request("r", queries.AuditKindDiagnose, "bob"); err != nil {
		t.Fatalf("diagnose now: %v", err)
	}
	h.run(time.Minute)
	if h.count("r") != 12+8 {
		t.Fatalf("after the diagnose pass: %d commands", h.count("r"))
	}
	if _, err := h.a.Request("r", queries.AuditKindDiagnose, "bob"); !errors.As(err, &budget) {
		t.Fatalf("a second diagnose pass within the hour: %v", err)
	}
	// An alert that opens now does not get around the budget either.
	openAlert(t, h.db, alerts.RuleCPUHigh, "r", alerts.SeverityWarning, true, h.clock.now())
	h.run(10 * time.Minute)
	if h.count("r") != 20 {
		t.Fatalf("an alert started a second diagnose pass within the hour (%d commands)", h.count("r"))
	}
	h.run(time.Hour)
	if h.count("r") != 28 {
		t.Fatalf("after the hour the open alert gets its pass: %d commands, want 28", h.count("r"))
	}
	// A later request is fine again — and moves the scheduled audit: the
	// next one is six hours after it.
	h.setCPU("r", 20)
	if _, err := h.a.Request("r", queries.AuditKindAudit, "alice"); err != nil {
		t.Fatalf("an audit after the hour: %v", err)
	}
	h.run(2 * time.Minute)
	end := h.commands("r")[h.count("r")-1].at
	if next, ok := h.nextAudit("r"); !ok || !next.Equal(end.Add(6*time.Hour)) {
		t.Fatalf("next scheduled audit = %v, %v; want six hours after the admin's (%v)", next, ok, end.Add(6*time.Hour))
	}

	// Refusals that name their reason.
	var paused *PassPausedError
	if _, err := h.a.Request("paused", queries.AuditKindAudit, "alice"); !errors.As(err, &paused) || !paused.Until.Equal(until) {
		t.Fatalf("paused device: %v", err)
	}
	if _, err := h.a.Request("down", queries.AuditKindDiagnose, "alice"); !errors.Is(err, ErrPassNoSession) {
		t.Fatalf("device without a session: %v", err)
	}
	if _, err := h.a.Request("nope", queries.AuditKindAudit, "alice"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown device: %v", err)
	}
	h.a.pollingAllowed = func() error { return routeros.ErrPollersOff }
	if _, err := h.a.Request("r", queries.AuditKindDiagnose, "alice"); !errors.Is(err, routeros.ErrPollersOff) {
		t.Fatalf("polling off: %v", err)
	}
	if h.count("paused")+h.count("down") != 0 {
		t.Fatal("a refused pass sent commands")
	}
}

// A pass ends when the device is paused under it: nothing more is sent, and
// nothing it had read is judged.
func TestPassIsAbortedByAPause(t *testing.T) {
	h := newAuditHarness(t)
	h.add("sw", hygienetest.Load(t, "switch48_clean").With("/tool/graphing/interface/print", graphingRule))
	h.run(auditStartupDelay + 45*time.Second)
	before := h.count("sw")
	if before == 0 {
		t.Fatal("the audit did not start")
	}
	h.cs.paused["sw"] = true
	h.run(20 * time.Minute)
	if h.count("sw") != before {
		t.Fatalf("%d commands were sent to a paused device", h.count("sw")-before)
	}
	if r := h.lastRun("sw", queries.AuditKindAudit); r.Status != queries.AuditAborted || !strings.Contains(r.Detail, "paused") {
		t.Fatalf("run = %+v", r)
	}
	if len(h.findings("sw")) != 0 {
		t.Fatalf("an aborted pass stored findings: %v", h.findings("sw"))
	}
}

// ---- what the pass is told about the device ------------------------------------------

// The facts of a pass are the personality's: the interface snapshot with the
// switch chip, flow control and loop-protect, the port rates, the bridge
// port table with hw and horizon, and the links that make a port a trunk.
func TestAuditFactsComeFromWhatTheNMSAlreadyKnows(t *testing.T) {
	h := newAuditHarness(t)
	p := hygienetest.Load(t, "switch48_clean").
		WithPort("ether1", func(port *hygiene.Port) { port.Horizon = "2"; port.RxFlowControl = "on"; port.LoopProtect = "on" })
	h.add("sw", p)
	h.add("ap", hygienetest.Load(t, "ap_clean"))
	if err := queries.SetSetting(h.db, "flow_advertise_address", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	prev := int64(1234)
	rt := hygiene.Runtime{At: h.clock.now().Add(-6 * time.Hour), CPUForwarded: &prev}
	runID, _ := queries.InsertAuditRun(h.db, &queries.AuditRun{DeviceID: "sw", Kind: queries.AuditKindAudit, StartedAt: rt.At})
	_ = queries.FinishAuditRun(h.db, &queries.AuditRun{ID: runID, FinishedAt: rt.At, Status: queries.AuditComplete, Runtime: rt.JSON()})

	dev, _ := queries.GetDevice(h.db, "sw")
	got := h.a.facts(*dev, h.clock.now())
	want := p.Facts(h.clock.now())
	if !reflect.DeepEqual(got.Ports, want.Ports) {
		for i := range want.Ports {
			if i < len(got.Ports) && !reflect.DeepEqual(got.Ports[i], want.Ports[i]) {
				t.Errorf("port %s:\n got  %+v\n want %+v", want.Ports[i].Name, got.Ports[i], want.Ports[i])
			}
		}
		t.Fatalf("ports differ (%d vs %d)", len(got.Ports), len(want.Ports))
	}
	if got.Class != devclass.C1 || got.Role != devclass.RoleSwitch || got.Board != p.Board || got.Architecture != "mipsbe" ||
		got.Version != p.Version || got.NMSUser != "nms" || !got.InterfacesKnown || !got.EthernetKnown || !got.BridgePortsKnown ||
		!got.HasBridge || !got.HasBond || got.WifiPackage || got.Collector != "192.0.2.10" || got.CollectorPort != 2055 {
		t.Fatalf("facts = %+v", got)
	}
	if got.Prev == nil || *got.Prev.CPUForwarded != 1234 || !got.Prev.At.Equal(rt.At) {
		t.Fatalf("previous runtime = %+v", got.Prev)
	}
	if !reflect.DeepEqual(got.NMSAddresses, h.self) {
		t.Fatalf("NMS addresses = %v", got.NMSAddresses)
	}

	// The access point is one because its default route points at a router.
	apDev, _ := queries.GetDevice(h.db, "ap")
	if f := h.a.facts(*apDev, h.clock.now()); f.Role != devclass.RoleAPCPE || f.Class != devclass.C3 || !f.WifiPackage {
		t.Fatalf("access point facts: role %q class %s wifi %v (%s)", f.Role, f.Class, f.WifiPackage, f.RoleReason)
	}

	// A device the collectors have not read yet: nothing is known, and
	// nothing is guessed.
	h.a.ifaces = func(string) (DeviceIfaces, bool) { return DeviceIfaces{}, false }
	if f := h.a.facts(*dev, h.clock.now()); f.InterfacesKnown || f.EthernetKnown || f.BridgePortsKnown || len(f.Ports) != 0 {
		t.Fatalf("facts without a snapshot: %+v", f)
	}
}

// ---- G6: the CPU watch after a flow exporter is accepted --------------------------------

func TestFlowExportWatchStartsWhenAnExporterIsAccepted(t *testing.T) {
	h := newAuditHarness(t)
	h.add("r", hygienetest.Load(t, "ap_clean"))
	h.add("fresh", hygienetest.Load(t, "ap_clean"))
	now := h.clock.now()
	// A day of minutes at 10 % for one of them; the other was added today.
	var minutes []queries.DeviceMinute
	for m := now.Add(-23 * time.Hour).Truncate(time.Minute); m.Before(now); m = m.Add(time.Minute) {
		minutes = append(minutes, queries.DeviceMinute{DeviceID: "r", Bucket: m, PolledS: 60, CPUSum: 10, CPUSamples: 1, CPUMax: 10})
	}
	for i := 0; i < 5; i++ {
		minutes = append(minutes, queries.DeviceMinute{DeviceID: "fresh", Bucket: now.Add(-time.Duration(i+1) * time.Minute).Truncate(time.Minute),
			PolledS: 60, CPUSum: 50, CPUSamples: 1, CPUMax: 50})
	}
	if err := queries.UpsertDeviceMinutes(h.db, minutes); err != nil {
		t.Fatal(err)
	}
	devR, _ := queries.GetDevice(h.db, "r")
	devFresh, _ := queries.GetDevice(h.db, "fresh")

	h.a.NoteExporterAccepted(queries.FlowExporter{ID: 7, Address: devR.Address, DeviceID: "r"}, now)
	h.a.NoteExporterAccepted(queries.FlowExporter{ID: 8, Address: devFresh.Address}, now) // added by address
	h.a.NoteExporterAccepted(queries.FlowExporter{ID: 9, Address: "198.51.100.77"}, now)  // not a managed device
	if n := countRows(t, h.db, `SELECT COUNT(*) FROM flow_export_watches`); n != 0 {
		t.Fatalf("the hook wrote %d rows itself: it must only queue", n)
	}
	h.run(time.Second)

	watches, err := queries.ListFlowExportWatches(h.db)
	if err != nil || len(watches) != 2 {
		t.Fatalf("watches = %+v, %v", watches, err)
	}
	byDev := map[string]queries.FlowExportWatch{}
	for _, w := range watches {
		byDev[w.DeviceID] = w
	}
	if w := byDev["r"]; w.ExporterID != 7 || !w.AcceptedAt.Equal(now) || w.Baseline == nil || *w.Baseline != 10 || w.CollectorPort != 2055 {
		t.Errorf("watch of r = %+v", w)
	}
	// Five minutes of history are no baseline.
	if w := byDev["fresh"]; w.ExporterID != 8 || w.Baseline != nil {
		t.Errorf("watch of fresh = %+v", w)
	}
	// The watch itself costs the device nothing.
	if h.count("r")+h.count("fresh") != 0 {
		t.Fatal("starting a watch sent commands")
	}
}

// With polling off nothing audits: the manager does not run the loop, the
// flow collector accepts nothing, and a request is refused before any
// device is looked at.
func TestAuditorWithPollingOff(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "d1", Address: "192.0.2.1", Status: "online"})
	routeros.SetDialPolicy(&routeros.DialPolicy{Enabled: false})
	t.Cleanup(func() { routeros.SetDialPolicy(nil) })
	cfg := &config.Config{Pollers: config.PollersOff, RetentionInterval: time.Hour, HealthInterval: 30 * time.Second}
	m := NewManager(db, routeros.NewPool(), ws.NewHub(), cfg)
	if m.Audit() == nil {
		t.Fatal("the manager has no auditor")
	}
	m.Start()
	m.Stop()

	for _, kind := range []string{queries.AuditKindAudit, queries.AuditKindDiagnose} {
		if _, err := m.Audit().Request("d1", kind, "alice"); !errors.Is(err, routeros.ErrPollersOff) {
			t.Fatalf("%s with polling off: %v, want ErrPollersOff", kind, err)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM device_audit_runs`); n != 0 {
		t.Fatalf("%d passes on record with polling off", n)
	}
	if next, ok := Schedule().NextRuns("d1")["audit"]; ok {
		t.Fatalf("an audit is scheduled with polling off (%v)", next)
	}
}

func TestAuditRetention(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d", Identity: "d", Address: "192.0.2.1"})
	now := at(12, 0, 0)
	for _, ago := range []time.Duration{40 * 24 * time.Hour, time.Hour} {
		id, _ := queries.InsertAuditRun(db, &queries.AuditRun{DeviceID: "d", Kind: queries.AuditKindAudit, StartedAt: now.Add(-ago)})
		_ = queries.FinishAuditRun(db, &queries.AuditRun{ID: id, FinishedAt: now.Add(-ago), Status: queries.AuditComplete})
	}
	for _, ago := range []time.Duration{500 * 24 * time.Hour, 400 * 24 * time.Hour} {
		_ = queries.InsertDeviceAudit(db, &queries.DeviceAudit{DeviceID: "d", TakenAt: now.Add(-ago), Hash: ago.String(), Snapshot: "{}"})
	}
	_ = queries.StartFlowExportWatch(db, &queries.FlowExportWatch{DeviceID: "d", AcceptedAt: now.Add(-8 * 24 * time.Hour)})
	sweepAudit(db, now)
	if n := countRows(t, db, `SELECT COUNT(*) FROM device_audit_runs`); n != 1 {
		t.Errorf("%d passes left, want the recent one", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM device_audit`); n != 1 {
		t.Errorf("%d snapshots left, want the device's newest", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM flow_export_watches`); n != 0 {
		t.Errorf("%d watches left", n)
	}
}

// With reads on their own goroutines — as in production — and an admin
// asking while the loop ticks, a pass still sends each read once, and the
// race detector has nothing to say.
func TestAuditorWithConcurrentReadsAndRequests(t *testing.T) {
	h := newAuditHarness(t)
	h.add("r", hygienetest.Load(t, "ap_clean"))
	var reads sync.WaitGroup
	h.a.spawn = func(f func()) {
		reads.Add(1)
		go func() {
			defer reads.Done()
			f()
		}()
	}

	if _, err := h.a.Request("r", queries.AuditKindAudit, "alice"); err != nil {
		t.Fatalf("audit now: %v", err)
	}
	stop := make(chan struct{})
	var asking sync.WaitGroup
	asking.Add(1)
	go func() { // the API, asking while the pass runs
		defer asking.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = h.a.Request("r", queries.AuditKindAudit, "bob")
			_ = h.a.Running("r")
			_ = h.a.NextAllowed("r", queries.AuditKindAudit, h.clock.now())
		}
	}()
	// Bounded by steps, not by time: each step is two seconds of fake time
	// and waits for the read it started.
	for i := 0; i < 5000 && h.a.Running("r") != ""; i++ {
		h.clock.set(h.clock.now().Add(2 * time.Second))
		h.a.tick(h.clock.now())
		reads.Wait()
	}
	close(stop)
	asking.Wait()
	reads.Wait()

	r := h.lastRun("r", queries.AuditKindAudit)
	if r == nil || r.Status != queries.AuditComplete || r.Reads != 12 {
		t.Fatalf("run = %+v", r)
	}
	seen := map[string]bool{}
	for _, c := range h.commands("r") {
		name := strings.Fields(c.command)[0]
		if seen[name] {
			t.Fatalf("%s was sent twice in one pass", name)
		}
		seen[name] = true
	}
	if len(seen) != 12 {
		t.Fatalf("%d commands, want the audit's 12", len(seen))
	}
}
