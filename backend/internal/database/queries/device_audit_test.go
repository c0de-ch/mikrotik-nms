package queries

import (
	"database/sql"
	"reflect"
	"testing"
	"time"
)

func auditTestDevices(t *testing.T, ids ...string) *sql.DB {
	t.Helper()
	db := testDB(t)
	for i, id := range ids {
		d := &Device{ID: id, Address: "192.0.2." + string(rune('1'+i)), Identity: id, Username: "nms", APIPort: 8728, Status: "online"}
		if err := CreateDevice(db, d); err != nil {
			t.Fatalf("create device %s: %v", id, err)
		}
	}
	return db
}

func TestAuditFindingsUpsertAndReplace(t *testing.T) {
	db := auditTestDevices(t, "d1", "d2")
	t1 := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	graph := &DeviceFinding{DeviceID: "d1", Kind: "graphing_enabled", Severity: "critical", Detail: "interface rule interface=all",
		Title: "Graphing is enabled on a weak device", Explanation: "why it matters",
		Fix: []FindingFix{{Text: "Remove the rule:", Commands: []string{`/tool graphing interface remove [find interface="all"]`}},
			{Text: "In a window.", Window: true}}}
	snmp := &DeviceFinding{DeviceID: "d1", Kind: "snmp_open", Severity: "warn", Detail: "1 community", Title: "SNMP open"}
	for _, f := range []*DeviceFinding{graph, snmp} {
		if err := UpsertAuditFinding(db, f, t1); err != nil {
			t.Fatal(err)
		}
	}
	// A finding the login watch owns sits next to them.
	if err := UpsertDeviceFinding(db, &DeviceFinding{DeviceID: "d1", Kind: "parallel_api_poller", Severity: "warn", Detail: "user x from 198.51.100.2 via api"}); err != nil {
		t.Fatal(err)
	}

	// Seen again six hours later: one row, counted twice, with the new text.
	graph.Explanation = "why it still matters"
	if err := UpsertAuditFinding(db, graph, t1.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := ListDeviceFindings(db, "d1", 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %d, %v", len(rows), err)
	}
	var got DeviceFinding
	for _, r := range rows {
		if r.Kind == "graphing_enabled" {
			got = r
		}
	}
	if got.Count != 2 || got.Source != FindingSourceAudit || got.Explanation != "why it still matters" ||
		!got.FirstSeen.Equal(t1) || !got.LastSeen.Equal(t1.Add(6*time.Hour)) || !reflect.DeepEqual(got.Fix, graph.Fix) {
		t.Fatalf("stored finding = %+v", got)
	}

	// A later pass evaluated graphing and SNMP and found only SNMP: the
	// graphing finding is gone; kinds it did not evaluate, the other device
	// and the login watch's row are untouched.
	if err := UpsertAuditFinding(db, &DeviceFinding{DeviceID: "d2", Kind: "graphing_enabled", Severity: "warn", Detail: "x"}, t1); err != nil {
		t.Fatal(err)
	}
	if err := UpsertAuditFinding(db, &DeviceFinding{DeviceID: "d1", Kind: "sniffer_running", Severity: "warn", Detail: "running"}, t1); err != nil {
		t.Fatal(err)
	}
	n, err := DeleteAuditFindingsExcept(db, "d1", []string{"graphing_enabled", "snmp_open", "parallel_api_poller"},
		[]FindingKey{{Kind: "snmp_open", Detail: "1 community"}})
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v; want exactly the graphing finding", n, err)
	}
	left := map[string]bool{}
	rows, _ = ListDeviceFindings(db, "d1", 0)
	for _, r := range rows {
		left[r.Kind] = true
	}
	if !reflect.DeepEqual(left, map[string]bool{"snmp_open": true, "sniffer_running": true, "parallel_api_poller": true}) {
		t.Fatalf("left on d1: %v", left)
	}
	if rows, _ = ListDeviceFindings(db, "d2", 0); len(rows) != 1 {
		t.Fatalf("d2 lost its finding: %v", rows)
	}

	// The fleet-wide list holds the audit's findings only, worst first.
	all, err := ListAuditFindings(db)
	if err != nil || len(all) != 3 {
		t.Fatalf("audit findings = %d, %v", len(all), err)
	}
	for _, f := range all {
		if f.Source != FindingSourceAudit {
			t.Fatalf("%s is not an audit finding", f.Kind)
		}
	}
	// Nothing evaluated: nothing deleted.
	if n, _ := DeleteAuditFindingsExcept(db, "d1", nil, nil); n != 0 {
		t.Fatalf("deleted %d rows without an evaluated kind", n)
	}
}

// A finding of a kind the audit shares with the login watch is held while a
// pass sees it — it is current whatever its age — and let go of, never
// deleted, by the pass that no longer does.
func TestAuditHoldsAndReleasesSharedFindings(t *testing.T) {
	db := auditTestDevices(t, "d1", "d2")
	t1 := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	const kind = "parallel_api_poller"
	// The login watch recorded one; the audit then saw it, and two more.
	if err := UpsertDeviceFinding(db, &DeviceFinding{DeviceID: "d1", Kind: kind, Severity: "warn", Detail: "user a from 198.51.100.1 via api"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*DeviceFinding{
		{DeviceID: "d1", Kind: kind, Severity: "warn", Detail: "user a from 198.51.100.1 via api", Title: "Another API client is logged in"},
		{DeviceID: "d1", Kind: kind, Severity: "warn", Detail: "user b from 198.51.100.2 via api"},
		{DeviceID: "d2", Kind: kind, Severity: "warn", Detail: "user b from 198.51.100.2 via api"},
	} {
		if err := UpsertAuditFinding(db, f, t1); err != nil {
			t.Fatal(err)
		}
	}
	current := func(at time.Time) []string {
		t.Helper()
		rows, err := ListCurrentDeviceFindings(db, kind, at.Add(-2*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, r := range rows {
			out = append(out, r.DeviceID+" "+r.Detail[5:6])
		}
		return out
	}
	week := t1.Add(7 * 24 * time.Hour)
	if got := current(week); len(got) != 3 {
		t.Fatalf("held findings a week after they were seen = %v, want all three", got)
	}
	// A pass on d1 still sees a, not b.
	n, err := ReleaseAuditFindings(db, "d1", kind, []string{"user a from 198.51.100.1 via api"})
	if err != nil || n != 1 {
		t.Fatalf("released %d, %v; want b of d1", n, err)
	}
	if got := current(week); len(got) != 2 || got[0] != "d1 a" || got[1] != "d2 b" {
		t.Fatalf("after the release = %v, want a of d1 and b of d2", got)
	}
	// The released row is still there, and current while it was seen lately.
	if got := current(t1.Add(time.Hour)); len(got) != 3 {
		t.Fatalf("an hour after it was seen = %v, want the released one too", got)
	}
	rows, _ := ListDeviceFindings(db, "d1", 0)
	if len(rows) != 2 {
		t.Fatalf("d1 has %d findings, want both rows kept", len(rows))
	}
}

func TestDeviceAuditSnapshots(t *testing.T) {
	db := auditTestDevices(t, "d1")
	if a, err := LatestDeviceAudit(db, "d1"); err != nil || a != nil {
		t.Fatalf("no audit yet: %+v, %v", a, err)
	}
	t1 := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	for i, hash := range []string{"aaa", "bbb"} {
		if err := InsertDeviceAudit(db, &DeviceAudit{DeviceID: "d1", TakenAt: t1.Add(time.Duration(i) * 6 * time.Hour), Hash: hash,
			ROSVersion: "7.20.4", Snapshot: `{"schema":1}`, Findings: `[]`}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := LatestDeviceAudit(db, "d1")
	if err != nil || a == nil || a.Hash != "bbb" || !a.TakenAt.Equal(t1.Add(6*time.Hour)) || a.ROSVersion != "7.20.4" {
		t.Fatalf("latest = %+v, %v", a, err)
	}
	// Retention keeps a device's newest snapshot however old it is.
	n, err := DeleteOldDeviceAudits(db, t1.Add(1000*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v; want the older one only", n, err)
	}
	if a, _ = LatestDeviceAudit(db, "d1"); a == nil || a.Hash != "bbb" {
		t.Fatalf("the newest snapshot was deleted: %+v", a)
	}
}

func TestAuditRuns(t *testing.T) {
	db := auditTestDevices(t, "d1", "d2")
	t1 := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

	if r, err := LatestAuditRun(db, "d1", AuditKindAudit); err != nil || r != nil {
		t.Fatalf("no run yet: %+v, %v", r, err)
	}
	run := &AuditRun{DeviceID: "d1", Kind: AuditKindAudit, Reason: "scheduled", StartedAt: t1}
	id, err := InsertAuditRun(db, run)
	if err != nil || id == 0 {
		t.Fatalf("insert: %d, %v", id, err)
	}
	got, _ := LatestAuditRun(db, "d1", AuditKindAudit)
	if got == nil || got.Status != AuditRunning || !got.FinishedAt.IsZero() || !got.StartedAt.Equal(t1) {
		t.Fatalf("running run = %+v", got)
	}
	// A run still marked running was cut off by a restart.
	if done, _ := LatestFinishedAuditRun(db, "d1", AuditKindAudit); done != nil {
		t.Fatalf("a running run counts as finished: %+v", done)
	}

	run.ID, run.FinishedAt, run.Status = id, t1.Add(6*time.Minute), AuditComplete
	run.Reads, run.FailedReads, run.Hash, run.Runtime, run.Findings = 17, 0, "aaa", `{"at":"2026-03-02T12:06:00Z"}`, `[{"kind":"snmp_open"}]`
	if err := FinishAuditRun(db, run); err != nil {
		t.Fatal(err)
	}
	done, err := LatestFinishedAuditRun(db, "d1", AuditKindAudit)
	if err != nil || done == nil || done.Reads != 17 || done.Hash != "aaa" || done.Findings != `[{"kind":"snmp_open"}]` ||
		!done.FinishedAt.Equal(t1.Add(6*time.Minute)) {
		t.Fatalf("finished run = %+v, %v", done, err)
	}

	// A diagnose pass for an alert, on the other device.
	did, _ := InsertAuditRun(db, &AuditRun{DeviceID: "d2", Kind: AuditKindDiagnose, Reason: "cpu_high", AlertID: 42, StartedAt: t1.Add(time.Hour)})
	if err := FinishAuditRun(db, &AuditRun{ID: did, FinishedAt: t1.Add(time.Hour + 5*time.Minute), Status: AuditPartial, Reads: 9, FailedReads: 1,
		Detail: "1 read failed", Findings: `[]`}); err != nil {
		t.Fatal(err)
	}
	// An older diagnose pass of d2, and a newer one that judged nothing.
	old, _ := InsertAuditRun(db, &AuditRun{DeviceID: "d2", Kind: AuditKindDiagnose, Reason: "admin", StartedAt: t1.Add(-10 * time.Hour)})
	_ = FinishAuditRun(db, &AuditRun{ID: old, FinishedAt: t1.Add(-10 * time.Hour), Status: AuditComplete, Findings: `[]`})
	cut, _ := InsertAuditRun(db, &AuditRun{DeviceID: "d2", Kind: AuditKindDiagnose, Reason: "admin", StartedAt: t1.Add(3 * time.Hour)})
	_ = FinishAuditRun(db, &AuditRun{ID: cut, FinishedAt: t1.Add(3 * time.Hour), Status: AuditAborted, Findings: `[]`})

	// Each device's newest diagnose pass that judged something, however old
	// (d1 has had none).
	latest, err := ListLatestDiagnoseRuns(db)
	if err != nil || len(latest) != 1 || latest["d2"].AlertID != 42 || latest["d2"].Status != AuditPartial ||
		!latest["d2"].StartedAt.Equal(t1.Add(time.Hour)) {
		t.Fatalf("latest diagnose runs = %+v, %v", latest, err)
	}

	// When each device last finished an audit (complete or partial): the
	// schedule is seeded from it after a restart.
	times, err := ListLastAuditTimes(db)
	if err != nil || len(times) != 1 || !times["d1"].Equal(t1.Add(6*time.Minute)) {
		t.Fatalf("last audit times = %v, %v", times, err)
	}

	// The newest stored runtime of a device, whatever the kind of pass.
	if rt, at, err := LatestAuditRuntime(db, "d1"); err != nil || rt != `{"at":"2026-03-02T12:06:00Z"}` || !at.Equal(t1.Add(6*time.Minute)) {
		t.Fatalf("runtime = %q at %v, %v", rt, at, err)
	}
	if rt, _, _ := LatestAuditRuntime(db, "d2"); rt != "" {
		t.Fatalf("d2 has no runtime, got %q", rt)
	}

	// A restart: what was running is aborted, finished runs are untouched.
	stuck, _ := InsertAuditRun(db, &AuditRun{DeviceID: "d1", Kind: AuditKindDiagnose, Reason: "admin", StartedAt: t1.Add(2 * time.Hour)})
	n, err := AbortRunningAuditRuns(db, t1.Add(3*time.Hour), "the NMS restarted")
	if err != nil || n != 1 {
		t.Fatalf("aborted %d, %v", n, err)
	}
	got, _ = LatestAuditRun(db, "d1", AuditKindDiagnose)
	if got.ID != stuck || got.Status != AuditAborted || got.Detail != "the NMS restarted" || got.FinishedAt.IsZero() {
		t.Fatalf("aborted run = %+v", got)
	}

	// Retention.
	if n, err := DeleteOldAuditRuns(db, t1.Add(-time.Hour)); err != nil || n != 1 {
		t.Fatalf("deleted %d old runs, %v", n, err)
	}
}

func TestFlowExportWatches(t *testing.T) {
	db := auditTestDevices(t, "r1")
	t1 := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	baseline := 12.5
	if err := StartFlowExportWatch(db, &FlowExportWatch{DeviceID: "r1", ExporterID: 3, Address: "192.0.2.1", AcceptedAt: t1, Baseline: &baseline}); err != nil {
		t.Fatal(err)
	}
	// Accepted again (the exporter was re-added): the watch starts over.
	if err := StartFlowExportWatch(db, &FlowExportWatch{DeviceID: "r1", ExporterID: 4, Address: "192.0.2.1", AcceptedAt: t1.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ws, err := ListFlowExportWatches(db)
	if err != nil || len(ws) != 1 || ws[0].ExporterID != 4 || !ws[0].AcceptedAt.Equal(t1.Add(time.Hour)) || ws[0].Baseline != nil {
		t.Fatalf("watches = %+v, %v", ws, err)
	}
	// Ending a watch names the accept it belongs to: one that was started
	// again since is left alone.
	if err := EndFlowExportWatch(db, "r1", t1); err != nil {
		t.Fatal(err)
	}
	if ws, _ := ListFlowExportWatches(db); len(ws) != 1 {
		t.Fatalf("ending the earlier accept removed the watch that was started again: %+v", ws)
	}
	if err := EndFlowExportWatch(db, "r1", t1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ws, _ := ListFlowExportWatches(db); len(ws) != 0 {
		t.Fatalf("watches after its end = %+v", ws)
	}

	_ = StartFlowExportWatch(db, &FlowExportWatch{DeviceID: "r1", ExporterID: 4, Address: "192.0.2.1", AcceptedAt: t1.Add(time.Hour)})
	if n, err := DeleteOldFlowExportWatches(db, t1.Add(2*time.Hour)); err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
}

// A removed exporter takes its CPU watch with it — whichever way it is
// removed: nothing is exported to the NMS any more, so there is no "after
// its flow export was accepted" left to watch.
func TestRemovingAnExporterEndsItsWatch(t *testing.T) {
	db := auditTestDevices(t, "r1", "r2", "r3")
	t1 := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	ids := map[string]int64{}
	for i, dev := range []string{"r1", "r2", "r3"} {
		id, err := CreateFlowExporter(db, &FlowExporter{Name: dev, Address: "192.0.2." + string(rune('1'+i)), Kind: "routeros", DeviceID: dev, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		ids[dev] = id
		if err := StartFlowExportWatch(db, &FlowExportWatch{DeviceID: dev, ExporterID: id, Address: "192.0.2." + string(rune('1'+i)), AcceptedAt: t1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteFlowExporter(db, ids["r1"]); err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteFlowExporterBlocking(db, ids["r2"], "removed", "admin", t1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ws, err := ListFlowExportWatches(db)
	if err != nil || len(ws) != 1 || ws[0].DeviceID != "r3" {
		t.Fatalf("watches after two exporters were removed = %+v, %v; want the third device's only", ws, err)
	}
}

func TestDeviceCPUBaseline(t *testing.T) {
	db := auditTestDevices(t, "r1")
	t1 := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	rows := []DeviceMinute{
		{DeviceID: "r1", Bucket: t1.Add(-3 * time.Hour), PolledS: 60, CPUSum: 10, CPUSamples: 1, CPUMax: 10},
		{DeviceID: "r1", Bucket: t1.Add(-2 * time.Hour), PolledS: 60, CPUSum: 60, CPUSamples: 2, CPUMax: 40},  // average 30
		{DeviceID: "r1", Bucket: t1.Add(-time.Hour), PolledS: 60},                                             // no sample
		{DeviceID: "r1", Bucket: t1.Add(-30 * time.Hour), PolledS: 60, CPUSum: 99, CPUSamples: 1, CPUMax: 99}, // before the window
		{DeviceID: "r1", Bucket: t1, PolledS: 60, CPUSum: 99, CPUSamples: 1, CPUMax: 99},                      // not before t1
	}
	if err := UpsertDeviceMinutes(db, rows); err != nil {
		t.Fatal(err)
	}
	avg, minutes, err := DeviceCPUBaseline(db, "r1", t1.Add(-24*time.Hour), t1)
	if err != nil || minutes != 2 || avg != 20 {
		t.Fatalf("baseline = %v over %d minutes, %v; want 20 over 2", avg, minutes, err)
	}
	if _, minutes, _ := DeviceCPUBaseline(db, "none", t1.Add(-24*time.Hour), t1); minutes != 0 {
		t.Fatalf("a device without minutes has %d", minutes)
	}
}

func TestBridgePortStatusKeepsHWAndHorizon(t *testing.T) {
	db := auditTestDevices(t, "s1")
	yes, no := true, false
	for _, p := range []*BridgePortStatus{
		{ID: "s1:bridge:ether1", DeviceID: "s1", BridgeName: "bridge", PortInterface: "ether1", HW: &yes, HWOffload: &no, Horizon: "none"},
		{ID: "s1:bridge:ether2", DeviceID: "s1", BridgeName: "bridge", PortInterface: "ether2", Horizon: "3"},
	} {
		if err := UpsertBridgePortStatus(db, p); err != nil {
			t.Fatal(err)
		}
	}
	ports, err := ListBridgePortStatusByDevice(db, "s1")
	if err != nil || len(ports) != 2 {
		t.Fatalf("ports = %d, %v", len(ports), err)
	}
	if ports[0].HW == nil || !*ports[0].HW || ports[0].HWOffload == nil || *ports[0].HWOffload || ports[0].Horizon != "none" {
		t.Fatalf("ether1 = %+v", ports[0])
	}
	if ports[1].HW != nil || ports[1].Horizon != "3" { // not reported stays unknown
		t.Fatalf("ether2 = %+v", ports[1])
	}
	if other, _ := ListBridgePortStatusByDevice(db, "nope"); len(other) != 0 {
		t.Fatalf("another device's ports: %v", other)
	}
}
