package poller

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

// Golden call plans: what the scheduled loops send each kind of device in
// the first hour after the NMS starts, one file per device in
// testdata/callplan. A change in what a device is sent is a change in one of
// these files — a diff a reviewer can read — and past a point it breaks a
// budget below.
//
//	go test ./internal/poller -run TestCallPlan -update    # rewrite the files
//
// (UPDATE_GOLDEN=1 does the same.) Read the diff before committing it: it is
// the device's side of the change.

var updateCallPlans = flag.Bool("update", false, "rewrite the golden call plans in testdata/callplan")

// planStartupMinutes are the minutes in which a just-started NMS reads
// everything once — the capability probes, the inventory, the first read of
// every table. The per-minute budget starts after them.
const planStartupMinutes = 2

// planBudget is what a device of the fleet may be sent.
type planBudget struct {
	// perMinute bounds the hour's average of commands per minute.
	perMinute float64
	// oneMinute bounds the commands of any single minute once start-up is
	// over: where the 5-, 15- and 30-minute reads fall together, or the
	// minute of the device's hygiene audit.
	oneMinute int
}

// planBudgets are the budgets: what the loops send today (the numbers in the
// comment) plus a small margin. A loop that starts printing a table on every
// cycle is far outside it — the registration prints of the connectivity
// poller, fixed in fa697a4, were up to six more commands a minute on every
// device. Raising a number is a decision about that class of device, to be
// made in review; none may be above its class's ceiling (devclass.BudgetOf),
// and the two C1 devices are at it in their busiest minute already.
var planBudgets = map[string]planBudget{
	"sw48":  {perMinute: 5, oneMinute: 18},   // C1 switch, 48 ports:    4.5 a minute today, 17 in its busiest
	"ap-ac": {perMinute: 5.5, oneMinute: 18}, // C1 access point:        4.8 a minute today, 18 in its busiest
	"sw24":  {perMinute: 9, oneMinute: 28},   // C2 switch, 24 ports:    7.9 a minute today, 26 in its busiest
	"ap-ax": {perMinute: 14, oneMinute: 28},  // C3 access point:       12.8 a minute today, 26 in its busiest
	"rtr":   {perMinute: 18, oneMinute: 30},  // C3 router, two probes: 16.3 a minute today, 27 in its busiest
}

// printsWithoutProplist are the prints the loops send whole today: neither a
// .proplist nor =count-only=, so RouterOS formats every field of every row.
// Every other print must carry one of the two. An entry is a debt with its
// debtor, not a licence: one the fleet is no longer sent whole has to go.
// (The legacy CAPsMAN and wireless registration tables are read by the same
// reader as the wifi one.)
var printsWithoutProplist = map[string]string{
	"/system/identity/print":                       "the pool's keep-alive when a session is reused (routeros.KeepAlive)",
	"/ip/neighbor/print":                           "Tier 3d, routeros.ReadNeighbors",
	"/ip/route/print":                              "Tier 3e, routeros.GetDefaultRoutes: narrowed by ?dst-address=0.0.0.0/0, the rows still come whole",
	"/interface/bridge/vlan/print":                 "Tier 3a, routeros.GetBridgeVLANs",
	"/ip/arp/print":                                "Tier 5a, routeros.GetARPTable",
	"/ip/dhcp-server/lease/print":                  "Tier 5b, routeros.GetDHCPLeases",
	"/ip/dhcp-server/print":                        "Tier 5b, routeros.GetDHCPLeases: the disabled servers, read after the leases",
	"/interface/wifi/registration-table/print":     "Tier 5c, routeros.GetRegistrations",
	"/caps-man/registration-table/print":           "Tier 5c, routeros.GetRegistrations",
	"/interface/wireless/registration-table/print": "Tier 5c, routeros.GetRegistrations",
}

// planCall is one command a device received.
type planCall struct {
	at      time.Duration // since the start
	command string        // the command word
	key     string        // the command as the plan names it (planKey)
	rows    int           // reply rows
	trap    bool          // the device answered with a !trap
	// bare: neither a .proplist nor =count-only=.
	bare bool
	// scripted is false for a command the personality has no answer for.
	scripted bool
}

// planKey names a command for the plan: the command word, =count-only= and
// the query words as sent, the names of its other attribute words (their
// values are addresses and ids), and the size of its .proplist — the field
// list itself would drown the table, its length still shows a change.
func planKey(ex routerostest.Exchange) (key string, bare bool) {
	var marks, attrs, queries []string
	fields := 0
	for _, w := range ex.Words {
		switch {
		case w == "=count-only=":
			marks = append(marks, w)
		case strings.HasPrefix(w, "=.proplist="):
			fields = len(strings.Split(strings.TrimPrefix(w, "=.proplist="), ","))
		case strings.HasPrefix(w, "?"):
			queries = append(queries, w)
		case strings.HasPrefix(w, "="):
			name, _, _ := strings.Cut(w[1:], "=")
			attrs = append(attrs, "="+name+"=")
		default:
			attrs = append(attrs, w)
		}
	}
	parts := append([]string{ex.Command}, marks...)
	parts = append(parts, attrs...)
	parts = append(parts, queries...)
	switch {
	case fields == 1:
		parts = append(parts, "[1 field]")
	case fields > 1:
		parts = append(parts, fmt.Sprintf("[%d fields]", fields))
	}
	return strings.Join(parts, " "), fields == 0 && len(marks) == 0
}

// calls returns what the device received, on the plan's time line.
func (d *planDevice) calls() []planCall {
	var out []planCall
	for _, ex := range d.fake.Exchanges() {
		key, bare := planKey(ex)
		out = append(out, planCall{
			at: ex.At.Sub(callPlanStart), command: ex.Command, key: key, rows: ex.Rows,
			trap: ex.Trap != "", bare: bare, scripted: ex.Scripted,
		})
	}
	return out
}

// planStats are the totals of a device's hour.
type planStats struct {
	commands, rows int
	perMinute      []int // commands in each minute of the hour
	peak, peakAt   int   // the busiest minute
	// busiest and busiestAt: the busiest minute after start-up.
	busiest, busiestAt int
}

func (s planStats) average() float64 { return float64(s.commands) / float64(len(s.perMinute)) }

func statsOf(calls []planCall, hour time.Duration) planStats {
	s := planStats{perMinute: make([]int, int(hour/time.Minute))}
	for _, c := range calls {
		if c.at >= hour {
			continue
		}
		s.commands++
		s.rows += c.rows
		s.perMinute[int(c.at/time.Minute)]++
	}
	for m, n := range s.perMinute {
		if n > s.peak {
			s.peak, s.peakAt = n, m
		}
		if m >= planStartupMinutes && n > s.busiest {
			s.busiest, s.busiestAt = n, m
		}
	}
	return s
}

// cadenceOf words how often a command is sent: the median gap between the
// moments it was sent at, as the cadence that implies — "2× 30 s" when it is
// sent twice at each of them.
func cadenceOf(at []time.Duration) string {
	var moments []time.Duration
	for _, t := range at {
		if len(moments) == 0 || t != moments[len(moments)-1] {
			moments = append(moments, t)
		}
	}
	if len(moments) < 2 {
		return "once"
	}
	gaps := make([]time.Duration, 0, len(moments)-1)
	for i := 1; i < len(moments); i++ {
		gaps = append(gaps, moments[i]-moments[i-1])
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	g := gaps[len(gaps)/2].Round(time.Second)
	every := fmt.Sprintf("%d s", int(g/time.Second))
	if g >= time.Minute {
		every = fmt.Sprintf("%d min", int(g/time.Minute))
		if rest := g % time.Minute; rest != 0 {
			every = fmt.Sprintf("%d min %d s", int(g/time.Minute), int(rest/time.Second))
		}
	}
	if len(at)%len(moments) == 0 && len(at) > len(moments) {
		every = fmt.Sprintf("%d× %s", len(at)/len(moments), every)
	}
	return every
}

// minuteLines lists the commands of one minute in the order they were first
// sent, with how often.
func minuteLines(calls []planCall, minute int) string {
	var order []string
	count := map[string]int{}
	total := 0
	for _, c := range calls {
		if int(c.at/time.Minute) != minute {
			continue
		}
		if count[c.key] == 0 {
			order = append(order, c.key)
		}
		count[c.key]++
		total++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "minute %d: %d commands\n", minute, total)
	for _, key := range order {
		fmt.Fprintf(&b, "  %3d× %s\n", count[key], key)
	}
	return b.String()
}

// planTable is the plan's table: one line per distinct command of the hour,
// the most frequent first — all of them (top 0), or only the top ones.
func planTable(calls []planCall, hour time.Duration, top int) string {
	type line struct {
		key         string
		at          []time.Duration
		rows, traps int
	}
	byKey := map[string]*line{}
	var lines []*line
	for _, c := range calls {
		if c.at >= hour {
			continue
		}
		l := byKey[c.key]
		if l == nil {
			l = &line{key: c.key}
			byKey[c.key] = l
			lines = append(lines, l)
		}
		l.at = append(l.at, c.at)
		l.rows += c.rows
		if c.trap {
			l.traps++
		}
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if len(lines[i].at) != len(lines[j].at) {
			return len(lines[i].at) > len(lines[j].at)
		}
		return lines[i].key < lines[j].key
	})
	var b strings.Builder
	fmt.Fprintf(&b, "%5s  %-11s  %9s  %s\n", "count", "every", "rows/call", "command")
	for i, l := range lines {
		if top > 0 && i == top {
			fmt.Fprintf(&b, "%5s  and %d more commands, none sent more than %d times\n", "…", len(lines)-top, len(l.at))
			break
		}
		n := len(l.at)
		rows := fmt.Sprintf("%d", l.rows/n)
		switch {
		case l.traps == n:
			rows = "trap"
		case l.traps > 0:
			rows = fmt.Sprintf("%d traps", l.traps)
		case l.rows%n != 0:
			rows = fmt.Sprintf("%.1f", float64(l.rows)/float64(n))
		}
		fmt.Fprintf(&b, "%5d  %-11s  %9s  %s\n", n, cadenceOf(l.at), rows, l.key)
	}
	return b.String()
}

// renderPlan is the golden file of one device.
func renderPlan(d *planDevice, calls []planCall, hour time.Duration) string {
	s := statsOf(calls, hour)
	var b strings.Builder
	fmt.Fprintf(&b, "# Call plan of %s: what the NMS's scheduled loops send this device.\n", d.id)
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# device:   %s, RouterOS %s, class %s, role %s (personality %s)\n", d.p.Board, d.p.Version, d.p.Class, d.p.Role, d.p.Name)
	fmt.Fprintf(&b, "# scenario: the NMS starts with this device in a known fleet of %d and polls it for one hour:\n", len(planFleet))
	fmt.Fprintf(&b, "#           default settings, nominal cadences (no jitter), a command takes no time\n")
	fmt.Fprintf(&b, "#\n")
	fmt.Fprintf(&b, "# A change in this file is a change in what a device of this kind is sent.\n")
	fmt.Fprintf(&b, "# Rewrite it with: go test ./internal/poller -run TestCallPlan -update\n")
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "commands in the hour     %d\n", s.commands)
	fmt.Fprintf(&b, "  average per minute     %.2f\n", s.average())
	fmt.Fprintf(&b, "  most in one minute     %d (minute %d)\n", s.peak, s.peakAt)
	fmt.Fprintf(&b, "  most after start-up    %d (minute %d; start-up is minutes 0-%d)\n", s.busiest, s.busiestAt, planStartupMinutes-1)
	fmt.Fprintf(&b, "reply rows in the hour   %d\n", s.rows)
	fmt.Fprintf(&b, "liveness dials           %d (a TCP connect to the API port, no command)\n", d.dials)

	fmt.Fprintf(&b, "\n## Commands in the hour\n\n")
	b.WriteString(planTable(calls, hour, 0))

	fmt.Fprintf(&b, "\n## Commands per minute\n\n")
	for m := 0; m < len(s.perMinute); m += 10 {
		fmt.Fprintf(&b, "minutes %2d-%2d:", m, m+9)
		for _, n := range s.perMinute[m:min(m+10, len(s.perMinute))] {
			fmt.Fprintf(&b, " %3d", n)
		}
		fmt.Fprintf(&b, "\n")
	}

	fmt.Fprintf(&b, "\n## The first 10 minutes\n\n")
	for m := 0; m < 10; m++ {
		b.WriteString(minuteLines(calls, m))
	}
	fmt.Fprintf(&b, "\n## The minute after the hour: the hourly inventory pass comes round\n\n")
	b.WriteString(minuteLines(calls, int(hour/time.Minute)))
	return b.String()
}

func goldenPlanPath(id string) string {
	return filepath.Join("testdata", "callplan", id+".golden")
}

// planCounts reads the commands and their counts out of a rendered plan's
// table.
func planCounts(plan string) map[string]string {
	out := map[string]string{}
	in := false
	for _, l := range strings.Split(plan, "\n") {
		switch {
		case strings.HasPrefix(l, "## Commands in the hour"):
			in = true
			continue
		case strings.HasPrefix(l, "## "):
			in = false
		}
		// count, every, rows/call, then the command — the first word that
		// starts with a slash.
		at := strings.Index(l, " /")
		if f := strings.Fields(l); in && at >= 0 && len(f) > 0 {
			out[strings.TrimSpace(l[at:])] = f[0]
		}
	}
	return out
}

// planDiff says what changed between a golden plan and the one the loops
// produce now, for a failure that can be read without opening two files: the
// commands whose count in the hour changed, and the first line that differs.
func planDiff(got, want string) string {
	g, w := planCounts(got), planCounts(want)
	keys := map[string]bool{}
	for k := range g {
		keys[k] = true
	}
	for k := range w {
		keys[k] = true
	}
	orZero := func(s string) string {
		if s == "" {
			return "0"
		}
		return s
	}
	var changed []string
	for k := range keys {
		if g[k] != w[k] {
			changed = append(changed, fmt.Sprintf("    %5s → %-5s %s", orZero(w[k]), orZero(g[k]), k))
		}
	}
	sort.Strings(changed)
	var b strings.Builder
	if len(changed) > 0 {
		fmt.Fprintf(&b, "  times in the hour, golden → now:\n%s\n", strings.Join(changed, "\n"))
	} else {
		fmt.Fprintf(&b, "  every command is sent as often as before; when, or what comes back, changed\n")
	}
	gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < max(len(gl), len(wl)); i++ {
		var a, z string
		if i < len(wl) {
			a = wl[i]
		}
		if i < len(gl) {
			z = gl[i]
		}
		if a != z {
			fmt.Fprintf(&b, "  first difference, line %d:\n    golden: %s\n    now:    %s\n", i+1, a, z)
			break
		}
	}
	return b.String()
}

// The plans of the fleet: an hour of every scheduled loop against the five
// personalities. Each device's plan is compared with its golden file and
// held to its budget, every command must be one its personality answers, and
// every print must name its fields.
func TestCallPlans(t *testing.T) {
	h := newCallPlan(t)
	// One minute past the hour: the hourly inventory pass comes round then,
	// and with it the only command of the pool's own (its keep-alive).
	h.run(h.hour + time.Minute)
	// A plan is only worth reading if the hour behind it worked.
	h.checkOutcome()

	update := *updateCallPlans || os.Getenv("UPDATE_GOLDEN") == "1"
	bare := map[string]bool{}
	for _, d := range h.devices {
		calls := d.calls()
		if len(calls) == 0 {
			t.Fatalf("%s was sent nothing: the harness reached no device", d.id)
		}
		reported := map[string]bool{}
		for _, c := range calls {
			// A command the personality has no answer for was answered with
			// the trap of a missing menu, and a plan built on that is worth
			// nothing: the device has to be taught the command.
			if !c.scripted && !reported[c.command] {
				reported[c.command] = true
				t.Errorf("%s was sent %s, which its personality %s has no answer for. Add the command to the fixture "+
					"(internal/hygiene/hygienetest/fixtures): the rows a device of this kind returns, or the trap of one without the menu.",
					d.id, c.command, d.p.Name)
			}
			// Every print names its fields, or asks for the count only.
			if c.bare && strings.HasSuffix(c.command, "/print") && !bare[c.command] {
				bare[c.command] = true
				if _, listed := printsWithoutProplist[c.command]; !listed {
					t.Errorf("%s was sent %q with neither a .proplist nor =count-only=: RouterOS then formats every field of "+
						"every row. Name the fields the reader uses — or, if the table has to be read whole, list it in "+
						"printsWithoutProplist with the reason.", d.id, c.key)
				}
			}
		}

		// The golden file: what changed.
		got, path := renderPlan(d, calls, h.hour), goldenPlanPath(d.id)
		if update {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		} else if want, err := os.ReadFile(path); err != nil {
			t.Errorf("%s has no golden call plan (%v). Write it with: go test ./internal/poller -run TestCallPlan -update", d.id, err)
		} else if got != string(want) {
			t.Errorf("%s is not sent what its golden call plan says (%s).\n%s"+
				"  If the change is meant: go test ./internal/poller -run TestCallPlan -update, and commit the diff — it is what the change costs a device.",
				d.id, path, planDiff(got, string(want)))
		}

		// The budget: whether it is too much — also for a plan that was just
		// rewritten.
		budget, ok := planBudgets[d.id]
		if !ok {
			t.Fatalf("%s has no budget in planBudgets", d.id)
		}
		s := statsOf(calls, h.hour)
		if avg := s.average(); avg > budget.perMinute {
			t.Errorf("%s (class %s) is sent %.1f commands a minute on average — %d in the hour. Its budget is %.1f a minute.\n"+
				"What it is sent most:\n%s", d.id, d.p.Class, avg, s.commands, budget.perMinute, planTable(calls, h.hour, 8))
		}
		if s.busiest > budget.oneMinute {
			t.Errorf("%s (class %s) is sent %d commands in minute %d. Its budget for one minute, once the %d minutes of start-up are over, is %d.\n%s",
				d.id, d.p.Class, s.busiest, s.busiestAt, planStartupMinutes, budget.oneMinute, minuteLines(calls, s.busiestAt))
		}
		// No budget here may be above what the class was sized for (design
		// §4.4): its sustained rate on average, rate plus burst in a minute.
		class := devclass.BudgetOf(d.p.Class)
		if budget.perMinute > float64(class.CommandsPerMin) || budget.oneMinute > class.CommandsPerMin+class.BurstCommands {
			t.Errorf("the budget of %s (%.1f a minute, %d in one minute) is above the ceiling of class %s (%d a minute, %d + %d in one minute)",
				d.id, budget.perMinute, budget.oneMinute, d.p.Class, class.CommandsPerMin, class.CommandsPerMin, class.BurstCommands)
		}
	}

	// The exceptions stay honest: a print the fleet is no longer sent whole
	// has been paid off, and comes out of the list.
	for command := range printsWithoutProplist {
		if !bare[command] {
			t.Errorf("printsWithoutProplist lists %s, but no device of the fleet was sent it without a .proplist: remove the entry", command)
		}
	}
}
