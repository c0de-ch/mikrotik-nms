package alerts

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// ---- the engine: "cannot tell" and escalation --------------------------------

// A rule that cannot tell for a key holds its alert: an open one stays as it
// is (not resolved, not touched), and none is opened. Unknown is not healthy.
func TestHoldNeitherOpensNorResolves(t *testing.T) {
	rule := &fakeRule{name: "r", obs: []Observation{{Key: "d1", DeviceID: "d1", Hold: true}}}
	e, d, now := newTestEngine(t, rule)

	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 0 {
		t.Fatalf("a hold opened an alert: %+v", open)
	}

	rule.obs = []Observation{{Key: "d1", DeviceID: "d1", Severity: SeverityWarning, Summary: "busy"}}
	mustTick(t, e)
	first := openAlerts(t, e)
	if len(first) != 1 {
		t.Fatalf("open = %+v", first)
	}
	d.take()

	// The device goes quiet: the rule can no longer tell.
	rule.obs = []Observation{{Key: "d1", DeviceID: "d1", Hold: true}}
	*now = now.Add(time.Hour)
	mustTick(t, e)
	open := openAlerts(t, e)
	if len(open) != 1 || open[0].ID != first[0].ID || !open[0].LastAt.Equal(first[0].LastAt) || ParsePayload(open[0].Payload).Summary != "busy" {
		t.Fatalf("a held alert must stay exactly as it was: %+v", open)
	}
	if got := d.take(); len(got) != 0 {
		t.Fatalf("a hold notified: %v", got)
	}

	// A device-wide hold keeps every alert of the rule for that device —
	// and no other device's.
	if _, err := queries.InsertAlert(e.db, &queries.Alert{Rule: "r", Key: "d1|ether2", DeviceID: "d1", Severity: SeverityWarning, FirstAt: *now, LastAt: *now, Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.InsertAlert(e.db, &queries.Alert{Rule: "r", Key: "d2|ether1", DeviceID: "d2", Severity: SeverityWarning, FirstAt: *now, LastAt: *now, Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	rule.obs = []Observation{{DeviceID: "d1", Hold: true}}
	mustTick(t, e)
	keys := map[string]bool{}
	for _, a := range openAlerts(t, e) {
		keys[a.Key] = true
	}
	if !keys["d1"] || !keys["d1|ether2"] || keys["d2|ether1"] {
		t.Fatalf("open keys = %v, want both d1 alerts held and d2's resolved", keys)
	}

	// Once the rule can tell again and the condition is gone, it resolves.
	rule.obs = nil
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 0 {
		t.Fatalf("still open after a clear evaluation: %+v", open)
	}
}

// An alert that gets worse is announced again at once, not at the next
// four-hourly reminder; one that is acknowledged stays quiet.
func TestEscalationIsNotifiedAgain(t *testing.T) {
	rule := &fakeRule{name: "r", obs: []Observation{{Key: "d1", Severity: SeverityWarning, Summary: "warm"}}}
	e, d, now := newTestEngine(t, rule)
	mustTick(t, e)
	if got := d.take(); !sameSet(got, []string{"firing r/d1"}) {
		t.Fatalf("first: %v", got)
	}

	rule.obs = []Observation{{Key: "d1", Severity: SeverityCritical, Summary: "hot"}}
	*now = now.Add(time.Minute)
	mustTick(t, e)
	if got := d.take(); !sameSet(got, []string{"firing r/d1"}) {
		t.Fatalf("escalation sent %v, want the alert again", got)
	}
	if open := openAlerts(t, e); len(open) != 1 || open[0].Severity != SeverityCritical || open[0].LastNotifiedAt == nil {
		t.Fatalf("after the escalation: %+v", open)
	}

	// Back to warning, and warning again: nothing more.
	rule.obs = []Observation{{Key: "d1", Severity: SeverityWarning, Summary: "warm"}}
	*now = now.Add(time.Minute)
	mustTick(t, e)
	if got := d.take(); len(got) != 0 {
		t.Fatalf("a de-escalation notified: %v", got)
	}

	// Acknowledged, then worse again: the operator already knows.
	open := openAlerts(t, e)
	if err := queries.AckAlert(e.db, open[0].ID, "kim", *now); err != nil {
		t.Fatal(err)
	}
	rule.obs = []Observation{{Key: "d1", Severity: SeverityCritical, Summary: "hot"}}
	*now = now.Add(time.Minute)
	mustTick(t, e)
	if got := d.take(); len(got) != 0 {
		t.Fatalf("an acknowledged alert was re-notified on escalation: %v", got)
	}
}

// ---- fixtures ---------------------------------------------------------------

// ruleNow is the evaluation time of the rule tests; the newest final minute
// is three minutes before it (the flush lags the clock).
var ruleNow = time.Date(2026, 11, 20, 12, 0, 30, 0, time.UTC)

func newest() time.Time { return ruleNow.Truncate(time.Minute).Add(-3 * time.Minute) }

// minuteAgo is the bucket n minutes before the newest final one.
func minuteAgo(n int) time.Time { return newest().Add(-time.Duration(n) * time.Minute) }

func addDevice(t *testing.T, db *sql.DB, id string, class devclass.Class, memTotal int64) {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateDevice(db, &queries.Device{ID: id, Identity: "sw-" + id, Address: fmt.Sprintf("192.0.2.%d", 10+n),
		Username: "u", APIPort: 8728, Status: "online", Tags: "[]"}); err != nil {
		t.Fatal(err)
	}
	if class != "" {
		if err := queries.SetDeviceClass(db, id, class, devclass.ResourceFacts{}, ruleNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE devices SET memory_total = ? WHERE id = ?`, memTotal, id); err != nil {
		t.Fatal(err)
	}
}

// cpuMinutes writes covered device-minutes with one CPU sample each: values
// are oldest first and end at the newest final minute. A negative value is a
// covered minute without a sample.
func cpuMinutes(t *testing.T, db *sql.DB, id string, values ...int64) {
	t.Helper()
	var rows []queries.DeviceMinute
	for i, v := range values {
		m := queries.DeviceMinute{DeviceID: id, Bucket: minuteAgo(len(values) - 1 - i), PolledS: 60}
		if v >= 0 {
			m.CPUSum, m.CPUSamples, m.CPUMax = v, 1, v
		}
		rows = append(rows, m)
	}
	if err := queries.UpsertDeviceMinutes(db, rows); err != nil {
		t.Fatal(err)
	}
}

func repeat(v int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// only returns the single non-hold observation, failing otherwise.
func firing(obs []Observation) []Observation {
	var out []Observation
	for _, o := range obs {
		if !o.Hold {
			out = append(out, o)
		}
	}
	return out
}

func holds(obs []Observation, key string) bool {
	for _, o := range obs {
		if o.Hold && (o.Key == key || (o.Key == "" && o.DeviceID == key)) {
			return true
		}
	}
	return false
}

// ---- cpu_high ---------------------------------------------------------------

func TestCPUHighRule(t *testing.T) {
	db := testDB(t)
	addDevice(t, db, "weak", devclass.C1, 64<<20)
	addDevice(t, db, "weak4", devclass.C1, 64<<20)
	addDevice(t, db, "capable", devclass.C3, 1<<30)
	addDevice(t, db, "unprobed", "", 64<<20)
	addDevice(t, db, "pegged", devclass.C3, 1<<30)
	addDevice(t, db, "spike", devclass.C1, 64<<20)
	addDevice(t, db, "calm", devclass.C1, 64<<20)

	cpuMinutes(t, db, "weak", repeat(82, 5)...)     // C1: ≥ 80 % for 5 min
	cpuMinutes(t, db, "weak4", 20, 82, 82, 82, 82)  // only 4 of the 5
	cpuMinutes(t, db, "capable", repeat(82, 5)...)  // C3 warns at 85
	cpuMinutes(t, db, "unprobed", repeat(81, 5)...) // budgeted as C1
	cpuMinutes(t, db, "pegged", repeat(97, 10)...)  // ≥ 95 % for 10 min
	cpuMinutes(t, db, "spike", 10, 10, 99, 10, 10)
	cpuMinutes(t, db, "calm", repeat(12, 5)...)

	obs, err := cpuHigh{db}.Evaluate(ruleNow)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Observation{}
	for _, o := range firing(obs) {
		got[o.Key] = o
	}
	if len(got) != 3 {
		t.Fatalf("firing = %+v, want weak, unprobed and pegged", got)
	}
	if o := got["weak"]; o.DeviceID != "weak" || o.Severity != SeverityWarning || !strings.Contains(o.Summary, "sw-weak") ||
		!strings.Contains(o.Summary, "80 %") || !strings.Contains(o.Summary, "5 minutes") {
		t.Errorf("weak = %+v", o)
	}
	if o := got["unprobed"]; o.Severity != SeverityWarning {
		t.Errorf("unprobed = %+v", o)
	}
	if o := got["pegged"]; o.Severity != SeverityCritical || !strings.Contains(o.Summary, "95 %") || !strings.Contains(o.Summary, "10 minutes") {
		t.Errorf("pegged = %+v", o)
	}
	// Not five minutes yet, and a single spike in an otherwise quiet window:
	// neither firing nor clearly healthy.
	if !holds(obs, "weak4") || !holds(obs, "spike") {
		t.Errorf("weak4 and spike must be held: %+v", obs)
	}
	// Below its threshold throughout: healthy, nothing reported.
	if holds(obs, "capable") || holds(obs, "calm") {
		t.Errorf("a device below its threshold for the whole window is healthy, not unknown: %+v", obs)
	}
}

// Minutes without samples never fire and never resolve: a device that goes
// quiet while its alert is open keeps it, and one that was never measured
// never gets one.
func TestCPUHighUnknownIsNotHealthy(t *testing.T) {
	db := testDB(t)
	addDevice(t, db, "d1", devclass.C1, 64<<20)
	addDevice(t, db, "never", devclass.C1, 64<<20)
	cpuMinutes(t, db, "d1", repeat(90, 5)...)
	e := NewEngine(db, nil, cpuHigh{db})
	now := ruleNow
	e.now = func() time.Time { return now }
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 1 || open[0].Key != "d1" || open[0].Rule != RuleCPUHigh {
		t.Fatalf("open = %+v", open)
	}

	// The device becomes unreachable: gap rows without samples, for an hour.
	var gaps []queries.DeviceMinute
	for m := 1; m <= 60; m++ {
		gaps = append(gaps, queries.DeviceMinute{DeviceID: "d1", Bucket: newest().Add(time.Duration(m) * time.Minute), GapReason: queries.GapUnreachable})
	}
	if err := queries.UpsertDeviceMinutes(db, gaps); err != nil {
		t.Fatal(err)
	}
	now = ruleNow.Add(time.Hour)
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 1 {
		t.Fatalf("the alert resolved while the device could not be measured: %+v", open)
	}

	// The NMS itself stops writing rows (stale data): still held.
	now = ruleNow.Add(3 * time.Hour)
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 1 {
		t.Fatalf("the alert resolved on stale data: %+v", open)
	}

	// It is measured again and calm for five minutes: resolved.
	base := now.Truncate(time.Minute).Add(-3 * time.Minute)
	var calm []queries.DeviceMinute
	for m := 4; m >= 0; m-- {
		calm = append(calm, queries.DeviceMinute{DeviceID: "d1", Bucket: base.Add(-time.Duration(m) * time.Minute), PolledS: 60, CPUSum: 20, CPUSamples: 1, CPUMax: 20})
	}
	if err := queries.UpsertDeviceMinutes(db, calm); err != nil {
		t.Fatal(err)
	}
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 0 {
		t.Fatalf("still open after five calm minutes: %+v", open)
	}
}

// ---- mem_low ----------------------------------------------------------------

func memMinutes(t *testing.T, db *sql.DB, id string, free ...int64) {
	t.Helper()
	var rows []queries.DeviceMinute
	for i, v := range free {
		m := queries.DeviceMinute{DeviceID: id, Bucket: minuteAgo(len(free) - 1 - i), PolledS: 60}
		if v >= 0 {
			m.CPUSum, m.CPUSamples, m.CPUMax = 5, 1, 5
			m.FreeMemoryMin = sql.NullInt64{Int64: v, Valid: true}
		}
		rows = append(rows, m)
	}
	if err := queries.UpsertDeviceMinutes(db, rows); err != nil {
		t.Fatal(err)
	}
}

func TestMemLowRule(t *testing.T) {
	const mib = int64(1 << 20)
	db := testDB(t)
	addDevice(t, db, "weak", devclass.C1, 64*mib)      // 5 % = 3.2 MiB; the 8 MiB floor decides
	addDevice(t, db, "weak-ok", devclass.C1, 64*mib)   // 9 MiB free
	addDevice(t, db, "big-low", devclass.C3, 1024*mib) // below 5 %
	addDevice(t, db, "big-ok", devclass.C3, 1024*mib)  // 60 MiB = 5.9 %
	addDevice(t, db, "dip", devclass.C3, 1024*mib)     // one low minute
	addDevice(t, db, "no-total", devclass.C3, 0)       // total unknown: 5 % cannot be told
	addDevice(t, db, "unsampled", devclass.C1, 64*mib) // covered, no samples
	memMinutes(t, db, "weak", 7*mib, 7*mib, 7*mib)
	memMinutes(t, db, "weak-ok", 9*mib, 9*mib, 9*mib)
	memMinutes(t, db, "big-low", 40*mib, 40*mib, 40*mib)
	memMinutes(t, db, "big-ok", 60*mib, 60*mib, 60*mib)
	memMinutes(t, db, "dip", 400*mib, 20*mib, 400*mib)
	memMinutes(t, db, "no-total", 7*mib, 7*mib, 7*mib)
	memMinutes(t, db, "unsampled", -1, -1, -1)

	obs, err := memLow{db}.Evaluate(ruleNow)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Observation{}
	for _, o := range firing(obs) {
		got[o.Key] = o
	}
	if len(got) != 2 {
		t.Fatalf("firing = %+v, want weak and big-low", got)
	}
	if o := got["weak"]; o.Severity != SeverityWarning || o.DeviceID != "weak" || !strings.Contains(o.Summary, "7 MiB") || !strings.Contains(o.Summary, "8 MiB") {
		t.Errorf("weak = %+v", o)
	}
	if o := got["big-low"]; !strings.Contains(o.Summary, "40 MiB") || !strings.Contains(o.Summary, "5 %") {
		t.Errorf("big-low = %+v", o)
	}
	for _, id := range []string{"dip", "no-total", "unsampled"} {
		if !holds(obs, id) {
			t.Errorf("%s must be held (cannot tell): %+v", id, obs)
		}
	}
	for _, id := range []string{"weak-ok", "big-ok"} {
		if holds(obs, id) {
			t.Errorf("%s is healthy, not unknown", id)
		}
	}
}

// ---- port_egress_drops ------------------------------------------------------

// portMinutes writes one port_stats_1m row per value (oldest first, ending
// at the newest final minute): drops on tx, at txPps packets per second.
func portMinutes(t *testing.T, db *sql.DB, dev, iface string, txPps int64, drops ...int64) {
	t.Helper()
	var rows []queries.PortStatRow
	for i, d := range drops {
		if d < 0 {
			continue // idle: no row
		}
		rows = append(rows, queries.PortStatRow{DeviceID: dev, InterfaceName: iface, Bucket: minuteAgo(len(drops) - 1 - i),
			TxBytes: txPps * 60 * 500, TxPpsAvg: txPps, Samples: 4,
			TxDrop: sql.NullInt64{Int64: d, Valid: true}, TxQueueDrop: sql.NullInt64{Valid: true}, CoveredS: sql.NullInt64{Int64: 60, Valid: true}})
	}
	if err := queries.InsertPortStats1m(db, rows); err != nil {
		t.Fatal(err)
	}
}

func coveredMinutes(t *testing.T, db *sql.DB, id string, n int) {
	t.Helper()
	cpuMinutes(t, db, id, repeat(5, n)...)
}

func TestPortEgressDropsRule(t *testing.T) {
	db := testDB(t)
	addDevice(t, db, "sw", devclass.C2, 256<<20)
	coveredMinutes(t, db, "sw", 6)

	portMinutes(t, db, "sw", "ether1", 100, 150, 150, 150, 150, 150)    // 2.5 % of 6000 packets, 5 minutes
	portMinutes(t, db, "sw", "ether2", 100, -1, 150, 150, 150, 150)     // only 4 minutes
	portMinutes(t, db, "sw", "ether3", 5000, 150, 150, 150, 150, 150)   // 0.05 % of 300000 packets
	portMinutes(t, db, "sw", "ether4", 10, 50, 50, 50, 50, 50)          // 8 % but under 100 drops a minute
	portMinutes(t, db, "sw", "ether5", 100, 0, 0, 0, 0, 0)              // traffic, no drops
	portMinutes(t, db, "sw", "ether6", 100, 150, 150, 150, 150, 150, 0) // dropped until a minute ago
	// The queue's drops count as egress drops too.
	queue := queries.PortStatRow{DeviceID: "sw", InterfaceName: "ether7", TxPpsAvg: 100, Samples: 4,
		TxDrop: sql.NullInt64{Int64: 20, Valid: true}, TxQueueDrop: sql.NullInt64{Int64: 130, Valid: true}, CoveredS: sql.NullInt64{Int64: 60, Valid: true}}
	// A port that does not report the counters at all can never fire.
	absent := queries.PortStatRow{DeviceID: "sw", InterfaceName: "ether8", TxPpsAvg: 1, TxBytes: 1000, Samples: 4, CoveredS: sql.NullInt64{Int64: 60, Valid: true}}
	var extra []queries.PortStatRow
	for m := 0; m < 5; m++ {
		queue.Bucket, absent.Bucket = minuteAgo(m), minuteAgo(m)
		extra = append(extra, queue, absent)
	}
	if err := queries.InsertPortStats1m(db, extra); err != nil {
		t.Fatal(err)
	}

	obs, err := portEgressDrops{db}.Evaluate(ruleNow)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Observation{}
	for _, o := range firing(obs) {
		got[o.Key] = o
	}
	if len(got) != 2 {
		t.Fatalf("firing = %+v, want ether1 and ether7", got)
	}
	o := got["sw|ether1"]
	if o.DeviceID != "sw" || o.Severity != SeverityWarning || !strings.Contains(o.Summary, "sw-sw ether1") || !strings.Contains(o.Summary, "150") {
		t.Errorf("ether1 = %+v", o)
	}
	if _, ok := got["sw|ether7"]; !ok {
		t.Errorf("ether7 (queue drops) did not fire: %+v", got)
	}
	// Four of five minutes, and five of six with the newest one clean: the
	// condition does not hold, but it is not clear either.
	if !holds(obs, "sw|ether2") || !holds(obs, "sw|ether6") {
		t.Errorf("ether2 and ether6 must be held: %+v", obs)
	}
	for _, key := range []string{"sw|ether3", "sw|ether4", "sw|ether5", "sw|ether8"} {
		if holds(obs, key) {
			t.Errorf("%s never met the condition in the window: it is clear, not held", key)
		}
	}
}

// A minute the device was not covered in is unknown for all of its ports: an
// open alert is kept, none is opened, and it resolves only on five covered
// minutes without the condition.
func TestPortEgressDropsUnknownIsNotHealthy(t *testing.T) {
	db := testDB(t)
	addDevice(t, db, "sw", devclass.C2, 256<<20)
	coveredMinutes(t, db, "sw", 5)
	portMinutes(t, db, "sw", "ether1", 100, 150, 150, 150, 150, 150)
	e := NewEngine(db, nil, portEgressDrops{db})
	now := ruleNow
	e.now = func() time.Time { return now }
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 1 || open[0].Key != "sw|ether1" || open[0].DeviceID != "sw" {
		t.Fatalf("open = %+v", open)
	}

	// The device is paused for ten minutes: no port rows, gap device-minutes.
	var rows []queries.DeviceMinute
	for m := 1; m <= 10; m++ {
		rows = append(rows, queries.DeviceMinute{DeviceID: "sw", Bucket: newest().Add(time.Duration(m) * time.Minute), Paused: true, GapReason: queries.GapPaused})
	}
	if err := queries.UpsertDeviceMinutes(db, rows); err != nil {
		t.Fatal(err)
	}
	now = ruleNow.Add(10 * time.Minute)
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 1 {
		t.Fatalf("the alert resolved while the device was paused: %+v", open)
	}

	// Five covered minutes without drops: resolved.
	rows = rows[:0]
	for m := 11; m <= 15; m++ {
		rows = append(rows, queries.DeviceMinute{DeviceID: "sw", Bucket: newest().Add(time.Duration(m) * time.Minute), PolledS: 60})
	}
	if err := queries.UpsertDeviceMinutes(db, rows); err != nil {
		t.Fatal(err)
	}
	now = ruleNow.Add(15 * time.Minute)
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 0 {
		t.Fatalf("still open after five covered minutes without drops: %+v", open)
	}
}

func TestLoadRulesAreRegistered(t *testing.T) {
	names := map[string]bool{}
	for _, n := range RuleNames() {
		names[n] = true
	}
	rules := map[string]bool{}
	for _, r := range DefaultRules(testDB(t)) {
		rules[r.Name()] = true
	}
	for _, want := range []string{RuleCPUHigh, RuleMemLow, RulePortEgressDrops, RuleDeviceUnreachable, RuleParallelAPIPoller} {
		if !names[want] || !rules[want] {
			t.Errorf("rule %s: in RuleNames %v, in DefaultRules %v", want, names[want], rules[want])
		}
	}
	if len(names) != len(rules) {
		t.Errorf("RuleNames (%d) and DefaultRules (%d) disagree", len(names), len(rules))
	}
}
