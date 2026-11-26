package api

import (
	"math"
	"net/http"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// GET /devices/{id}/load — one device's CPU and memory telemetry next to
// what the NMS itself sent it, from device_minutes (ranges up to 24 h) or
// device_hours (beyond). Database only: opening the view costs the device
// nothing.
//
// The point of the view is "is it us?": cpu_avg is the quiet-window sample
// (taken once the device had been left alone for 3 s), cpu_post the sample
// 3 s after an interface-counter read, and nms_* what the NMS asked of the
// device in the bucket. A bucket the device's counters did not cover carries
// the reason in gap and nulls for what was not measured — never zeros.

// loadRange is one selectable range of the load view.
type loadRange struct {
	key        string
	window     time.Duration
	step       time.Duration
	hourly     bool
	resolution string
}

var loadRanges = map[string]loadRange{
	"1h":  {key: "1h", window: time.Hour, step: time.Minute, resolution: "1m"},
	"6h":  {key: "6h", window: 6 * time.Hour, step: time.Minute, resolution: "1m"},
	"24h": {key: "24h", window: 24 * time.Hour, step: time.Minute, resolution: "1m"},
	"7d":  {key: "7d", window: 7 * 24 * time.Hour, step: time.Hour, hourly: true, resolution: "1h"},
	"30d": {key: "30d", window: 30 * 24 * time.Hour, step: time.Hour, hourly: true, resolution: "1h"},
}

// Gap values of a load point beyond the device_minutes reasons.
const (
	// loadGapNoData: there is no row for the bucket — the NMS was not
	// running, or the telemetry did not exist yet.
	loadGapNoData = "no_data"
	// loadGapNotCovered: an hourly bucket with a row but no covered second
	// (the hourly table does not keep the reason).
	loadGapNotCovered = "not_covered"
)

// deviceLoadPoint is one bucket. Every measurement is null when it was not
// taken; gap is null when the device's counters covered (part of) the bucket.
type deviceLoadPoint struct {
	TS       time.Time `json:"ts"`
	Gap      *string   `json:"gap"`
	CoveredS *int64    `json:"covered_s"`
	Paused   bool      `json:"paused"`
	Rebooted bool      `json:"rebooted"`
	// Quiet-window CPU: average and maximum of the bucket's samples; the
	// hourly resolution adds the p95 of the minute averages.
	CPUAvg *float64 `json:"cpu_avg"`
	CPUMax *int64   `json:"cpu_max"`
	CPUP95 *int64   `json:"cpu_p95"`
	// CPU 3 s after an interface-counter read (hourly: the average of the
	// hour's post-poll samples).
	CPUPost    *int64 `json:"cpu_post"`
	FreeMemory *int64 `json:"free_memory"` // lowest reading, bytes
	// What the NMS sent the device in the bucket.
	NMSCmds       *int64 `json:"nms_cmds"`
	NMSWallMs     *int64 `json:"nms_wall_ms"`
	NMSRows       *int64 `json:"nms_rows"`
	NMSTimeouts   *int64 `json:"nms_timeouts"`
	NMSReconnects *int64 `json:"nms_reconnects"`
}

// deviceLoadSummary condenses the range.
type deviceLoadSummary struct {
	CoveredPct float64  `json:"covered_pct"` // covered seconds of the window, %
	CPUAvg     *float64 `json:"cpu_avg"`     // quiet-window average
	CPUMax     *int64   `json:"cpu_max"`
	// A paired comparison over the buckets that have both samples: the
	// post-poll average, and the quiet-window average of those very buckets.
	// Their difference is what a counter read adds. (A post-poll sample in a
	// bucket without a quiet-window sample has nothing to be compared with
	// and is left out.)
	CPUPostAvg     *float64 `json:"cpu_post_avg"`
	CPUQuietAtPost *float64 `json:"cpu_quiet_at_post"`
	// Per recorded minute.
	NMSCmdsPerMin   *float64 `json:"nms_cmds_per_min"`
	NMSWallMsPerMin *float64 `json:"nms_wall_ms_per_min"`
	NMSTimeouts     int64    `json:"nms_timeouts"`
	NMSReconnects   int64    `json:"nms_reconnects"`
	Reboots         int64    `json:"reboots"` // buckets with a reboot marker
}

type deviceLoadResponse struct {
	DeviceID     string            `json:"device_id"`
	Range        string            `json:"range"`
	Resolution   string            `json:"resolution"` // 1m | 1h
	StepSeconds  int               `json:"step_seconds"`
	From         time.Time         `json:"from"`
	To           time.Time         `json:"to"`
	CoverageFrom *time.Time        `json:"coverage_from"` // first bucket with telemetry; null = none in the range
	MemoryTotal  *int64            `json:"memory_total"`
	Points       []deviceLoadPoint `json:"points"`
	Summary      deviceLoadSummary `json:"summary"`
}

func float64Ptr(v float64) *float64 { return &v }
func round1(v float64) float64      { return math.Round(v*10) / 10 }

// loadTotals accumulates the summary.
type loadTotals struct {
	covered                    int64
	cpuWeighted, cpuWeight     float64
	cpuMax                     int64
	hasCPU                     bool
	postSum, quietAtPost       float64
	posts                      int64
	cmds, cmdMinutes           int64
	wallMs, wallMinutes        int64
	timeouts, reconnects, boot int64
}

func (t *loadTotals) summary(window time.Duration) deviceLoadSummary {
	s := deviceLoadSummary{NMSTimeouts: t.timeouts, NMSReconnects: t.reconnects, Reboots: t.boot}
	if secs := window.Seconds(); secs > 0 {
		s.CoveredPct = round1(float64(t.covered) / secs * 100)
	}
	if t.hasCPU && t.cpuWeight > 0 {
		s.CPUAvg, s.CPUMax = float64Ptr(round1(t.cpuWeighted/t.cpuWeight)), int64Ptr(t.cpuMax)
	}
	if t.posts > 0 {
		s.CPUPostAvg = float64Ptr(round1(t.postSum / float64(t.posts)))
		s.CPUQuietAtPost = float64Ptr(round1(t.quietAtPost / float64(t.posts)))
	}
	if t.cmdMinutes > 0 {
		s.NMSCmdsPerMin = float64Ptr(round1(float64(t.cmds) / float64(t.cmdMinutes)))
	}
	if t.wallMinutes > 0 {
		s.NMSWallMsPerMin = float64Ptr(round1(float64(t.wallMs) / float64(t.wallMinutes)))
	}
	return s
}

// handleDeviceLoad — GET /devices/{id}/load?range=1h|6h|24h|7d|30d.
func (s *Server) handleDeviceLoad(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("range")
	if key == "" {
		key = "1h"
	}
	rng, ok := loadRanges[key]
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid range")
		return
	}
	d, ok := s.loadDevice(w, r.PathValue("id"))
	if !ok {
		return
	}

	// The same watermarks as the port charts: a device-minute is written
	// with the port rows of its minute, a device-hour with the hourly rollup.
	table := queries.PortStats1m
	if rng.hourly {
		table = queries.PortStats1h
	}
	end := s.windowEnd(table, time.Now())
	from := end.Add(-rng.window)
	resp := deviceLoadResponse{DeviceID: d.ID, Range: rng.key, Resolution: rng.resolution,
		StepSeconds: int(rng.step / time.Second), From: from, To: end, MemoryTotal: d.MemoryTotal}

	var totals loadTotals
	var err error
	if rng.hourly {
		resp.Points, err = s.loadHours(d.ID, from, end, &totals)
	} else {
		resp.Points, err = s.loadMinutes(d.ID, from, end, &totals)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read device load")
		return
	}
	for _, p := range resp.Points {
		if p.CoveredS != nil {
			resp.CoverageFrom = timePtr(p.TS)
			break
		}
	}
	resp.Summary = totals.summary(rng.window)
	writeJSON(w, http.StatusOK, resp)
}

// emptyLoadPoints is one explicit "no data" point per bucket of [from, to).
func emptyLoadPoints(from, to time.Time, step time.Duration) []deviceLoadPoint {
	n := int(to.Sub(from) / step)
	points := make([]deviceLoadPoint, n)
	for i := range points {
		points[i] = deviceLoadPoint{TS: from.Add(time.Duration(i) * step).UTC(), Gap: strPtr(loadGapNoData)}
	}
	return points
}

// loadMinutes builds the minute series of [from, to) from device_minutes.
func (s *Server) loadMinutes(deviceID string, from, to time.Time, t *loadTotals) ([]deviceLoadPoint, error) {
	rows, err := queries.ListDeviceMinutes(s.db, deviceID, from, to)
	if err != nil {
		return nil, err
	}
	points := emptyLoadPoints(from, to, time.Minute)
	for _, m := range rows {
		i := int(m.Bucket.Sub(from) / time.Minute)
		if i < 0 || i >= len(points) {
			continue
		}
		p := &points[i]
		p.Gap = nil
		if !m.Covered() {
			p.Gap = strPtr(m.GapReason)
		}
		p.CoveredS, p.Paused, p.Rebooted = int64Ptr(m.PolledS), m.Paused, m.Rebooted
		if avg, ok := m.CPUAvg(); ok {
			p.CPUAvg, p.CPUMax = float64Ptr(round1(avg)), int64Ptr(m.CPUMax)
			t.cpuWeighted += float64(m.CPUSum)
			t.cpuWeight += float64(m.CPUSamples)
			t.cpuMax, t.hasCPU = max(t.cpuMax, m.CPUMax), true
			if m.CPUPost.Valid {
				t.postSum += float64(m.CPUPost.Int64)
				t.quietAtPost += avg
				t.posts++
			}
		}
		if m.CPUPost.Valid {
			p.CPUPost = int64Ptr(m.CPUPost.Int64)
		}
		if m.FreeMemoryMin.Valid {
			p.FreeMemory = int64Ptr(m.FreeMemoryMin.Int64)
		}
		p.NMSCmds, p.NMSWallMs, p.NMSRows = int64Ptr(m.NMSCmds), int64Ptr(m.NMSWallMs), int64Ptr(m.NMSRows)
		p.NMSTimeouts, p.NMSReconnects = int64Ptr(m.NMSTimeouts), int64Ptr(m.NMSReconnects)

		t.covered += m.PolledS
		t.cmds, t.cmdMinutes = t.cmds+m.NMSCmds, t.cmdMinutes+1
		t.wallMs, t.wallMinutes = t.wallMs+m.NMSWallMs, t.wallMinutes+1
		t.timeouts += m.NMSTimeouts
		t.reconnects += m.NMSReconnects
		if m.Rebooted {
			t.boot++
		}
	}
	return points, nil
}

// loadHours builds the hourly series of [from, to) from device_hours, with
// the figures only the minute rows hold (reboot markers, post-poll CPU, wall
// time, reply rows, timeouts, reconnects) for the hours they still cover.
func (s *Server) loadHours(deviceID string, from, to time.Time, t *loadTotals) ([]deviceLoadPoint, error) {
	rows, err := queries.ListDeviceHours(s.db, deviceID, from, to)
	if err != nil {
		return nil, err
	}
	extras, err := queries.ListDeviceHourExtras(s.db, deviceID, from, to)
	if err != nil {
		return nil, err
	}
	points := emptyLoadPoints(from, to, time.Hour)
	for _, h := range rows {
		i := int(h.Bucket.Sub(from) / time.Hour)
		if i < 0 || i >= len(points) {
			continue
		}
		p := &points[i]
		p.Gap = nil
		if h.CoveredS <= 0 {
			p.Gap = strPtr(loadGapNotCovered)
		}
		p.CoveredS, p.NMSCmds = int64Ptr(h.CoveredS), int64Ptr(h.NMSCmds)
		if h.CPUAvg.Valid {
			p.CPUAvg = float64Ptr(round1(h.CPUAvg.Float64))
			weight := float64(max(h.CoveredS, 1))
			t.cpuWeighted += h.CPUAvg.Float64 * weight
			t.cpuWeight += weight
			t.hasCPU = true
		}
		if h.CPUMax.Valid {
			p.CPUMax = int64Ptr(h.CPUMax.Int64)
			t.cpuMax = max(t.cpuMax, h.CPUMax.Int64)
		}
		if h.CPUP95.Valid {
			p.CPUP95 = int64Ptr(h.CPUP95.Int64)
		}
		if h.MemFreeMin.Valid {
			p.FreeMemory = int64Ptr(h.MemFreeMin.Int64)
		}
		t.covered += h.CoveredS
		t.cmds, t.cmdMinutes = t.cmds+h.NMSCmds, t.cmdMinutes+60

		x, ok := extras[h.Bucket.Unix()]
		if !ok {
			continue
		}
		p.Rebooted = x.Rebooted
		p.NMSWallMs, p.NMSRows = int64Ptr(x.WallMs), int64Ptr(x.Rows)
		p.NMSTimeouts, p.NMSReconnects = int64Ptr(x.Timeouts), int64Ptr(x.Reconnects)
		if x.CPUPost.Valid {
			post := int64(math.Round(x.CPUPost.Float64))
			p.CPUPost = int64Ptr(post)
			if h.CPUAvg.Valid {
				t.postSum += x.CPUPost.Float64
				t.quietAtPost += h.CPUAvg.Float64
				t.posts++
			}
		}
		t.wallMs, t.wallMinutes = t.wallMs+x.WallMs, t.wallMinutes+60
		t.timeouts += x.Timeouts
		t.reconnects += x.Reconnects
		if x.Rebooted {
			t.boot++
		}
	}
	return points, nil
}
