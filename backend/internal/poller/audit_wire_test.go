package poller

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/alerts"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/hygiene"
	"github.com/mikrotik-nms/backend/internal/hygiene/hygienetest"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

// onTheWire puts a harness device behind a fake RouterOS endpoint on a real
// pooled session: the pass's reads then travel through routeros.RunCommand
// and the API protocol, and the fake records exactly what a device would
// receive. It answers with whole rows, whatever the proplist says.
func (h *auditHarness) onTheWire(id string, pool *routeros.Pool) *routerostest.Device {
	h.t.Helper()
	fake := routerostest.NewDevice(h.t)
	h.mu.Lock()
	p := h.devices[id]
	h.mu.Unlock()
	p.Script(fake)
	fake.Dial(h.t, pool, id)
	return fake
}

// wireCalls renders what the fake received as "command args…" lines.
func wireCalls(fake *routerostest.Device) []string {
	cmds, args := fake.Commands(), fake.Args()
	out := make([]string, len(cmds))
	for i := range cmds {
		out[i] = strings.TrimSpace(cmds[i] + " " + strings.Join(args[i], " "))
	}
	return out
}

// dump renders every row of the tables the audit writes as text, so a test
// can look for anything that must not be there.
func dumpAuditTables(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	for _, table := range []string{"device_audit", "device_audit_runs", "device_findings", "alerts", "audit_log"} {
		rows, err := db.Query(`SELECT * FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				if raw, ok := v.([]byte); ok {
					v = string(raw)
				}
				fmt.Fprintf(&b, "%s.%s=%v\n", table, cols[i], v)
			}
		}
		rows.Close()
	}
	return b.String()
}

// The device answers every read with the whole row — the SNMP community's
// name and passwords, the scheduler's script, the trap community. Not one of
// them may reach the database, in any table, in any column.
func TestAuditOverTheWireNeverStoresASecret(t *testing.T) {
	h := newAuditHarness(t)
	const secret = "DO-NOT-STORE"
	p := hygienetest.Load(t, "switch48_clean").
		With("/snmp/print", map[string]string{"enabled": "true", "trap-community": secret + "-trap", "contact": "noc"}).
		With("/snmp/community/print", map[string]string{"name": secret + "-community", "addresses": "::/0", "security": "none",
			"read-access": "true", "write-access": "true", "authentication-password": secret + "-auth", "encryption-password": secret + "-priv"}).
		With("/system/scheduler/print", map[string]string{"name": "push", "interval": "10s",
			"on-event": "/tool fetch url=https://198.51.100.9/x user=u password=" + secret + "-script"}).
		With("/user/active/print", map[string]string{"name": "nms", "address": hygienetest.NMSAddress, "via": "api", "group": "full",
			"password": secret + "-user"}).
		With("/ip/traffic-flow/print", map[string]string{"enabled": "true", "interfaces": "all"}).
		With("/ip/traffic-flow/target/print", map[string]string{"dst-address": "198.51.100.7", "port": "2055", "version": "ipfix"}).
		With("/system/logging/print", map[string]string{"topics": "debug", "action": "remote"}).
		With("/system/logging/action/print", map[string]string{"name": "remote", "target": "remote", "remote": "198.51.100.8",
			"email-start-tls": "false", "email-to": secret + "@example.com"})
	h.add("sw", p)
	pool := routeros.NewPool()
	fake := h.onTheWire("sw", pool)
	h.a.runner = func(id string) routeros.Runner { return poolRunner(pool, id) }

	h.run(15 * time.Minute)

	// Every read of the catalogue that applies to this device went out —
	// each with a proplist, or asking for a count.
	calls := wireCalls(fake)
	if len(calls) != 22 {
		t.Fatalf("the device received %d commands, want 22:\n  %s", len(calls), strings.Join(calls, "\n  "))
	}
	sent := map[string]bool{}
	for _, c := range calls {
		if !strings.Contains(c, "=.proplist=") && !strings.Contains(c, "=count-only=") {
			t.Errorf("%q carries neither a proplist nor count-only", c)
		}
		sent[strings.Fields(c)[0]] = true
	}
	for _, cmd := range []string{"/snmp/community/print", "/system/scheduler/print", "/user/active/print", "/system/logging/action/print"} {
		if !sent[cmd] {
			t.Fatalf("%s was not read: the test proves nothing about it", cmd)
		}
	}
	if r := h.lastRun("sw", queries.AuditKindAudit); r.Status != queries.AuditComplete || r.Reads != 22 {
		t.Fatalf("run = %+v", r)
	}

	// The alert rules read what the audit stored, too.
	if err := alerts.NewEngine(h.db, nil, alerts.DefaultRules(h.db)...).Tick(); err != nil {
		t.Fatalf("alert tick: %v", err)
	}
	stored := dumpAuditTables(t, h.db)
	if strings.Contains(stored, secret) {
		for _, line := range strings.Split(stored, "\n") {
			if strings.Contains(line, secret) {
				t.Errorf("a secret was stored: %s", line)
			}
		}
	}
	// It did store things — the test is not passing on an empty database.
	for _, want := range []string{"device_audit.snapshot=", `"snmp_communities"`, "snmp_open", "fast_scheduler", "audit_critical"} {
		if !strings.Contains(stored, want) {
			t.Errorf("the stored rows lack %q", want)
		}
	}
	// And no field of the stored snapshot has a name that says "secret".
	snap, err := queries.LatestDeviceAudit(h.db, "sw")
	if err != nil || snap == nil {
		t.Fatalf("no snapshot: %v", err)
	}
	for _, field := range []string{`"name":"` + secret, `"password"`, `"on-event"`, `"trap-community"`, `"authentication-password"`, `"encryption-password"`, `"email-to"`} {
		if strings.Contains(snap.Snapshot, field) {
			t.Errorf("the snapshot holds %s", field)
		}
	}
}

// The golden devices over the wire: a clean access point and a clean 48-port
// switch are audited and no critical finding — and no audit_critical alert —
// comes of it. Then somebody enables graphing on the switch: the next audit
// finds it, and the alert opens with the finding and its fix.
func TestGoldenDevicesOverTheWireRaiseNoCriticalAlert(t *testing.T) {
	h := newAuditHarness(t)
	clean := hygienetest.Load(t, "switch48_clean")
	h.add("sw", clean)
	h.add("ap", hygienetest.Load(t, "ap_clean"))
	pool := routeros.NewPool()
	swFake, apFake := h.onTheWire("sw", pool), h.onTheWire("ap", pool)
	h.a.runner = func(id string) routeros.Runner { return poolRunner(pool, id) }
	engine := alerts.NewEngine(h.db, nil, alerts.DefaultRules(h.db)...)
	auditAlerts := func() []queries.Alert {
		t.Helper()
		if err := engine.Tick(); err != nil {
			t.Fatalf("alert tick: %v", err)
		}
		open, err := queries.ListOpenAlerts(h.db)
		if err != nil {
			t.Fatal(err)
		}
		var out []queries.Alert
		for _, a := range open {
			if a.Rule == alerts.RuleAuditCritical {
				out = append(out, a)
			}
		}
		return out
	}

	h.run(15 * time.Minute)
	if n := len(swFake.Commands()); n != 19 {
		t.Errorf("the clean switch received %d commands, want 19:\n  %s", n, strings.Join(wireCalls(swFake), "\n  "))
	}
	if n := len(apFake.Commands()); n != 12 {
		t.Errorf("the clean access point received %d commands, want 12:\n  %s", n, strings.Join(wireCalls(apFake), "\n  "))
	}
	for _, id := range []string{"sw", "ap"} {
		if r := h.lastRun(id, queries.AuditKindAudit); r == nil || r.Status != queries.AuditComplete {
			t.Fatalf("%s: run = %+v", id, r)
		}
		for kind, f := range h.findings(id) {
			if f.Severity == "critical" {
				t.Errorf("%s: critical finding on a clean device: %s — %s", id, kind, f.Detail)
			}
		}
	}
	if open := auditAlerts(); len(open) != 0 {
		t.Fatalf("audit_critical fired for a clean device: %+v", open)
	}

	// Graphing appears on the switch.
	swFake.SetReply("/tool/graphing/interface/print", graphingRule)
	h.idle(5*time.Hour + 50*time.Minute)
	h.run(20 * time.Minute)
	open := auditAlerts()
	if len(open) != 1 || open[0].DeviceID != "sw" || open[0].Severity != alerts.SeverityCritical {
		t.Fatalf("after graphing was enabled: %+v", open)
	}
	payload := alerts.ParsePayload(open[0].Payload)
	if !strings.Contains(payload.Summary, "Graphing is enabled on a weak device") {
		t.Errorf("summary = %q", payload.Summary)
	}
	if fix := fmt.Sprint(payload.Facts[alerts.FactFix]); !strings.Contains(fix, `/tool graphing interface remove [find interface="all"]`) {
		t.Errorf("the alert does not carry the fix: %v", payload.Facts[alerts.FactFix])
	}

	// It is removed again: the next audit takes the finding away, and the
	// alert with it.
	swFake.SetReply("/tool/graphing/interface/print")
	h.idle(5*time.Hour + 50*time.Minute)
	h.run(20 * time.Minute)
	if open := auditAlerts(); len(open) != 0 {
		t.Fatalf("the alert outlived the graphing rule: %+v", open)
	}
	if _, still := h.findings("sw")[hygiene.KindGraphing]; still {
		t.Fatal("the finding outlived the graphing rule")
	}
}

// recordingDeliverer records the notifications the alert engine sends for
// one rule.
type recordingDeliverer struct {
	rule string
	sent []string
}

func (d *recordingDeliverer) Deliver(a queries.Alert, event alerts.Event) (int, error) {
	if a.Rule != d.rule {
		return 1, nil
	}
	causes := 0
	if lines, ok := alerts.ParsePayload(a.Payload).Facts[alerts.FactLikelyCause].([]any); ok {
		causes = len(lines)
	}
	d.sent = append(d.sent, fmt.Sprintf("%s %s causes=%d", event, a.Rule, causes))
	return 1, nil
}

// The whole chain behind "why is this switch at 100 %": its minutes are
// high, cpu_high opens and is announced; the alert starts the diagnose pass
// on the device; the pass finds the graphing rule; the alert then carries it
// as the likely cause and is announced once more.
func TestCPUHighAlertGetsItsLikelyCauseFromTheDiagnosePass(t *testing.T) {
	h := newAuditHarness(t)
	// The alert engine runs on the wall clock: so does this test's device.
	start := time.Now().UTC().Truncate(time.Second)
	h.clock.set(start)
	h.add("sw", hygienetest.Load(t, "switch48_clean").With("/tool/graphing/interface/print", graphingRule))
	h.setCPU("sw", 100)
	pool := routeros.NewPool()
	fake := h.onTheWire("sw", pool)
	h.a.runner = func(id string) routeros.Runner { return poolRunner(pool, id) }

	var minutes []queries.DeviceMinute
	for i := 12; i >= 3; i-- { // the newest final minute is three minutes back
		minutes = append(minutes, queries.DeviceMinute{DeviceID: "sw", Bucket: start.Truncate(time.Minute).Add(-time.Duration(i) * time.Minute),
			PolledS: 60, CPUSum: 100, CPUSamples: 1, CPUMax: 100})
	}
	if err := queries.UpsertDeviceMinutes(h.db, minutes); err != nil {
		t.Fatal(err)
	}
	deliverer := &recordingDeliverer{rule: alerts.RuleCPUHigh}
	engine := alerts.NewEngine(h.db, deliverer, alerts.DefaultRules(h.db)...)
	tick := func() {
		t.Helper()
		if err := engine.Tick(); err != nil {
			t.Fatalf("alert tick: %v", err)
		}
	}

	tick()
	if len(deliverer.sent) != 1 || deliverer.sent[0] != "firing cpu_high causes=0" {
		t.Fatalf("first notification: %v", deliverer.sent)
	}

	h.run(8 * time.Minute) // the pass: 11 reads, 30 s apart
	if n := len(fake.Commands()); n != 11 {
		t.Fatalf("the device received %d commands, want the diagnose pass (11):\n  %s", n, strings.Join(wireCalls(fake), "\n  "))
	}

	tick()
	if len(deliverer.sent) != 2 || deliverer.sent[1] != "firing cpu_high causes=1" {
		t.Fatalf("after the diagnose pass: %v, want the alert again with its cause", deliverer.sent)
	}
	open, _ := queries.ListOpenAlerts(h.db)
	var cause string
	for _, a := range open {
		if a.Rule == alerts.RuleCPUHigh {
			cause = fmt.Sprint(alerts.ParsePayload(a.Payload).Facts[alerts.FactLikelyCause])
		}
	}
	if !strings.Contains(cause, "[critical] Graphing is enabled on a weak device") {
		t.Fatalf("likely cause = %q", cause)
	}
	// And then it is old news.
	tick()
	tick()
	if len(deliverer.sent) != 2 {
		t.Fatalf("the cause was announced again: %v", deliverer.sent[2:])
	}
}
