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
// cause of an open alert. A device gets at most one pass an hour, so the
// pass of the current episode is younger than this, and one from yesterday
// is not passed off as today's.
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

// likelyCauses reads every device's newest diagnose pass of the last
// likelyCauseWindow and words what it found. A pass that found nothing is
// worded too: "not on the device" is an answer.
func likelyCauses(db *sql.DB, now time.Time) (map[string]diagnosed, error) {
	runs, err := queries.ListLatestDiagnoseRuns(db, now.Add(-likelyCauseWindow))
	if err != nil {
		return nil, err
	}
	out := make(map[string]diagnosed, len(runs))
	for id, run := range runs {
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
		when := run.FinishedAt.UTC().Format("15:04 UTC")
		if len(found) == 0 {
			d.lines = append(d.lines, fmt.Sprintf("The diagnose pass at %s (%d reads) found none of the usual device-side "+
				"causes: no graphing, traffic-flow, running sniffer, mirror or switch rule to the CPU, unsuitable bond "+
				"mode, bridged traffic through the IP firewall, SNMP or other API client.", when, run.Reads))
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
	// The watch trips when the CPU reaches flowWatchHighPct in
	// flowWatchHighCount of its minutes, or once rises flowWatchRise points
	// above the device's 24 h baseline.
	flowWatchHighPct   = 80
	flowWatchHighCount = 3
	flowWatchRise      = 30
	// flowWatchCalmMinutes is the window the alert is judged by once the
	// watch is over: it stands while any of them is still elevated.
	flowWatchCalmMinutes = 5
)

// flowExportCPUHigh fires when a device's CPU goes up in the 30 minutes
// after the collector started accepting its flow export: three minutes at or
// above 80 %, or one 30 points above the device's 24-hour baseline (the
// watch row the poller writes when the exporter is accepted). A warning: the
// export is the likely reason, and the diagnose pass it starts looks at the
// others.
//
// Once tripped it stands for the rest of the watch. After that it stands
// while the device's newest minutes are still elevated and resolves when
// they are calm; a device that is not being recorded keeps its state.
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
	causes, err := likelyCauses(r.db, now)
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
		minutes, err := queries.ListDeviceMinutes(r.db, w.DeviceID, start, now)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", RuleFlowExportCPUHigh, err)
		}

		// What the watch saw.
		var highs int
		var peak, peakRise float64
		var newest time.Time
		byUnix := make(map[int64]queries.DeviceMinute, len(minutes))
		for _, m := range minutes {
			byUnix[m.Bucket.Unix()] = m
			if m.Bucket.After(newest) {
				newest = m.Bucket
			}
			if !m.Bucket.Before(end) {
				continue
			}
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

		if now.Before(end) {
			out = append(out, o) // the watch is still running
			continue
		}
		// The watch is over: is the CPU still up?
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
