package alerts

import (
	"database/sql"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/mailer"
)

// exportDevice adds a switch whose address sent flow datagrams: the sighting
// the collector records, and the finding it raises.
func exportDevice(t *testing.T, db *sql.DB, id, addr, version string, at time.Time) {
	t.Helper()
	if err := queries.CreateDevice(db, &queries.Device{ID: id, Identity: id, Address: addr, Username: "u", APIPort: 8728,
		Status: "online", Tags: "[]"}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpdateDeviceInfo(db, id, "MikroTik", "CRS326-24G-2S+", version, "", "arm"); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetDeviceClass(db, id, devclass.C2, devclass.ResourceFacts{}, at); err != nil {
		t.Fatal(err)
	}
	if err := queries.NoteFlowExportSighting(db, id, addr, 2055, at); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertFlowExportFinding(db, id, RuleFlowExportOnSwitch, "flow datagrams from "+addr, at); err != nil {
		t.Fatal(err)
	}
}

func factStrings(t *testing.T, o Observation, key string) []string {
	t.Helper()
	raw, err := json.Marshal(o.Facts[key])
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

// A datagram from a switch's address is a warning marked unconfirmed. Only
// a read of the device that says it exports makes it critical, and that
// alert carries the rollback text.
func TestFlowExportOnSwitchRule(t *testing.T) {
	db := testDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	exportDevice(t, db, "sw", "192.0.2.10", "7.23.1 (stable)", now)
	if err := queries.SetSetting(db, "flow_advertise_address", "198.51.100.5"); err != nil {
		t.Fatal(err)
	}
	rule := flowExportOnSwitch{db}

	obs, err := rule.Evaluate(now.Add(time.Minute))
	if err != nil || len(obs) != 1 {
		t.Fatalf("observations = %+v (%v)", obs, err)
	}
	o := obs[0]
	if o.Key != "sw" || o.DeviceID != "sw" || o.Severity != SeverityWarning || !o.Unconfirmed ||
		!strings.Contains(o.Summary, "sw (192.0.2.10)") || !strings.Contains(o.Summary, "not confirmed") {
		t.Fatalf("unconfirmed observation = %+v", o)
	}
	if o.Facts["confirmed"] != false || len(factStrings(t, o, "rollback")) != 0 {
		t.Fatalf("an unconfirmed alert carries a confirmation or a rollback: %+v", o.Facts)
	}
	if check := factStrings(t, o, "check"); !slices.Equal(check, []string{"/ip traffic-flow print", "/ip traffic-flow target print"}) {
		t.Fatalf("check commands = %q", check)
	}

	// The device says it does not export: still a warning, never critical.
	if err := queries.SetFlowExportCheckResult(db, "sw", now, queries.FlowExportNotExporting, "traffic-flow disabled; no targets", nil); err != nil {
		t.Fatal(err)
	}
	obs, _ = rule.Evaluate(now.Add(time.Minute))
	if len(obs) != 1 || obs[0].Severity != SeverityWarning || !obs[0].Unconfirmed || !strings.Contains(obs[0].Summary, "may be forged") {
		t.Fatalf("after a negative read: %+v", obs)
	}
	// A read that failed confirms nothing either.
	if err := queries.SetFlowExportCheckResult(db, "sw", now, queries.FlowExportCheckFailed, "the read failed: timeout", nil); err != nil {
		t.Fatal(err)
	}
	obs, _ = rule.Evaluate(now.Add(time.Minute))
	if len(obs) != 1 || obs[0].Severity != SeverityWarning || !obs[0].Unconfirmed {
		t.Fatalf("after a failed read: %+v", obs)
	}

	// Confirmed: critical, with the rollback for this device's version and
	// the one target it exports to.
	if err := queries.SetFlowExportCheckResult(db, "sw", now, queries.FlowExportExporting,
		"traffic-flow enabled (interfaces=all); targets: 198.51.100.5:2056 (ipfix)",
		[]netip.AddrPort{netip.MustParseAddrPort("198.51.100.5:2056")}); err != nil {
		t.Fatal(err)
	}
	obs, _ = rule.Evaluate(now.Add(time.Minute))
	if len(obs) != 1 {
		t.Fatalf("confirmed: %+v", obs)
	}
	o = obs[0]
	if o.Severity != SeverityCritical || o.Unconfirmed || o.Key != "sw" || !strings.Contains(o.Summary, "confirmed on the device") ||
		o.Facts["confirmed"] != true {
		t.Fatalf("confirmed observation = %+v", o)
	}
	rollback := strings.Join(factStrings(t, o, "rollback"), "\n")
	for _, want := range []string{"/tool profile duration=30",
		"/ip traffic-flow target disable [find dst-address=198.51.100.5 port=2056]",
		"/system package update download", "WARNING", "/ip traffic-flow set enabled=no",
		"/ip traffic-flow target remove [find dst-address=198.51.100.5 port=2056]"} {
		if !strings.Contains(rollback, want) {
			t.Errorf("rollback lacks %q:\n%s", want, rollback)
		}
	}
	if strings.Index(rollback, "target disable") > strings.Index(rollback, "enabled=no") {
		t.Errorf("rollback order:\n%s", rollback)
	}

	// On RouterOS 7.25 the same alert carries no kernel warning and no download.
	if err := queries.UpdateDeviceInfo(db, "sw", "MikroTik", "CRS326-24G-2S+", "7.25 (stable)", "", "arm"); err != nil {
		t.Fatal(err)
	}
	obs, _ = rule.Evaluate(now.Add(time.Minute))
	rollback = strings.Join(factStrings(t, obs[0], "rollback"), "\n")
	if strings.Contains(rollback, "WARNING") || strings.Contains(rollback, "package update download") ||
		!strings.Contains(rollback, "/ip traffic-flow set enabled=no") {
		t.Errorf("7.25 rollback:\n%s", rollback)
	}

	// A confirmation nobody renewed does not last: datagrams that keep
	// arriving (they may be forged) long after the device was last read are
	// a warning again, not a critical alert resting on an old "yes".
	if err := queries.SetFlowExportCheckResult(db, "sw", now.Add(-queries.FlowExportConfirmationValid-time.Hour),
		queries.FlowExportExporting, "traffic-flow enabled", nil); err != nil {
		t.Fatal(err)
	}
	obs, _ = rule.Evaluate(now.Add(time.Minute))
	if len(obs) != 1 || obs[0].Severity != SeverityWarning || !obs[0].Unconfirmed || !strings.Contains(obs[0].Summary, "too old") ||
		len(factStrings(t, obs[0], "rollback")) != 0 {
		t.Fatalf("with an expired confirmation: %+v", obs)
	}

	// No datagram for longer than the window: the condition is gone.
	if obs, _ := rule.Evaluate(now.Add(flowExportCurrent + 2*time.Minute)); len(obs) != 0 {
		t.Fatalf("a finding nobody refreshed still alerts: %+v", obs)
	}
	// A deleted device takes its alert with it.
	if err := queries.DeleteDevice(db, "sw"); err != nil {
		t.Fatal(err)
	}
	if obs, err := rule.Evaluate(now.Add(time.Minute)); err != nil || len(obs) != 0 {
		t.Fatalf("after the device was deleted: %+v %v", obs, err)
	}
}

// The engine stores the unconfirmed mark, and an alert that turns from an
// unconfirmed warning into a confirmed critical is notified again at once —
// even when the warning had been delivered or acknowledged.
func TestUnconfirmedAlertAndEscalation(t *testing.T) {
	rule := &fakeRule{name: "r", obs: []Observation{{Key: "d1", DeviceID: "d1", Severity: SeverityWarning,
		Summary: "maybe", Unconfirmed: true}}}
	e, d, now := newTestEngine(t, rule)
	mustTick(t, e)
	open := openAlerts(t, e)
	if len(open) != 1 || open[0].Confirmed || open[0].Severity != SeverityWarning {
		t.Fatalf("unconfirmed alert stored as %+v", open)
	}
	if got := d.take(); !sameSet(got, []string{"firing r/d1"}) {
		t.Fatalf("first tick: %v", got)
	}
	if err := queries.AckAlert(e.db, open[0].ID, "admin", *now); err != nil {
		t.Fatal(err)
	}

	// Still unconfirmed: nothing new.
	*now = now.Add(30 * time.Second)
	mustTick(t, e)
	if got := d.take(); len(got) != 0 {
		t.Fatalf("an unchanged, acknowledged warning notified again: %v", got)
	}

	// Confirmed: critical, firing again, notified in the same tick.
	rule.obs = []Observation{{Key: "d1", DeviceID: "d1", Severity: SeverityCritical, Summary: "confirmed"}}
	*now = now.Add(30 * time.Second)
	mustTick(t, e)
	open = openAlerts(t, e)
	if len(open) != 1 || !open[0].Confirmed || open[0].Severity != SeverityCritical || open[0].State != queries.AlertFiring ||
		open[0].AckedBy != "" {
		t.Fatalf("after the confirmation: %+v", open)
	}
	if got := d.take(); !sameSet(got, []string{"firing r/d1"}) {
		t.Fatalf("the escalation was not notified: %v", got)
	}

	// Critical and unchanged: quiet again until the usual re-notification.
	*now = now.Add(30 * time.Second)
	mustTick(t, e)
	if got := d.take(); len(got) != 0 {
		t.Fatalf("an unchanged critical alert notified again: %v", got)
	}
	// Back to a warning (the device stopped; datagrams still arrive): no
	// new notification — only a rising severity pages.
	rule.obs = []Observation{{Key: "d1", DeviceID: "d1", Severity: SeverityWarning, Summary: "maybe", Unconfirmed: true}}
	*now = now.Add(30 * time.Second)
	mustTick(t, e)
	open = openAlerts(t, e)
	if len(open) != 1 || open[0].Confirmed || open[0].Severity != SeverityWarning {
		t.Fatalf("after the downgrade: %+v", open)
	}
	if got := d.take(); len(got) != 0 {
		t.Fatalf("a downgrade notified: %v", got)
	}
}

// The mail of a confirmed alert carries the rollback steps, the webhook the
// facts.
func TestNotificationCarriesTheRollback(t *testing.T) {
	db := testDB(t)
	n := NewNotifier(db, mailer.Config{SMTPHost: "smtp.example.com", SMTPPort: 587})
	var text, htmlBody string
	n.sendMail = func(_ mailer.Config, _, _, body, h string) error {
		text, htmlBody = body, h
		return nil
	}
	payload, _ := json.Marshal(Payload{Summary: "sw is exporting flows", Facts: map[string]any{
		"rollback": []string{"0. Decide first.", "   /tool profile duration=30", "1. Disable <the> target."}}})
	alert := queries.Alert{ID: 1, Rule: RuleFlowExportOnSwitch, Key: "sw", Severity: SeverityCritical, State: queries.AlertFiring,
		FirstAt: time.Now(), Payload: string(payload)}
	if err := n.DeliverTo(queries.NotificationChannel{Kind: ChannelSMTP, Target: "ops@example.com"}, alert, EventFiring); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Rollback") || !strings.Contains(text, "   /tool profile duration=30") ||
		strings.Index(text, "0. Decide first.") > strings.Index(text, "1. Disable") {
		t.Errorf("mail text lacks the rollback steps:\n%s", text)
	}
	if !strings.Contains(htmlBody, "<pre>") || !strings.Contains(htmlBody, "Disable &lt;the&gt; target.") {
		t.Errorf("mail HTML lacks the (escaped) rollback:\n%s", htmlBody)
	}
	// A resolution does not repeat them.
	text = ""
	alert.State = queries.AlertResolved
	if err := n.DeliverTo(queries.NotificationChannel{Kind: ChannelSMTP, Target: "ops@example.com"}, alert, EventResolved); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "/tool profile") {
		t.Errorf("a resolution mail repeats the rollback:\n%s", text)
	}
}

func TestFlowExportRuleIsRegistered(t *testing.T) {
	if !slices.Contains(RuleNames(), RuleFlowExportOnSwitch) {
		t.Fatalf("RuleNames = %v", RuleNames())
	}
	names := []string{}
	for _, r := range DefaultRules(testDB(t)) {
		names = append(names, r.Name())
	}
	if !slices.Contains(names, RuleFlowExportOnSwitch) {
		t.Fatalf("DefaultRules = %v", names)
	}
}

// A confirmation belongs to the run of datagrams it was read during. After
// the export was stopped and the datagrams ended, a new datagram from the
// same address — which may be forged — starts a new run: the old "yes" does
// not make it critical. It stays an unconfirmed warning until the device is
// read again.
func TestOldConfirmationDoesNotConfirmANewRun(t *testing.T) {
	db := testDB(t)
	t0 := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	exportDevice(t, db, "sw", "192.0.2.10", "7.25.2 (stable)", t0)
	rule := flowExportOnSwitch{db}
	only := func(at time.Time) Observation {
		t.Helper()
		obs, err := rule.Evaluate(at)
		if err != nil {
			t.Fatal(err)
		}
		if len(obs) != 1 {
			t.Fatalf("observations at %s = %+v, want one", at.Format(time.TimeOnly), obs)
		}
		return obs[0]
	}
	sight := func(at time.Time) {
		t.Helper()
		if err := queries.NoteFlowExportSighting(db, "sw", "192.0.2.10", 2055, at); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpsertFlowExportFinding(db, "sw", RuleFlowExportOnSwitch, "flow datagrams from 192.0.2.10", at); err != nil {
			t.Fatal(err)
		}
	}

	// The device is read one minute into the run and says it exports.
	if err := queries.SetFlowExportCheckResult(db, "sw", t0.Add(time.Minute), queries.FlowExportExporting, "traffic-flow enabled", nil); err != nil {
		t.Fatal(err)
	}
	sight(t0.Add(2 * time.Minute))
	if o := only(t0.Add(3 * time.Minute)); o.Severity != SeverityCritical || o.Unconfirmed {
		t.Fatalf("during the confirmed run: %s unconfirmed=%v, want critical and confirmed", o.Severity, o.Unconfirmed)
	}

	// The export is stopped; the datagrams end. 45 minutes later one
	// datagram carries the switch's address again — inside the 2 h 15 min a
	// confirmation is valid, and inside the hour before the next read.
	later := t0.Add(50 * time.Minute)
	sight(later)
	o := only(later.Add(time.Minute))
	if o.Severity != SeverityWarning || !o.Unconfirmed {
		t.Errorf("a new run after the datagrams had ended: %s unconfirmed=%v — an old confirmation made it critical", o.Severity, o.Unconfirmed)
	}
	if lines := factStrings(t, o, "rollback"); len(lines) != 0 {
		t.Errorf("an unconfirmed alert carries rollback steps: %v", lines)
	}
	f, err := queries.ListDeviceFindings(db, "sw", 10)
	if err != nil || len(f) != 1 || f[0].Severity != "warn" {
		t.Errorf("finding after the new run = %+v %v, want one warn", f, err)
	}

	// A read during the new run that says "exporting" confirms it again.
	if err := queries.SetFlowExportCheckResult(db, "sw", later.Add(2*time.Minute), queries.FlowExportExporting, "traffic-flow enabled", nil); err != nil {
		t.Fatal(err)
	}
	sight(later.Add(3 * time.Minute))
	if o := only(later.Add(4 * time.Minute)); o.Severity != SeverityCritical || o.Unconfirmed {
		t.Errorf("after a read during the new run: %s unconfirmed=%v, want critical and confirmed", o.Severity, o.Unconfirmed)
	}

	// Datagrams that keep coming are one run: its confirmation holds.
	run := later.Add(4 * time.Minute)
	for i := 0; i < 4; i++ {
		run = run.Add(20 * time.Minute)
		sight(run)
	}
	if o := only(run.Add(time.Minute)); o.Severity != SeverityCritical {
		t.Errorf("80 minutes into an uninterrupted run: %s, want still critical", o.Severity)
	}
}
