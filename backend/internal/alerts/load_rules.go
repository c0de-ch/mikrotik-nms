package alerts

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// Rules over the device telemetry (design §2.6): cpu_high, mem_low and
// port_egress_drops. They read device_minutes and port_stats_1m — what the
// pollers stored — and never a device.
//
// All three judge a condition over a window of the device's newest final
// minutes, and all three treat a minute that was not measured as unknown,
// which is not healthy:
//
//   - firing: every minute of the window is known and meets the condition;
//   - clear: no minute meets it and at least one is known — the alert
//     resolves;
//   - anything else (minutes that disagree, or nothing known) is a Hold: an
//     open alert stays open, none is opened.
//
// So an alert opens after N consecutive bad minutes, closes after the window
// has moved past the last bad one, and a device that goes quiet — paused,
// unreachable, the NMS down — keeps whatever state it had.

const (
	RuleCPUHigh         = "cpu_high"
	RuleMemLow          = "mem_low"
	RulePortEgressDrops = "port_egress_drops"
)

const (
	// loadFresh is how old a device's newest device-minute may be for the
	// rules to judge it. Minutes are written when they become final, one to
	// a few minutes late (longer at a slow counter cadence); past this the
	// NMS is not recording the device and nothing can be told.
	loadFresh = 10 * time.Minute
	// loadLookback is how far back the rules read: the longest window plus
	// the freshness margin.
	loadLookback = cpuCriticalMinutes*time.Minute + loadFresh

	cpuWarningMinutes  = 5
	cpuCriticalMinutes = 10
	cpuCriticalPct     = 95

	memLowMinutes = 3
	// memLowFloor is the absolute floor for weak (C1) devices: RouterOS on a
	// 64 MiB board is in trouble long before 5 % are left.
	memLowFloor = 8 << 20
	memLowPct   = 5

	egressDropMinutes = 5
	// A minute counts when at least this many packets were dropped on
	// egress and they are at least 0.1 % of the packets sent.
	egressDropMin      = 100
	egressDropPerMille = 1
)

// cpuWarningPct is the warning threshold of a class: a weak device has no
// headroom to spare, so it warns earlier.
func cpuWarningPct(c devclass.Class) int {
	if c.Budgeted() == devclass.C1 {
		return 80
	}
	return 85
}

// verdict is what a window of minutes says about a condition.
type verdict int

const (
	verdictUnknown verdict = iota
	verdictClear
	verdictFiring
)

// minuteState is one minute's answer: the condition holds, does not hold, or
// the minute was not measured.
type minuteState int

const (
	minuteUnknown minuteState = iota
	minuteFalse
	minuteTrue
)

// judge applies the window rule above.
func judge(states []minuteState) verdict {
	var trues, falses int
	for _, s := range states {
		switch s {
		case minuteTrue:
			trues++
		case minuteFalse:
			falses++
		}
	}
	switch {
	case len(states) > 0 && trues == len(states):
		return verdictFiring
	case trues == 0 && falses > 0:
		return verdictClear
	}
	return verdictUnknown
}

// deviceWindow is one device's newest device-minutes, by bucket.
type deviceWindow struct {
	newest time.Time
	byUnix map[int64]queries.DeviceMinute
}

// fresh reports whether the device is being recorded at now.
func (w *deviceWindow) fresh(now time.Time) bool {
	return w != nil && !w.newest.IsZero() && now.Sub(w.newest) <= loadFresh
}

// minutes returns the n newest minutes ending at the device's newest row,
// oldest first (a minute among them may have no row).
func (w *deviceWindow) minutes(n int) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = w.newest.Add(-time.Duration(n-1-i) * time.Minute)
	}
	return out
}

// loadWindows reads every device's recent device-minutes.
func loadWindows(db *sql.DB, now time.Time) (map[string]*deviceWindow, error) {
	rows, err := queries.ListDeviceMinutesSince(db, now.Add(-loadLookback).Truncate(time.Minute))
	if err != nil {
		return nil, err
	}
	out := map[string]*deviceWindow{}
	for _, m := range rows {
		w := out[m.DeviceID]
		if w == nil {
			w = &deviceWindow{byUnix: map[int64]queries.DeviceMinute{}}
			out[m.DeviceID] = w
		}
		w.byUnix[m.Bucket.Unix()] = m
		if m.Bucket.After(w.newest) {
			w.newest = m.Bucket
		}
	}
	return out, nil
}

// hold is the "cannot tell" observation for a device-keyed rule.
func hold(deviceID string) Observation {
	return Observation{Key: deviceID, DeviceID: deviceID, Hold: true}
}

func mibOf(bytes int64) int64 { return int64(math.Round(float64(bytes) / (1 << 20))) }

// ---- cpu_high ---------------------------------------------------------------

// cpuHigh fires while a device's quiet-window CPU stays high: at or above
// the class's warning threshold (C1 80 %, C2/C3 85 %) for 5 minutes
// (warning), at or above 95 % for 10 minutes (critical). It judges the minute
// averages of the samples taken with the device left alone, so the NMS's own
// read burst is not what trips it.
type cpuHigh struct{ db *sql.DB }

func (cpuHigh) Name() string { return RuleCPUHigh }

// cpuStates is whether each minute's quiet-window average reached pct.
func cpuStates(w *deviceWindow, minutes int, pct int) (states []minuteState, avg float64, peak int64) {
	var sum float64
	var known int
	for _, m := range w.minutes(minutes) {
		row, ok := w.byUnix[m.Unix()]
		v, sampled := row.CPUAvg()
		if !ok || !sampled {
			states = append(states, minuteUnknown)
			continue
		}
		sum += v
		known++
		peak = max(peak, row.CPUMax)
		if v >= float64(pct) {
			states = append(states, minuteTrue)
		} else {
			states = append(states, minuteFalse)
		}
	}
	if known > 0 {
		avg = sum / float64(known)
	}
	return states, avg, peak
}

func (r cpuHigh) Evaluate(now time.Time) ([]Observation, error) {
	devices, err := queries.ListDevices(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleCPUHigh, err)
	}
	windows, err := loadWindows(r.db, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleCPUHigh, err)
	}
	// What the diagnose pass found on the device while its CPU was high.
	causes, err := likelyCauses(r.db, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleCPUHigh, err)
	}
	var out []Observation
	for _, d := range devices {
		w := windows[d.ID]
		if !w.fresh(now) {
			out = append(out, hold(d.ID))
			continue
		}
		class := d.EffectiveClass()
		warnPct := cpuWarningPct(class)
		crit, critAvg, critPeak := cpuStates(w, cpuCriticalMinutes, cpuCriticalPct)
		warn, warnAvg, warnPeak := cpuStates(w, cpuWarningMinutes, warnPct)

		o := Observation{Key: d.ID, DeviceID: d.ID}
		switch {
		case judge(crit) == verdictFiring:
			o.Severity = SeverityCritical
			o.Summary = fmt.Sprintf("%s: CPU at or above %d %% for %d minutes (average %.0f %%)",
				deviceLabel(d), cpuCriticalPct, cpuCriticalMinutes, critAvg)
			o.Facts = map[string]any{"device": deviceLabel(d), "class": string(class), "threshold_pct": cpuCriticalPct,
				"minutes": cpuCriticalMinutes, "cpu_avg": math.Round(critAvg), "cpu_max": critPeak}
		case judge(warn) == verdictFiring:
			o.Severity = SeverityWarning
			o.Summary = fmt.Sprintf("%s: CPU at or above %d %% for %d minutes (average %.0f %%)",
				deviceLabel(d), warnPct, cpuWarningMinutes, warnAvg)
			o.Facts = map[string]any{"device": deviceLabel(d), "class": string(class), "threshold_pct": warnPct,
				"minutes": cpuWarningMinutes, "cpu_avg": math.Round(warnAvg), "cpu_max": warnPeak}
		case judge(warn) == verdictClear:
			continue // measured, and below the threshold throughout
		default:
			o = hold(d.ID)
		}
		if !o.Hold {
			causes[d.ID].attach(o.Facts)
		}
		out = append(out, o)
	}
	return out, nil
}

// ---- mem_low ----------------------------------------------------------------

// memLow fires while a device's free memory stays low for 3 minutes: below
// 5 % of its total, or — on a weak (C1) device — below 8 MiB.
type memLow struct{ db *sql.DB }

func (memLow) Name() string { return RuleMemLow }

// memLimit is the free-memory limit of a device in bytes and how to say it;
// ok is false when no limit can be told (total unknown on a device that has
// no absolute floor).
func memLimit(d queries.Device) (limit int64, text string, ok bool) {
	if d.MemoryTotal != nil && *d.MemoryTotal > 0 {
		limit = *d.MemoryTotal * memLowPct / 100
		text = fmt.Sprintf("%d %% of %d MiB", memLowPct, mibOf(*d.MemoryTotal))
		ok = true
	}
	if d.EffectiveClass().Budgeted() == devclass.C1 && memLowFloor > limit {
		return memLowFloor, fmt.Sprintf("%d MiB (weak device)", mibOf(memLowFloor)), true
	}
	return limit, text, ok
}

func (r memLow) Evaluate(now time.Time) ([]Observation, error) {
	devices, err := queries.ListDevices(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleMemLow, err)
	}
	windows, err := loadWindows(r.db, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleMemLow, err)
	}
	var out []Observation
	for _, d := range devices {
		w := windows[d.ID]
		limit, limitText, ok := memLimit(d)
		if !w.fresh(now) || !ok {
			out = append(out, hold(d.ID))
			continue
		}
		var states []minuteState
		lowest := int64(math.MaxInt64)
		for _, m := range w.minutes(memLowMinutes) {
			row, found := w.byUnix[m.Unix()]
			switch {
			case !found || !row.FreeMemoryMin.Valid:
				states = append(states, minuteUnknown)
			case row.FreeMemoryMin.Int64 < limit:
				states = append(states, minuteTrue)
				lowest = min(lowest, row.FreeMemoryMin.Int64)
			default:
				states = append(states, minuteFalse)
			}
		}
		switch judge(states) {
		case verdictFiring:
			out = append(out, Observation{
				Key: d.ID, DeviceID: d.ID, Severity: SeverityWarning,
				Summary: fmt.Sprintf("%s: %d MiB of memory free, below %s for %d minutes",
					deviceLabel(d), mibOf(lowest), limitText, memLowMinutes),
				Facts: map[string]any{"device": deviceLabel(d), "free_bytes": lowest, "limit_bytes": limit, "minutes": memLowMinutes},
			})
		case verdictClear:
		default:
			out = append(out, hold(d.ID))
		}
	}
	return out, nil
}

// ---- port_egress_drops ------------------------------------------------------

// portEgressDrops fires for a port that keeps dropping packets on egress:
// in each of 5 consecutive minutes at least 100 drops (tx-drop plus
// tx-queue-drop) that are at least 0.1 % of the packets it sent. One alert
// per port (key "<device>|<interface>").
type portEgressDrops struct{ db *sql.DB }

func (portEgressDrops) Name() string { return RulePortEgressDrops }

// egressDropping judges one stored port minute; known is false when the
// interface does not report either drop counter.
func egressDropping(row queries.PortStatRow) (dropping, known bool, drops int64) {
	if !row.TxDrop.Valid && !row.TxQueueDrop.Valid {
		return false, false, 0
	}
	drops = row.TxDrop.Int64 + row.TxQueueDrop.Int64
	// The row stores packets per second over the seconds it covers.
	secs := int64(60)
	if row.CoveredS.Valid && row.CoveredS.Int64 > 0 {
		secs = row.CoveredS.Int64
	}
	packets := row.TxPpsAvg * secs
	return drops >= egressDropMin && drops*1000 >= packets*egressDropPerMille, true, drops
}

func (r portEgressDrops) Evaluate(now time.Time) ([]Observation, error) {
	devices, err := queries.ListDevices(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RulePortEgressDrops, err)
	}
	windows, err := loadWindows(r.db, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RulePortEgressDrops, err)
	}
	// The port rows of every window that can still be judged: one that
	// ends at a device's newest minute, at most loadFresh old.
	from := now.Add(-loadFresh - egressDropMinutes*time.Minute).Truncate(time.Minute)
	rows, err := queries.ListPortEgressMinutes(r.db, from, now)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RulePortEgressDrops, err)
	}
	type portKey struct{ device, iface string }
	ports := map[portKey]map[int64]queries.PortStatRow{}
	for _, row := range rows {
		k := portKey{row.DeviceID, row.InterfaceName}
		if ports[k] == nil {
			ports[k] = map[int64]queries.PortStatRow{}
		}
		ports[k][row.Bucket.Unix()] = row
	}

	var out []Observation
	for _, d := range devices {
		w := windows[d.ID]
		if !w.fresh(now) {
			out = append(out, Observation{DeviceID: d.ID, Hold: true})
			continue
		}
		minutes := w.minutes(egressDropMinutes)
		covered := true
		for _, m := range minutes {
			if row, ok := w.byUnix[m.Unix()]; !ok || !row.Covered() {
				covered = false
				break
			}
		}
		if !covered {
			// A minute the device was not read in says nothing about any
			// of its ports.
			out = append(out, Observation{DeviceID: d.ID, Hold: true})
			continue
		}

		var names []string
		for k := range ports {
			if k.device == d.ID {
				names = append(names, k.iface)
			}
		}
		sort.Strings(names)
		for _, iface := range names {
			byMinute := ports[portKey{d.ID, iface}]
			var states []minuteState
			var worst, total int64
			candidate := false
			for _, m := range minutes {
				row, ok := byMinute[m.Unix()]
				if !ok {
					states = append(states, minuteFalse) // covered and no row: idle
					continue
				}
				dropping, known, drops := egressDropping(row)
				switch {
				case !known:
					states = append(states, minuteUnknown)
				case dropping:
					states = append(states, minuteTrue)
				default:
					states = append(states, minuteFalse)
				}
				if drops > 0 {
					candidate = true
				}
				worst, total = max(worst, drops), total+drops
			}
			if !candidate {
				continue // nothing dropped in the window: clear
			}
			key := d.ID + "|" + iface
			switch judge(states) {
			case verdictFiring:
				out = append(out, Observation{
					Key: key, DeviceID: d.ID, Severity: SeverityWarning,
					Summary: fmt.Sprintf("%s %s: dropping packets on egress for %d minutes (%d a minute on average, up to %d)",
						deviceLabel(d), iface, egressDropMinutes, total/egressDropMinutes, worst),
					Facts: map[string]any{"device": deviceLabel(d), "interface": iface, "minutes": egressDropMinutes,
						"drops_per_minute": total / egressDropMinutes, "drops_max": worst},
				})
			case verdictClear:
			default:
				out = append(out, Observation{Key: key, DeviceID: d.ID, Hold: true})
			}
		}
	}
	return out, nil
}
