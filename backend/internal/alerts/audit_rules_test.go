package alerts

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/mailer"
)

func auditFinding(t *testing.T, db *sql.DB, device, kind, severity, title, detail string, fix ...queries.FindingFix) {
	t.Helper()
	if err := queries.UpsertAuditFinding(db, &queries.DeviceFinding{DeviceID: device, Kind: kind, Severity: severity,
		Title: title, Detail: detail, Explanation: "because", Fix: fix}, ruleNow); err != nil {
		t.Fatal(err)
	}
}

// ---- audit_critical ---------------------------------------------------------------

// One alert per device with a critical hygiene finding, for as long as the
// finding stands: it is removed by the pass that finds the setting gone, not
// by the clock.
func TestAuditCriticalRule(t *testing.T) {
	db := testDB(t)
	addDevice(t, db, "sw", devclass.C1, 64<<20)
	addDevice(t, db, "ap", devclass.C3, 1<<30)
	addDevice(t, db, "rt", devclass.C3, 1<<30)

	auditFinding(t, db, "sw", "graphing_enabled", "critical", "Graphing is enabled on a weak device", "interface rule interface=all",
		queries.FindingFix{Text: "Remove the graphing rules:", Commands: []string{`/tool graphing interface remove [find interface="all"]`}})
	auditFinding(t, db, "sw", "switch_mirror_to_cpu", "critical", "The switch mirrors traffic to the CPU", "switch1: mirror-target=cpu",
		queries.FindingFix{Text: "In a window.", Window: true})
	auditFinding(t, db, "sw", "snmp_on_weak_device", "info", "SNMP is enabled on a weak device", "SNMP enabled")
	auditFinding(t, db, "ap", "device_mode_permissive", "info", "Device-mode", "allows fetch") // nothing critical
	auditFinding(t, db, "rt", "snmp_open", "warn", "SNMP open", "1 community")                 // a warning is not an alert
	// A critical finding that is not the audit's has its own rule.
	if err := queries.UpsertDeviceFinding(db, &queries.DeviceFinding{DeviceID: "rt", Kind: "tls_pin_mismatch", Severity: "critical", Detail: "ab12"}); err != nil {
		t.Fatal(err)
	}

	rule := auditCritical{db}
	obs, err := rule.Evaluate(ruleNow)
	if err != nil || len(obs) != 1 {
		t.Fatalf("observations = %+v, %v; want one, for the switch", obs, err)
	}
	o := obs[0]
	if o.Key != "sw" || o.DeviceID != "sw" || o.Severity != SeverityCritical || o.Hold || o.Unconfirmed {
		t.Fatalf("observation = %+v", o)
	}
	if !strings.Contains(o.Summary, "sw-sw") || !strings.Contains(o.Summary, "2 critical hygiene findings") ||
		!strings.Contains(o.Summary, "Graphing is enabled on a weak device") {
		t.Errorf("summary = %q", o.Summary)
	}
	findings := factStrings(t, o, "findings")
	if len(findings) != 2 || !strings.Contains(findings[0], "interface rule interface=all") || strings.Contains(strings.Join(findings, " "), "SNMP") {
		t.Errorf("findings fact = %q", findings)
	}
	fix := strings.Join(factStrings(t, o, "fix"), "\n")
	if !strings.Contains(fix, `/tool graphing interface remove [find interface="all"]`) || !strings.Contains(fix, "maintenance window") {
		t.Errorf("fix fact =\n%s", fix)
	}

	// Still there a week later, untouched: a finding does not age out — a
	// device whose CPU is pegged is not audited, and its finding must stay.
	if obs, _ = rule.Evaluate(ruleNow.Add(7 * 24 * time.Hour)); len(obs) != 1 {
		t.Fatalf("after a week: %+v", obs)
	}

	// The next pass finds graphing gone and the mirror still there.
	if _, err := queries.DeleteAuditFindingsExcept(db, "sw", []string{"graphing_enabled", "switch_mirror_to_cpu"},
		[]queries.FindingKey{{Kind: "switch_mirror_to_cpu", Detail: "switch1: mirror-target=cpu"}}); err != nil {
		t.Fatal(err)
	}
	if obs, _ = rule.Evaluate(ruleNow); len(obs) != 1 || !strings.Contains(obs[0].Summary, "1 critical hygiene finding:") {
		t.Fatalf("after the fix: %+v", obs)
	}
	// And then the mirror too: the alert has nothing left to stand on.
	if _, err := queries.DeleteAuditFindingsExcept(db, "sw", []string{"switch_mirror_to_cpu"}, nil); err != nil {
		t.Fatal(err)
	}
	if obs, _ = rule.Evaluate(ruleNow); len(obs) != 0 {
		t.Fatalf("after both fixes: %+v", obs)
	}
}

// ---- the likely cause ---------------------------------------------------------------

func diagnoseRun(t *testing.T, db *sql.DB, device string, finished time.Time, status string, reads, failed int, findings string) {
	t.Helper()
	id, err := queries.InsertAuditRun(db, &queries.AuditRun{DeviceID: device, Kind: queries.AuditKindDiagnose, Reason: RuleCPUHigh,
		StartedAt: finished.Add(-5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.FinishAuditRun(db, &queries.AuditRun{ID: id, FinishedAt: finished, Status: status, Reads: reads,
		FailedReads: failed, Findings: findings}); err != nil {
		t.Fatal(err)
	}
}

// The diagnose pass's findings ride on the cpu_high alert as its likely
// cause — also when it found nothing, which is an answer too.
func TestCPUHighCarriesTheLikelyCause(t *testing.T) {
	db := testDB(t)
	addDevice(t, db, "graph", devclass.C1, 64<<20)
	addDevice(t, db, "clean", devclass.C1, 64<<20)
	addDevice(t, db, "stale", devclass.C1, 64<<20)
	addDevice(t, db, "none", devclass.C1, 64<<20)
	for _, id := range []string{"graph", "clean", "stale", "none"} {
		cpuMinutes(t, db, id, repeat(90, 5)...)
	}
	diagnoseRun(t, db, "graph", ruleNow.Add(-4*time.Minute), queries.AuditComplete, 11, 0,
		`[{"kind":"snmp_on_weak_device","severity":"info","title":"SNMP is enabled on a weak device","detail":"SNMP enabled"},
		  {"kind":"graphing_enabled","severity":"critical","title":"Graphing is enabled on a weak device","detail":"interface rule interface=all"}]`)
	diagnoseRun(t, db, "clean", ruleNow.Add(-10*time.Minute), queries.AuditPartial, 10, 1, `[]`)
	diagnoseRun(t, db, "stale", ruleNow.Add(-5*time.Hour), queries.AuditComplete, 11, 0,
		`[{"kind":"sniffer_running","severity":"warn","title":"The packet sniffer is running","detail":"running"}]`)

	obs, err := cpuHigh{db}.Evaluate(ruleNow)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Observation{}
	for _, o := range firing(obs) {
		got[o.Key] = o
	}

	cause := factStrings(t, got["graph"], "likely_cause")
	if len(cause) != 2 || !strings.HasPrefix(cause[0], "[critical] Graphing is enabled on a weak device — interface rule interface=all") ||
		!strings.HasPrefix(cause[1], "[info] SNMP") {
		t.Errorf("likely cause = %q, want the critical finding first", cause)
	}
	if at, _ := got["graph"].Facts["diagnosed_at"].(string); at != ruleNow.Add(-4*time.Minute).UTC().Format(time.RFC3339) {
		t.Errorf("diagnosed_at = %v", got["graph"].Facts["diagnosed_at"])
	}

	// Nothing found is said in words, with what was looked at — and that a
	// read failed.
	clean := strings.Join(factStrings(t, got["clean"], "likely_cause"), " ")
	if !strings.Contains(clean, "found none of the usual device-side causes") || !strings.Contains(clean, "1 of its 10 reads failed") {
		t.Errorf("clean cause = %q", clean)
	}
	// A pass from hours ago says nothing about this episode; and before any
	// pass there is no cause to show.
	for _, id := range []string{"stale", "none"} {
		if _, ok := got[id].Facts["likely_cause"]; ok || got[id].Summary == "" {
			t.Errorf("%s: %+v", id, got[id].Facts)
		}
	}
}

// The cause belongs to the alert it answered. An alert that stays open for
// hours keeps it: no second pass is started for an alert that has its answer,
// and the payload is rewritten at every evaluation.
func TestOpenAlertKeepsItsLikelyCause(t *testing.T) {
	db := testDB(t)
	graphing := `[{"kind":"graphing_enabled","severity":"critical","title":"Graphing is enabled on a weak device","detail":"interface rule interface=all"}]`
	open := func(id string, at time.Time) {
		t.Helper()
		if _, err := queries.InsertAlert(db, &queries.Alert{Rule: RuleCPUHigh, Key: id, DeviceID: id, Severity: SeverityWarning,
			FirstAt: at, LastAt: at, Confirmed: true, Payload: `{"summary":"busy"}`}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"long", "earlier"} {
		addDevice(t, db, id, devclass.C1, 64<<20)
		cpuMinutes(t, db, id, repeat(90, 5)...)
	}
	// "long": pegged for five hours; the pass ran in the first minutes.
	opened := ruleNow.Add(-5 * time.Hour)
	open("long", opened)
	diagnoseRun(t, db, "long", opened.Add(6*time.Minute), queries.AuditComplete, 11, 0, graphing)
	// "earlier": a pass of the same age, from an episode that is over; its
	// alert of today opened ten minutes ago and has no answer yet.
	diagnoseRun(t, db, "earlier", opened.Add(6*time.Minute), queries.AuditComplete, 11, 0, graphing)
	open("earlier", ruleNow.Add(-10*time.Minute))

	obs, err := cpuHigh{db}.Evaluate(ruleNow)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Observation{}
	for _, o := range firing(obs) {
		got[o.Key] = o
	}
	cause := factStrings(t, got["long"], "likely_cause")
	if len(cause) != 1 || !strings.HasPrefix(cause[0], "[critical] Graphing is enabled on a weak device") {
		t.Errorf("an alert open for five hours lost its likely cause: %q", cause)
	}
	if at, _ := got["long"].Facts["diagnosed_at"].(string); at != opened.Add(6*time.Minute).UTC().Format(time.RFC3339) {
		t.Errorf("diagnosed_at = %v, want the time of the pass that answered the alert", got["long"].Facts["diagnosed_at"])
	}
	if _, ok := got["earlier"].Facts["likely_cause"]; ok || got["earlier"].Summary == "" {
		t.Errorf("a pass from before the alert opened, hours ago, was passed off as its cause: %+v", got["earlier"].Facts)
	}
}

// When the cause arrives — minutes after the alert was announced — the
// alert is announced once more, with it. Never again for the same pass, and
// not through an acknowledgement.
func TestLikelyCauseIsAnnouncedOnce(t *testing.T) {
	rule := &fakeRule{name: RuleCPUHigh, obs: []Observation{{Key: "d1", DeviceID: "d1", Severity: SeverityWarning, Summary: "busy"}}}
	e, d, now := newTestEngine(t, rule)
	mustTick(t, e)
	if got := d.take(); !sameSet(got, []string{"firing cpu_high/d1"}) {
		t.Fatalf("first: %v", got)
	}

	withCause := func(at string) []Observation {
		return []Observation{{Key: "d1", DeviceID: "d1", Severity: SeverityWarning, Summary: "busy",
			Facts: map[string]any{"likely_cause": []string{"[critical] Graphing"}, "diagnosed_at": at}}}
	}
	rule.obs = withCause("2026-11-20T12:06:00Z")
	*now = now.Add(6 * time.Minute)
	mustTick(t, e)
	if got := d.take(); !sameSet(got, []string{"firing cpu_high/d1"}) {
		t.Fatalf("the cause arrived: sent %v, want the alert again", got)
	}
	for i := 0; i < 3; i++ {
		*now = now.Add(30 * time.Second)
		mustTick(t, e)
	}
	if got := d.take(); len(got) != 0 {
		t.Fatalf("the same cause was announced again: %v", got)
	}

	// Acknowledged: a later pass's cause shows on the page, nobody is paged.
	if err := queries.AckAlert(e.db, openAlerts(t, e)[0].ID, "operator", *now); err != nil {
		t.Fatal(err)
	}
	rule.obs = withCause("2026-11-20T13:10:00Z")
	*now = now.Add(time.Hour)
	mustTick(t, e)
	if got := d.take(); len(got) != 0 {
		t.Fatalf("an acknowledged alert was announced again: %v", got)
	}
	open := openAlerts(t, e)
	if open[0].State != queries.AlertAcked || ParsePayload(open[0].Payload).Facts["diagnosed_at"] != "2026-11-20T13:10:00Z" {
		t.Fatalf("after the second cause: %+v", open[0])
	}

	// An alert that already carries its cause when it opens is announced once.
	rule2 := &fakeRule{name: RuleCPUHigh, obs: withCause("2026-11-20T12:06:00Z")}
	e2, d2, _ := newTestEngine(t, rule2)
	mustTick(t, e2)
	mustTick(t, e2)
	if got := d2.take(); !sameSet(got, []string{"firing cpu_high/d1"}) {
		t.Fatalf("opened with a cause: %v", got)
	}
}

func TestNotificationCarriesTheLikelyCauseAndTheFix(t *testing.T) {
	db := testDB(t)
	n := NewNotifier(db, mailer.Config{SMTPHost: "smtp.example.com", SMTPPort: 587})
	var text, htmlBody string
	n.sendMail = func(_ mailer.Config, _, _, body, h string) error {
		text, htmlBody = body, h
		return nil
	}
	payload, _ := json.Marshal(Payload{Summary: "sw: CPU at or above 80 % for 5 minutes", Facts: map[string]any{
		"likely_cause": []string{"[critical] Graphing is enabled on a weak device — interface rule interface=all <x>"},
		"findings":     []string{"Graphing is enabled on a weak device — interface rule interface=all"},
		"fix":          []string{"Graphing is enabled on a weak device", "  Remove the graphing rules:", `    /tool graphing interface remove [find interface="all"]`},
	}})
	alert := queries.Alert{ID: 1, Rule: RuleCPUHigh, Key: "sw", Severity: SeverityWarning, State: queries.AlertFiring,
		FirstAt: time.Now(), Payload: string(payload)}
	if err := n.DeliverTo(queries.NotificationChannel{Kind: ChannelSMTP, Target: "ops@example.com"}, alert, EventFiring); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Likely cause:", "[critical] Graphing is enabled", "Suggested fix:", `[find interface="all"]`, "Findings:"} {
		if !strings.Contains(text, want) {
			t.Errorf("mail text lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(htmlBody, "<strong>Likely cause</strong>") || !strings.Contains(htmlBody, "interface=all &lt;x&gt;") {
		t.Errorf("mail HTML lacks the (escaped) likely cause:\n%s", htmlBody)
	}
}

// ---- flow_export_cpu_high (G6) ----------------------------------------------------------

// watchMinutes writes one device-minute per value starting at from; a
// negative value is a minute without a CPU sample.
func watchMinutes(t *testing.T, db *sql.DB, id string, from time.Time, values ...int64) {
	t.Helper()
	var rows []queries.DeviceMinute
	for i, v := range values {
		m := queries.DeviceMinute{DeviceID: id, Bucket: from.Add(time.Duration(i) * time.Minute), PolledS: 60}
		if v >= 0 {
			m.CPUSum, m.CPUSamples, m.CPUMax = v, 1, v
		}
		rows = append(rows, m)
	}
	if err := queries.UpsertDeviceMinutes(db, rows); err != nil {
		t.Fatal(err)
	}
}

func startWatch(t *testing.T, db *sql.DB, id string, accepted time.Time, baseline *float64) {
	t.Helper()
	if err := queries.StartFlowExportWatch(db, &queries.FlowExportWatch{DeviceID: id, ExporterID: 1, Address: "192.0.2.50",
		CollectorPort: 2055, AcceptedAt: accepted, Baseline: baseline}); err != nil {
		t.Fatal(err)
	}
}

func TestFlowExportCPUHighRule(t *testing.T) {
	accepted := time.Date(2026, 11, 20, 11, 40, 0, 0, time.UTC) // 20 min before ruleNow
	base := 12.0
	evaluate := func(t *testing.T, db *sql.DB, now time.Time) map[string]Observation {
		t.Helper()
		obs, err := flowExportCPUHigh{db}.Evaluate(now)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Observation{}
		for _, o := range obs {
			got[o.Key] = o
		}
		return got
	}

	t.Run("three times at or above 80 %", func(t *testing.T) {
		db := testDB(t)
		addDevice(t, db, "r", devclass.C3, 1<<30)
		busy := 60.0 // a device that is busy anyway: only the absolute line can trip
		startWatch(t, db, "r", accepted, &busy)
		// Two highs are not three …
		watchMinutes(t, db, "r", accepted, 20, 85, 25, 81, 30, 30)
		if got := evaluate(t, db, accepted.Add(8*time.Minute)); len(got) != 0 {
			t.Fatalf("two minutes at 80 %%: %+v", got)
		}
		// … the third is.
		watchMinutes(t, db, "r", accepted.Add(6*time.Minute), 80, 20)
		o := evaluate(t, db, accepted.Add(10*time.Minute))["r"]
		if o.Severity != SeverityWarning || o.DeviceID != "r" || o.Hold || !strings.Contains(o.Summary, "3 times") ||
			!strings.Contains(o.Summary, "sw-r") {
			t.Fatalf("observation = %+v", o)
		}
		if rb := strings.Join(factStrings(t, o, "rollback"), "\n"); !strings.Contains(rb, "/ip traffic-flow target disable") {
			t.Errorf("the alert does not carry the rollback:\n%s", rb)
		}
		// It keeps firing for the rest of the watch, calm minutes or not.
		watchMinutes(t, db, "r", accepted.Add(8*time.Minute), repeat(15, 10)...)
		if got := evaluate(t, db, accepted.Add(20*time.Minute)); got["r"].Severity != SeverityWarning {
			t.Fatalf("mid-watch: %+v", got)
		}
	})

	t.Run("30 points above the baseline", func(t *testing.T) {
		db := testDB(t)
		addDevice(t, db, "r", devclass.C3, 1<<30)
		addDevice(t, db, "nobase", devclass.C3, 1<<30)
		startWatch(t, db, "r", accepted, &base)
		startWatch(t, db, "nobase", accepted, nil)
		for _, id := range []string{"r", "nobase"} {
			watchMinutes(t, db, id, accepted, 20, 41, 43, 25) // baseline 12: 42 is the line
		}
		got := evaluate(t, db, accepted.Add(6*time.Minute))
		if o := got["r"]; o.Severity != SeverityWarning || !strings.Contains(o.Summary, "31 points above") {
			t.Fatalf("rise: %+v", o)
		}
		// Without a baseline only the absolute criterion counts.
		if _, ok := got["nobase"]; ok {
			t.Fatalf("no baseline: %+v", got["nobase"])
		}
	})

	t.Run("only the 30 minutes after the accept count", func(t *testing.T) {
		db := testDB(t)
		addDevice(t, db, "before", devclass.C3, 1<<30)
		addDevice(t, db, "after", devclass.C3, 1<<30)
		startWatch(t, db, "before", accepted, nil)
		watchMinutes(t, db, "before", accepted.Add(-5*time.Minute), 90, 90, 90, 90, 90, 20, 20) // high before, calm since
		old := accepted.Add(-2 * time.Hour)
		startWatch(t, db, "after", old, nil)
		watchMinutes(t, db, "after", old, repeat(20, 30)...)                  // a calm watch …
		watchMinutes(t, db, "after", old.Add(30*time.Minute), 95, 95, 95, 95) // … and a busy hour after it
		watchMinutes(t, db, "after", ruleNow.Add(-8*time.Minute), repeat(95, 5)...)
		if got := evaluate(t, db, ruleNow); len(got) != 0 {
			t.Fatalf("observations = %+v, want none", got)
		}
	})

	t.Run("after the watch it stands while the CPU stays up", func(t *testing.T) {
		db := testDB(t)
		addDevice(t, db, "r", devclass.C3, 1<<30)
		start := ruleNow.Truncate(time.Minute).Add(-50 * time.Minute)
		startWatch(t, db, "r", start, &base)
		watchMinutes(t, db, "r", start, repeat(88, 30)...) // the whole watch
		// Still high in the newest minutes: firing.
		watchMinutes(t, db, "r", start.Add(30*time.Minute), repeat(86, 17)...)
		if got := evaluate(t, db, ruleNow); got["r"].Severity != SeverityWarning || got["r"].Hold {
			t.Fatalf("still high: %+v", got)
		}
		// Five calm minutes at the end: it resolves.
		watchMinutes(t, db, "r", start.Add(42*time.Minute), repeat(14, 5)...)
		if got := evaluate(t, db, ruleNow); len(got) != 0 {
			t.Fatalf("calm again: %+v", got)
		}
		// None of the newest minutes was measured: cannot tell.
		watchMinutes(t, db, "r", start.Add(42*time.Minute), repeat(-1, 5)...)
		if got := evaluate(t, db, ruleNow); !got["r"].Hold {
			t.Fatalf("no sample in the newest minutes: %+v", got)
		}
		// The device is no longer recorded at all: cannot tell either.
		if got := evaluate(t, db, ruleNow.Add(2*time.Hour)); !got["r"].Hold {
			t.Fatalf("stale: %+v", got)
		}
	})
}

func TestAuditRulesAreRegistered(t *testing.T) {
	names := []string{}
	for _, r := range DefaultRules(testDB(t)) {
		names = append(names, r.Name())
	}
	for _, want := range []string{RuleAuditCritical, RuleFlowExportCPUHigh} {
		if !slices.Contains(RuleNames(), want) || !slices.Contains(names, want) {
			t.Errorf("%s: RuleNames = %v, DefaultRules = %v", want, RuleNames(), names)
		}
	}
	if RuleAuditCritical != "audit_critical" || RuleFlowExportCPUHigh != "flow_export_cpu_high" {
		t.Fatal("rule names are part of the API and the runbook anchors")
	}
}
