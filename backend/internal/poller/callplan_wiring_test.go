package poller

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// The call-plan harness is only worth its golden files while two things
// hold: it runs the loops the way the manager does, and the hour it
// simulates is an hour in which the loops work — a fleet whose reads fail is
// sent commands too.

// mirroredFuncs are the functions the harness repeats by hand
// ((*callPlan).wire): Manager.Start, which builds and wires the loops, and
// each loop's own run function — its start delay, its tick and its sleep.
// They launch goroutines and sleep in real time, so a simulated clock cannot
// call them; it calls what they call.
var mirroredFuncs = []struct{ file, recv, name string }{
	{"manager.go", "Manager", "Start"},
	{"manager.go", "Manager", "healthLoop"},
	{"manager.go", "Manager", "infoLoop"},
	{"manager.go", "Manager", "topologyLoop"},
	{"cpusample.go", "CPUSampler", "Run"},
	{"portstats.go", "PortStatsCollector", "Run"},
	{"portstats.go", "PortStatsCollector", "hostsLoop"},
	{"wifi.go", "WifiTracker", "Run"},
	{"client_discovery.go", "ClientDiscoveryPoller", "Run"},
	{"network_health.go", "NetworkHealthPoller", "Run"},
	{"connectivity.go", "ConnectivityPoller", "Run"},
	{"audit.go", "Auditor", "Run"},
}

// funcBody returns the body of the method recv.name in file as gofmt would
// print it, without comments and blank lines: what it does, whatever it says
// about it.
func funcBody(t *testing.T, file, recv, name string) []string {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Body == nil {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if id, ok := star.X.(*ast.Ident); !ok || id.Name != recv {
			continue
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, fn.Body); err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, l := range strings.Split(buf.String(), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, l)
			}
		}
		return lines
	}
	t.Fatalf("%s has no (*%s).%s: the wiring check looks at nothing", file, recv, name)
	return nil
}

// The harness builds the loops Manager.Start builds and repeats each loop's
// run function, by hand. This pins what those functions do, so that a loop
// added to Start, wired differently, or given another start delay or sleep
// cannot stay out of the call plans unnoticed.
func TestCallPlanWiringIsPinned(t *testing.T) {
	bodies := map[string][]string{}
	var order []string
	var b strings.Builder
	for _, f := range mirroredFuncs {
		title := fmt.Sprintf("(*%s).%s in %s", f.recv, f.name, f.file)
		order = append(order, title)
		bodies[title] = funcBody(t, f.file, f.recv, f.name)
		fmt.Fprintf(&b, "== %s\n%s\n\n", title, strings.Join(bodies[title], "\n"))
	}
	got := b.String()

	path := filepath.Join("testdata", "callplan", "wiring.golden")
	if updateGolden() {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no pinned copy of the loops' wiring (%v). Write it with: go test ./internal/poller -run TestCallPlan -update", err)
	}
	if got == string(raw) {
		return
	}

	// Say which functions changed, and how.
	pinned := map[string][]string{}
	var title string
	for _, l := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(l, "== "):
			title = strings.TrimPrefix(l, "== ")
			pinned[title] = []string{}
		case l != "":
			pinned[title] = append(pinned[title], l)
		}
	}
	set := func(lines []string) map[string]bool {
		out := map[string]bool{}
		for _, l := range lines {
			out[l] = true
		}
		return out
	}
	var diff []string
	for _, title := range order {
		before, now := pinned[title], bodies[title]
		if strings.Join(before, "\n") == strings.Join(now, "\n") {
			continue
		}
		diff = append(diff, "  "+title+":")
		inNow, inBefore := set(now), set(before)
		for _, l := range before {
			if !inNow[l] {
				diff = append(diff, "    - "+l)
			}
		}
		for _, l := range now {
			if !inBefore[l] {
				diff = append(diff, "    + "+l)
			}
		}
	}
	t.Errorf("the loops are not started the way the call-plan harness was written after:\n%s\n"+
		"The harness repeats these functions by hand ((*callPlan).wire in callplan_harness_test.go). If the change adds, removes or\n"+
		"rewires a loop that reads devices, or changes when a loop wakes, make the same change there. Then pin the new wiring and\n"+
		"rewrite the plans — go test ./internal/poller -run TestCallPlan -update — and read the diff of the plans.",
		strings.Join(diff, "\n"))
}

// checkOutcome says what is wrong with the simulated hour, if it did not do
// what an hour of polling does: every device audited once and completely,
// every probe answered, the watched client's signal recorded, every device
// in the role its personality has, and its interface list stored whole.
func (h *callPlan) checkOutcome() (problems []string) {
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	count := func(query string, args ...any) int {
		var n int
		if err := h.db.QueryRow(query, args...).Scan(&n); err != nil {
			fail("call plan: %s: %v", query, err)
		}
		return n
	}
	roles, _, err := topology.LoadDeviceRoles(h.db, UplinkAge(h.m.cadence), h.clock.now())
	if err != nil {
		return append(problems, fmt.Sprintf("call plan: device roles: %v", err))
	}
	for _, d := range h.devices {
		if got := roles[d.id].Role; got != d.p.Role {
			fail("%s ended the hour as %q (%s), its personality is a %q: the fleet is not what the plans say it is",
				d.id, got, roles[d.id].Reason, d.p.Role)
		}
		dev, err := queries.GetDevice(h.db, d.id)
		if err != nil {
			fail("call plan: %s: %v", d.id, err)
			continue
		}
		if dev.Status != "online" || dev.ProbedClass() != d.p.Class {
			fail("%s ended the hour %s in class %s, want online in class %s", d.id, dev.Status, dev.ProbedClass(), d.p.Class)
		}
		if got := count(`SELECT COUNT(*) FROM interfaces WHERE device_id = ?`, d.id); got != len(d.p.Ports) {
			fail("%s: %d interfaces stored, its personality has %d: the counter read is not being understood", d.id, got, len(d.p.Ports))
		}
		audits := count(`SELECT COUNT(*) FROM device_audit_runs WHERE device_id = ? AND kind = ?`, d.id, queries.AuditKindAudit)
		complete := count(`SELECT COUNT(*) FROM device_audit_runs WHERE device_id = ? AND kind = ? AND status = ? AND failed_reads = 0`,
			d.id, queries.AuditKindAudit, queries.AuditComplete)
		if audits != 1 || complete != 1 {
			fail("%s: %d hygiene audits in the hour, %d of them complete; want exactly one, complete", d.id, audits, complete)
		}
	}
	if n := count(`SELECT COUNT(*) FROM device_audit_runs WHERE kind != ?`, queries.AuditKindAudit); n != 0 {
		fail("%d diagnose passes ran: no alert was open in this hour", n)
	}
	for _, target := range []string{"internet", "watch"} {
		ok := count(`SELECT COUNT(*) FROM ping_samples WHERE target_id = ? AND device_id = 'rtr' AND error = '' AND received > 0`, target)
		failed := count(`SELECT COUNT(*) FROM ping_samples WHERE target_id = ? AND (error != '' OR device_id != 'rtr')`, target)
		if ok < 120 || failed > 0 {
			fail("ping target %q: %d answered samples from the router and %d others, want one every 30 s and nothing else", target, ok, failed)
		}
	}
	if n := count(`SELECT COUNT(*) FROM client_signal_samples WHERE mac_address = ? AND signal_dbm IS NOT NULL`, planWatchedMAC); n < 120 {
		fail("%d signal samples of the watched client, want one per registration read of its access point (every 30 s)", n)
	}
	return problems
}
