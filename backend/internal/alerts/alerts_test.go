package alerts

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/crypto"
	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/mailer"
)

func TestMain(m *testing.M) {
	// Channel targets are stored like device secrets; the suite has no key.
	queries.AllowPlaintextSecrets(true)
	os.Exit(m.Run())
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.OpenFresh(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeRule reports whatever the test sets.
type fakeRule struct {
	name string
	obs  []Observation
	err  error
}

func (r *fakeRule) Name() string                              { return r.name }
func (r *fakeRule) Evaluate(time.Time) ([]Observation, error) { return r.obs, r.err }

// fakeDeliverer records deliveries; delivered is what Deliver reports.
type fakeDeliverer struct {
	sent      []string // "event rule/key"
	delivered int
}

func (d *fakeDeliverer) Deliver(a queries.Alert, event Event) (int, error) {
	d.sent = append(d.sent, string(event)+" "+a.Rule+"/"+a.Key)
	return d.delivered, nil
}

func (d *fakeDeliverer) take() []string {
	out := d.sent
	d.sent = nil
	return out
}

func newTestEngine(t *testing.T, rules ...Rule) (*Engine, *fakeDeliverer, *time.Time) {
	t.Helper()
	d := &fakeDeliverer{delivered: 1}
	e := NewEngine(testDB(t), d, rules...)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	return e, d, &now
}

func openAlerts(t *testing.T, e *Engine) []queries.Alert {
	t.Helper()
	open, err := queries.ListOpenAlerts(e.db)
	if err != nil {
		t.Fatal(err)
	}
	return open
}

func mustTick(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w]--; seen[w] < 0 {
			return false
		}
	}
	return true
}

// One condition = one open alert, notified once, again after four hours
// while nobody acknowledged it, never after an ack, and once more when it
// resolves.
func TestAlertLifecycle(t *testing.T) {
	rule := &fakeRule{name: "r", obs: []Observation{{Key: "d1", DeviceID: "d1", Severity: SeverityCritical, Summary: "d1 is down"}}}
	e, d, now := newTestEngine(t, rule)
	changes := 0
	e.OnChange = func() { changes++ }

	mustTick(t, e)
	open := openAlerts(t, e)
	if len(open) != 1 || open[0].State != queries.AlertFiring || open[0].Severity != SeverityCritical ||
		ParsePayload(open[0].Payload).Summary != "d1 is down" || open[0].LastNotifiedAt == nil {
		t.Fatalf("after the first tick: %+v", open)
	}
	if got := d.take(); !sameSet(got, []string{"firing r/d1"}) || changes != 1 {
		t.Fatalf("first tick sent %v (changes %d), want one firing notification", got, changes)
	}

	// The condition holds: the same alert, no new notification.
	*now = now.Add(30 * time.Second)
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 1 || !open[0].LastAt.Equal(*now) || !open[0].FirstAt.Equal(now.Add(-30*time.Second)) {
		t.Fatalf("the alert was not carried on: %+v", open)
	}
	if got := d.take(); len(got) != 0 || changes != 1 {
		t.Fatalf("a held condition notified again: %v (changes %d)", got, changes)
	}

	// Four hours on, still firing and unacknowledged: once more.
	*now = now.Add(RenotifyEvery)
	mustTick(t, e)
	if got := d.take(); !sameSet(got, []string{"firing r/d1"}) {
		t.Fatalf("after 4 h: %v, want a re-notification", got)
	}

	// Acknowledged: open, but quiet.
	if err := queries.AckAlert(e.db, open[0].ID, "kim", *now); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * RenotifyEvery)
	mustTick(t, e)
	if got := d.take(); len(got) != 0 {
		t.Fatalf("an acknowledged alert was re-notified: %v", got)
	}
	if open := openAlerts(t, e); len(open) != 1 || open[0].State != queries.AlertAcked || open[0].AckedBy != "kim" {
		t.Fatalf("after the ack: %+v", open)
	}

	// The condition is gone: resolved, and the resolution is announced.
	rule.obs = nil
	*now = now.Add(time.Minute)
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 0 {
		t.Fatalf("still open after the condition went: %+v", open)
	}
	if got := d.take(); !sameSet(got, []string{"resolved r/d1"}) {
		t.Fatalf("resolution sent %v", got)
	}
	recent, err := queries.ListRecentAlerts(e.db, now.Add(-time.Hour), 10)
	if err != nil || len(recent) != 1 || recent[0].State != queries.AlertResolved || recent[0].ResolvedAt == nil {
		t.Fatalf("recent = %+v (%v)", recent, err)
	}

	// It fires again later: a new alert, not the old row.
	rule.obs = []Observation{{Key: "d1", Severity: SeverityWarning, Summary: "d1 again"}}
	*now = now.Add(time.Hour)
	mustTick(t, e)
	if open := openAlerts(t, e); len(open) != 1 || open[0].ID == recent[0].ID || open[0].Severity != SeverityWarning {
		t.Fatalf("second firing: %+v", open)
	}
}

// A rule that cannot be evaluated says nothing: its alerts stay open, and
// the other rules are still evaluated.
func TestRuleErrorNeverResolves(t *testing.T) {
	flaky := &fakeRule{name: "flaky", obs: []Observation{{Key: "k", Severity: SeverityWarning, Summary: "s"}}}
	steady := &fakeRule{name: "steady"}
	e, d, now := newTestEngine(t, flaky, steady)
	mustTick(t, e)
	d.take()

	flaky.obs, flaky.err = nil, errors.New("database is locked")
	steady.obs = []Observation{{Key: "x", Severity: SeverityInfo, Summary: "x"}}
	*now = now.Add(30 * time.Second)
	if err := e.Tick(); err == nil {
		t.Fatal("Tick must report the rule's error")
	}
	open := openAlerts(t, e)
	if len(open) != 2 {
		t.Fatalf("open after a failed evaluation = %+v, want the flaky alert kept and the steady one opened", open)
	}
	if got := d.take(); !sameSet(got, []string{"firing steady/x"}) {
		t.Fatalf("sent %v", got)
	}

	// An alert of a rule this build does not have is left alone too.
	if _, err := queries.InsertAlert(e.db, &queries.Alert{Rule: "from_a_newer_version", Key: "k", Severity: SeverityWarning, FirstAt: *now, LastAt: *now, Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	flaky.err = nil
	mustTick(t, e)
	rules := map[string]bool{}
	for _, a := range openAlerts(t, e) {
		rules[a.Rule] = true
	}
	if !rules["from_a_newer_version"] || rules["flaky"] {
		t.Fatalf("open rules = %v, want the unknown rule kept and flaky resolved", rules)
	}
}

// A silence stops the notification, not the alert; when it ends a still
// firing alert is announced, and an alert that came and went under a silence
// is never mentioned.
func TestSilences(t *testing.T) {
	rule := &fakeRule{name: RuleDeviceUnreachable, obs: []Observation{
		{Key: "d1", DeviceID: "d1", Severity: SeverityCritical, Summary: "d1"},
		{Key: "d2", DeviceID: "d2", Severity: SeverityCritical, Summary: "d2"},
	}}
	e, d, now := newTestEngine(t, rule)
	if _, err := queries.InsertAlertSilence(e.db, &queries.AlertSilence{Scope: "device", Target: "d1", Until: now.Add(time.Hour), CreatedBy: "kim", CreatedAt: *now}); err != nil {
		t.Fatal(err)
	}
	mustTick(t, e)
	if got := d.take(); !sameSet(got, []string{"firing " + RuleDeviceUnreachable + "/d2"}) {
		t.Fatalf("with d1 silenced: sent %v", got)
	}
	if open := openAlerts(t, e); len(open) != 2 {
		t.Fatalf("a silence must not stop the alert itself: %+v", open)
	}

	// The silence ends, d1 still fires: now it is announced.
	*now = now.Add(time.Hour + time.Minute)
	mustTick(t, e)
	if got := d.take(); !sameSet(got, []string{"firing " + RuleDeviceUnreachable + "/d1"}) {
		t.Fatalf("after the silence ended: sent %v", got)
	}

	// A rule-wide silence: d1 resolves under it — nobody is told.
	if _, err := queries.InsertAlertSilence(e.db, &queries.AlertSilence{Scope: "rule", Target: RuleDeviceUnreachable, Until: now.Add(time.Hour), CreatedBy: "kim", CreatedAt: *now}); err != nil {
		t.Fatal(err)
	}
	rule.obs = rule.obs[1:]
	*now = now.Add(time.Minute)
	mustTick(t, e)
	if got := d.take(); len(got) != 0 {
		t.Fatalf("under a rule silence: sent %v", got)
	}

	if !Silenced(queries.Alert{Rule: "x"}, []queries.AlertSilence{{Scope: "site"}}) {
		t.Error("a site silence covers every alert")
	}
	if Silenced(queries.Alert{Rule: "x", DeviceID: ""}, []queries.AlertSilence{{Scope: "device", Target: ""}}) {
		t.Error("a device silence must not match an alert without a device")
	}
}

// Nothing delivered (no channel yet, or every channel failing) is not
// "notified": the next tick tries again, and a resolution nobody heard the
// start of is not announced.
func TestUndeliveredAlertsAreRetried(t *testing.T) {
	rule := &fakeRule{name: "r", obs: []Observation{{Key: "k", Severity: SeverityWarning, Summary: "s"}}}
	e, d, now := newTestEngine(t, rule)
	d.delivered = 0
	mustTick(t, e)
	*now = now.Add(30 * time.Second)
	mustTick(t, e)
	if got := d.take(); !sameSet(got, []string{"firing r/k", "firing r/k"}) {
		t.Fatalf("with nothing delivered: %v, want a retry each tick", got)
	}
	if open := openAlerts(t, e); open[0].LastNotifiedAt != nil {
		t.Fatal("an undelivered alert was marked notified")
	}
	rule.obs = nil
	mustTick(t, e)
	if got := d.take(); len(got) != 0 {
		t.Fatalf("resolution of a never-delivered alert was sent: %v", got)
	}
}

func TestDeviceUnreachableRule(t *testing.T) {
	db := testDB(t)
	seen := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	for _, d := range []queries.Device{
		{ID: "up", Identity: "sw-up", Address: "192.0.2.1", Status: "online"},
		{ID: "grace", Identity: "sw-grace", Address: "192.0.2.2", Status: "unknown"},
		{ID: "down", Identity: "sw-down", Address: "192.0.2.3", Status: "offline"},
	} {
		d.Username, d.APIPort, d.Tags = "u", 8728, "[]"
		if err := queries.CreateDevice(db, &d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE devices SET last_seen = ? WHERE id = 'down'`, seen); err != nil {
		t.Fatal(err)
	}
	obs, err := deviceUnreachable{db}.Evaluate(seen.Add(time.Hour))
	if err != nil || len(obs) != 1 {
		t.Fatalf("observations = %+v (%v), want only the offline device", obs, err)
	}
	o := obs[0]
	if o.Key != "down" || o.DeviceID != "down" || o.Severity != SeverityCritical ||
		!strings.Contains(o.Summary, "sw-down (192.0.2.3) is not reachable") || !strings.Contains(o.Summary, "2026-11-20 11:00 UTC") {
		t.Fatalf("observation = %+v", o)
	}
}

func TestParallelAPIPollerRule(t *testing.T) {
	db := testDB(t)
	if err := queries.CreateDevice(db, &queries.Device{ID: "d1", Identity: "rb", Address: "192.0.2.1", Username: "u", APIPort: 8728, Status: "online", Tags: "[]"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, f := range []queries.DeviceFinding{
		{DeviceID: "d1", Kind: RuleParallelAPIPoller, Severity: "warn", Detail: "user other from 192.0.2.50 via api"},
		{DeviceID: "d1", Kind: RuleParallelAPIPoller, Severity: "info", Detail: "user gone from 192.0.2.51 via api"},
		{DeviceID: "d1", Kind: RuleParallelAPIPoller, Severity: "warn", Detail: "user old from 192.0.2.52 via api"},
		{DeviceID: "d1", Kind: "login_failure", Severity: "warn", Detail: "user admin from 192.0.2.70 via ssh"},
	} {
		if err := queries.UpsertDeviceFinding(db, &f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE device_findings SET last_seen = ? WHERE detail LIKE 'user old%'`, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	obs, err := parallelAPIPoller{db}.Evaluate(now.Add(time.Minute))
	if err != nil || len(obs) != 1 {
		t.Fatalf("observations = %+v (%v), want only the confirmed, current poller", obs, err)
	}
	if o := obs[0]; o.DeviceID != "d1" || o.Severity != SeverityWarning || !strings.Contains(o.Summary, "rb: API login") ||
		!strings.Contains(o.Summary, "192.0.2.50") || o.Key != "d1|user other from 192.0.2.50 via api" {
		t.Fatalf("observation = %+v", o)
	}
}

func TestTargetValidationAndHint(t *testing.T) {
	for _, ok := range [][2]string{{ChannelSMTP, "ops@example.com"}, {ChannelWebhook, "https://hooks.example.com/services/T0/B0/secret"}, {ChannelWebhook, "http://ntfy.lan:8080/alerts"}} {
		if err := ValidateTarget(ok[0], ok[1]); err != nil {
			t.Errorf("ValidateTarget(%s, %s) = %v", ok[0], ok[1], err)
		}
	}
	for _, bad := range [][2]string{{ChannelSMTP, "not-an-address"}, {ChannelSMTP, ""}, {ChannelWebhook, "ftp://x/y"}, {ChannelWebhook, "hooks.example.com/x"}, {"pager", "x"}} {
		if err := ValidateTarget(bad[0], bad[1]); err == nil {
			t.Errorf("ValidateTarget(%s, %q) accepted", bad[0], bad[1])
		}
	}
	if h := TargetHint(ChannelWebhook, "https://hooks.example.com/services/T0/B0/secret"); h != "https://hooks.example.com/…" || strings.Contains(h, "secret") {
		t.Errorf("webhook hint = %q", h)
	}
	if h := TargetHint(ChannelSMTP, "ops@example.com"); h != "o…@example.com" {
		t.Errorf("smtp hint = %q", h)
	}
	if h := TargetHint(ChannelWebhook, crypto.Unreadable); strings.Contains(h, "\x00") || h == "" {
		t.Errorf("unreadable hint = %q", h)
	}
}

// Delivery goes to every enabled channel the alert's severity reaches; a
// failing channel does not stop the others; errors never contain the webhook
// URL.
func TestNotifierDelivery(t *testing.T) {
	db := testDB(t)
	var got []webhookBody
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("webhook request: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		raw, _ := io.ReadAll(r.Body)
		var b webhookBody
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Errorf("webhook body %s: %v", raw, err)
		}
		got = append(got, b)
	}))
	defer hook.Close()
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer failing.Close()

	add := func(c queries.NotificationChannel) {
		t.Helper()
		if _, err := queries.InsertNotificationChannel(db, &c); err != nil {
			t.Fatal(err)
		}
	}
	add(queries.NotificationChannel{Kind: ChannelWebhook, Target: hook.URL + "/secret-path", MinSeverity: SeverityWarning, Enabled: true})
	add(queries.NotificationChannel{Kind: ChannelWebhook, Target: failing.URL + "/very-secret-token", MinSeverity: SeverityInfo, Enabled: true})
	add(queries.NotificationChannel{Kind: ChannelSMTP, Target: "ops@example.com", MinSeverity: SeverityCritical, Enabled: true})
	add(queries.NotificationChannel{Kind: ChannelSMTP, Target: "off@example.com", MinSeverity: SeverityInfo, Enabled: false})

	n := NewNotifier(db, mailer.Config{SMTPHost: "smtp.example.com", SMTPPort: 587, PublicBaseURL: "https://nms.example.com/"})
	var mails []string
	n.sendMail = func(cfg mailer.Config, to, subject, text, htmlBody string) error {
		if cfg.SMTPHost != "smtp.example.com" {
			t.Errorf("mail config = %+v", cfg)
		}
		mails = append(mails, to+" | "+subject)
		if !strings.Contains(text, "Runbook:") || !strings.Contains(text, "https://nms.example.com/alerts") || !strings.Contains(htmlBody, "<a href=") {
			t.Errorf("mail body lacks the runbook or the link:\n%s\n%s", text, htmlBody)
		}
		return nil
	}
	payload, _ := json.Marshal(Payload{Summary: "sw01 (192.0.2.1) is not reachable", Facts: map[string]any{"device": "sw01"}})
	alert := queries.Alert{ID: 7, Rule: RuleDeviceUnreachable, Key: "d1", DeviceID: "d1", Severity: SeverityWarning,
		State: queries.AlertFiring, FirstAt: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), Payload: string(payload)}

	// A warning: the first webhook takes it, the failing one fails, the
	// critical-only mail channel and the disabled one are skipped.
	delivered, err := n.Deliver(alert, EventFiring)
	if delivered != 1 || err == nil {
		t.Fatalf("Deliver = %d, %v; want 1 delivery and the failing channel's error", delivered, err)
	}
	if strings.Contains(err.Error(), "very-secret-token") || strings.Contains(err.Error(), failing.URL) {
		t.Fatalf("the error leaks the webhook URL: %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("error = %v, want the HTTP status", err)
	}
	if len(got) != 1 || len(mails) != 0 {
		t.Fatalf("webhooks = %d mails = %v", len(got), mails)
	}
	b := got[0]
	if b.Title != "[NMS] WARNING: sw01 (192.0.2.1) is not reachable" || b.Text != b.Title || b.Message == "" || b.Priority != 5 ||
		b.Alert.Rule != RuleDeviceUnreachable || b.Alert.Event != EventFiring || b.Alert.RunbookURL != RunbookURL(RuleDeviceUnreachable) ||
		b.Alert.URL != "https://nms.example.com/alerts" || b.Alert.Facts["device"] != "sw01" {
		t.Fatalf("webhook body = %+v", b)
	}

	// Critical reaches the mail channel too; a resolution is labelled so.
	alert.Severity = SeverityCritical
	resolvedAt := alert.FirstAt.Add(time.Hour)
	alert.State, alert.ResolvedAt = queries.AlertResolved, &resolvedAt
	delivered, _ = n.Deliver(alert, EventResolved)
	if delivered != 2 || len(mails) != 1 || !strings.HasPrefix(mails[0], "ops@example.com | [NMS] RESOLVED:") {
		t.Fatalf("critical resolution: delivered %d, mails %v", delivered, mails)
	}

	// No relay configured: the mail channel says so instead of dialling.
	bare := NewNotifier(db, mailer.Config{})
	ch := queries.NotificationChannel{Kind: ChannelSMTP, Target: "ops@example.com"}
	if err := bare.DeliverTo(ch, TestAlert(time.Now()), EventTest); err == nil || !strings.Contains(err.Error(), "SMTP") {
		t.Fatalf("mail without a relay = %v", err)
	}
	// An undecryptable target is refused, not used.
	if err := n.DeliverTo(queries.NotificationChannel{Kind: ChannelWebhook, Target: crypto.Unreadable}, TestAlert(time.Now()), EventTest); err == nil {
		t.Fatal("an unreadable target was used")
	}
	// A webhook that cannot be reached: the error names the host only.
	gone := queries.NotificationChannel{Kind: ChannelWebhook, Target: "http://127.0.0.1:1/hook-secret"}
	if err := n.DeliverTo(gone, TestAlert(time.Now()), EventTest); err == nil || strings.Contains(err.Error(), "hook-secret") {
		t.Fatalf("unreachable webhook error = %v", err)
	}
}
