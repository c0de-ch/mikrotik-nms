package alerts

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowguard"
	"github.com/mikrotik-nms/backend/internal/hygiene"
)

// Rules that rest on the hygiene audit and the diagnose pass (P0-10), and
// the CPU watch after a flow exporter is accepted (G6). Like every rule they
// read the database only: the passes themselves are the poller's.
const (
	RuleAuditCritical     = "audit_critical"
	RuleFlowExportCPUHigh = "flow_export_cpu_high"
)

// Facts a rule attaches for whoever is paged. Each is a list of text lines;
// the notifier appends them to the mail of a firing alert.
const (
	// FactLikelyCause is what the diagnose pass found on the device, and
	// FactDiagnosedAt when that pass finished. An open alert whose
	// diagnosed_at changes is announced once more (see Engine.Tick).
	FactLikelyCause = "likely_cause"
	FactDiagnosedAt = "diagnosed_at"
	// FactFindings and FactFix are the hygiene findings behind an
	// audit_critical alert and what to do about them.
	FactFindings = "findings"
	FactFix      = "fix"
)

// ---- audit_critical ---------------------------------------------------------------

// auditCritical fires for every device that has a critical hygiene finding:
// a setting the audit or the diagnose pass read on the device and that costs
// its CPU or is unsafe — graphing on a weak switch, a mirror to the CPU,
// traffic-flow on a device that must not export.
//
// One alert per device. It stands for as long as a critical finding does: a
// finding is removed by the pass that reads the device again and finds the
// setting gone, never by the clock — a device that is too busy to be audited
// keeps its finding and its alert.
type auditCritical struct{ db *sql.DB }

func (auditCritical) Name() string { return RuleAuditCritical }

func (r auditCritical) Evaluate(time.Time) ([]Observation, error) {
	findings, err := queries.ListAuditFindings(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleAuditCritical, err)
	}
	byDevice := map[string][]queries.DeviceFinding{}
	for _, f := range findings {
		if f.Severity == "critical" {
			byDevice[f.DeviceID] = append(byDevice[f.DeviceID], f)
		}
	}
	if len(byDevice) == 0 {
		return nil, nil
	}
	devices, err := queries.ListDevices(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleAuditCritical, err)
	}
	var out []Observation
	for _, d := range devices {
		list := byDevice[d.ID]
		if len(list) == 0 {
			continue
		}
		var titles, lines, fix []string
		checked := time.Time{}
		for _, f := range list {
			title := f.Title
			if title == "" {
				title = f.Kind
			}
			titles = append(titles, title)
			lines = append(lines, title+" — "+f.Detail)
			fix = append(fix, title)
			for _, step := range f.Fix {
				text := step.Text
				if step.Window {
					text += " (plan a maintenance window)"
				}
				fix = append(fix, "  "+text)
				for _, c := range step.Commands {
					fix = append(fix, "    "+c)
				}
			}
			if f.LastSeen.After(checked) {
				checked = f.LastSeen
			}
		}
		noun := "critical hygiene findings"
		if len(list) == 1 {
			noun = "critical hygiene finding"
		}
		facts := map[string]any{"device": deviceLabel(d), "count": len(list), FactFindings: lines, FactFix: fix}
		if !checked.IsZero() {
			facts["checked_at"] = checked.UTC().Format(time.RFC3339)
		}
		out = append(out, Observation{
			Key: d.ID, DeviceID: d.ID, Severity: SeverityCritical,
			Summary: fmt.Sprintf("%s: %d %s: %s", deviceLabel(d), len(list), noun, strings.Join(titles, "; ")),
			Facts:   facts,
		})
	}
	return out, nil
}

// ---- the likely cause ---------------------------------------------------------------

// likelyCauseWindow is how old a diagnose pass may be to be shown as the
// cause of an alert it did not answer — one that ran shortly before the
// alert opened (a device gets at most one pass an hour, so a new episode
// within the hour has no pass of its own yet). One from yesterday is not
// passed off as today's.
const likelyCauseWindow = 2 * time.Hour

// diagnosed is what a diagnose pass contributes to an alert.
type diagnosed struct {
	lines []string
	at    time.Time
}

// attach adds the cause to an alert's facts.
func (d diagnosed) attach(facts map[string]any) {
	if len(d.lines) == 0 {
		return
	}
	facts[FactLikelyCause] = d.lines
	facts[FactDiagnosedAt] = d.at.UTC().Format(time.RFC3339)
}

// runFinding is what a pass stored of a finding.
type runFinding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

func findingRank(severity string) int {
	switch severity {
	case "critical":
		return 0
	case "warn":
		return 1
	}
	return 2
}

// likelyCauses words, per device, what its newest diagnose pass found — for
// the alerts of rule. The pass counts when it belongs to the episode:
//
//   - it started at or after the device's open alert of this rule opened —
//     the pass that answered the alert (the auditor applies the same test
//     before it starts one). It stays the alert's cause however long the
//     alert stays open: no second pass is started for an alert that has its
//     answer, and the payload is rewritten at every evaluation;
//   - or it finished within likelyCauseWindow.
//
// A pass that found nothing is worded too: "not on the device" is an answer.
func likelyCauses(db *sql.DB, rule string, now time.Time) (map[string]diagnosed, error) {
	runs, err := queries.ListLatestDiagnoseRuns(db)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return map[string]diagnosed{}, nil
	}
	open, err := queries.ListOpenAlerts(db)
	if err != nil {
		return nil, err
	}
	opened := map[string]time.Time{} // device → when its open alert of this rule opened
	for _, a := range open {
		if a.Rule != rule || a.DeviceID == "" {
			continue
		}
		if at, ok := opened[a.DeviceID]; !ok || a.FirstAt.Before(at) {
			opened[a.DeviceID] = a.FirstAt
		}
	}
	out := make(map[string]diagnosed, len(runs))
	for id, run := range runs {
		since, isOpen := opened[id]
		answers := isOpen && !run.StartedAt.Before(since)
		if !answers && run.FinishedAt.Before(now.Add(-likelyCauseWindow)) {
			continue
		}
		var found []runFinding
		_ = json.Unmarshal([]byte(run.Findings), &found)
		sort.SliceStable(found, func(i, j int) bool { return findingRank(found[i].Severity) < findingRank(found[j].Severity) })
		d := diagnosed{at: run.FinishedAt}
		for _, f := range found {
			title := f.Title
			if title == "" {
				title = f.Kind
			}
			d.lines = append(d.lines, "["+f.Severity+"] "+title+" — "+f.Detail)
		}
		if len(found) == 0 {
			// What the pass saw, in the pass's own words: it never calls
			// absent what it saw present or could not read — on the alert
			// about a device's flow export, traffic-flow is enabled.
			d.lines = append(d.lines, fmt.Sprintf("The diagnose pass at %s (%d reads) %s", run.FinishedAt.UTC().Format("15:04 UTC"),
				run.Reads, hygiene.NothingFound(hygiene.ParseChecks(run.Checks), rule == RuleFlowExportCPUHigh)))
		}
		if run.FailedReads > 0 {
			d.lines = append(d.lines, fmt.Sprintf("The pass is incomplete: %d of its %d reads failed.", run.FailedReads, run.Reads))
		}
		out[id] = d
	}
	return out, nil
}

// ---- flow_export_cpu_high (G6) ----------------------------------------------------------

const (
	// flowWatchWindow is how long a device's CPU is watched after its flow
	// exporter was accepted.
	flowWatchWindow = 30 * time.Minute
	// flowWatchSettle is how long after the window its last minutes may
	// still be written (a device-minute lands when it is final, up to
	// loadFresh late). For that long the watch still counts as running: one
	// that trips on a late minute opens its alert, one that has tripped
	// stands.
	flowWatchSettle = loadFresh
	// The watch trips when the CPU reaches flowWatchHighPct in
	// flowWatchHighCount of its minutes, or once rises flowWatchRise points
	// above the device's 24 h baseline.
	flowWatchHighPct   = 80
	flowWatchHighCount = 3
	flowWatchRise      = 30
	// flowWatchCalmMinutes is the window an alert that is still open is
	// judged by once the watch is over: it stands while any of them is
	// still elevated.
	flowWatchCalmMinutes = 5
)

// FlowExportWatchRunning reports whether the CPU watch of an exporter
// accepted at acceptedAt can still open an alert at now: the 30 minutes
// after the accept, plus the time their last minutes take to be written.
// After that the rule only keeps an alert that is open — and once that has
// resolved, the watch is over (the poller removes its row).
func FlowExportWatchRunning(acceptedAt, now time.Time) bool {
	return now.Before(acceptedAt.Truncate(time.Minute).Add(flowWatchWindow + flowWatchSettle))
}

// flowExportCPUHigh fires when a device's CPU goes up in the 30 minutes
// after the collector started accepting its flow export: three minutes at or
// above 80 %, or one 30 points above the device's 24-hour baseline (the
// watch row the poller writes when the exporter is accepted). A warning: the
// export is the likely reason, and the diagnose pass it starts looks at the
// others.
//
// Once tripped it stands for the rest of the watch. After that it only keeps
// its alert: the alert stands while the device's newest minutes are still
// elevated and resolves when they are calm (a device that is not being
// recorded keeps its state) — and a resolved alert is the end of the watch.
// A CPU spike days later is not "after its flow export was accepted": it
// opens nothing here (Observation.KeepOnly), whether or not the watch row is
// still in the table.
type flowExportCPUHigh struct{ db *sql.DB }

func (flowExportCPUHigh) Name() string { return RuleFlowExportCPUHigh }

// elevated reports a minute whose CPU is at the watch's absolute line, or
// above the baseline by the watch's rise; known is false without a sample.
func elevated(m queries.DeviceMinute, baseline *float64) (high, rise, known bool) {
	avg, ok := m.CPUAvg()
	if !ok {
		return false, false, false
	}
	high = m.CPUMax >= flowWatchHighPct
	rise = baseline != nil && avg >= *baseline+flowWatchRise
	return high, rise, true
}

func (r flowExportCPUHigh) Evaluate(now time.Time) ([]Observation, error) {
	watches, err := queries.ListFlowExportWatches(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleFlowExportCPUHigh, err)
	}
	if len(watches) == 0 {
		return nil, nil
	}
	devices, err := queries.ListDevices(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleFlowExportCPUHigh, err)
	}
	byID := make(map[string]queries.Device, len(devices))
	for _, d := range devices {
		byID[d.ID] = d
	}
	causes, err := likelyCauses(r.db, RuleFlowExportCPUHigh, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleFlowExportCPUHigh, err)
	}
	advertise, _ := queries.GetSetting(r.db, "flow_advertise_address")

	var out []Observation
	for _, w := range watches {
		d, ok := byID[w.DeviceID]
		if !ok || w.AcceptedAt.After(now) {
			continue
		}
		start := w.AcceptedAt.Truncate(time.Minute)
		end := start.Add(flowWatchWindow)
		minutes, err := queries.ListDeviceMinutes(r.db, w.DeviceID, start, end)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", RuleFlowExportCPUHigh, err)
		}

		// What the watch saw: the minutes of its half hour, no others.
		var highs int
		var peak, peakRise float64
		for _, m := range minutes {
			high, rise, known := elevated(m, w.Baseline)
			if !known {
				continue
			}
			if high {
				highs++
				peak = math.Max(peak, float64(m.CPUMax))
			}
			if rise {
				avg, _ := m.CPUAvg()
				peakRise = math.Max(peakRise, avg)
			}
		}
		tripped := highs >= flowWatchHighCount || peakRise > 0
		if !tripped {
			continue
		}

		name := deviceLabel(d)
		summary := fmt.Sprintf("%s: CPU reached %d %% or more %d times in the %d minutes after its flow export was accepted — "+
			"the export may be loading it", name, flowWatchHighPct, highs, int(flowWatchWindow/time.Minute))
		if highs < flowWatchHighCount {
			summary = fmt.Sprintf("%s: CPU rose to %.0f %%, %.0f points above its 24-hour baseline of %.0f %%, after its flow "+
				"export was accepted — the export may be loading it", name, peakRise, peakRise-*w.Baseline, *w.Baseline)
		}
		facts := map[string]any{"device": name, "address": w.Address, "accepted_at": w.AcceptedAt.UTC().Format(time.RFC3339),
			"high_minutes": highs, "watch_minutes": int(flowWatchWindow / time.Minute)}
		if w.Baseline != nil {
			facts["baseline_pct"] = math.Round(*w.Baseline)
		}
		if peak > 0 {
			facts["cpu_max"] = peak
		}
		target, port := flowguard.RollbackTarget(advertise, w.CollectorPort, nil)
		facts["rollback"] = flowguard.Rollback(d.ROSVersion, target, port).Lines()
		causes[d.ID].attach(facts)
		o := Observation{Key: d.ID, DeviceID: d.ID, Severity: SeverityWarning, Summary: summary, Facts: facts}

		if FlowExportWatchRunning(w.AcceptedAt, now) {
			out = append(out, o) // the watch is still running
			continue
		}
		// The watch is over: nothing opens the alert any more. While it is
		// still open, is the CPU still up?
		o.KeepOnly = true
		recent, err := queries.ListDeviceMinutes(r.db, w.DeviceID,
			now.Add(-loadFresh-flowWatchCalmMinutes*time.Minute).Truncate(time.Minute), now.Add(time.Minute))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", RuleFlowExportCPUHigh, err)
		}
		var newest time.Time
		byUnix := make(map[int64]queries.DeviceMinute, len(recent))
		for _, m := range recent {
			byUnix[m.Bucket.Unix()] = m
			if m.Bucket.After(newest) {
				newest = m.Bucket
			}
		}
		if newest.IsZero() || now.Sub(newest) > loadFresh {
			out = append(out, hold(d.ID))
			continue
		}
		var states []minuteState
		for i := flowWatchCalmMinutes - 1; i >= 0; i-- {
			m, found := byUnix[newest.Add(-time.Duration(i)*time.Minute).Unix()]
			high, rise, known := elevated(m, w.Baseline)
			switch {
			case !found || !known:
				states = append(states, minuteUnknown)
			case high || rise:
				states = append(states, minuteTrue)
			default:
				states = append(states, minuteFalse)
			}
		}
		anyUp, anyKnown := false, false
		for _, s := range states {
			anyUp = anyUp || s == minuteTrue
			anyKnown = anyKnown || s != minuteUnknown
		}
		switch {
		case anyUp:
			out = append(out, o)
		case !anyKnown:
			out = append(out, hold(d.ID))
		}
	}
	return out, nil
}
