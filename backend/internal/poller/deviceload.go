package poller

import (
	"database/sql"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Device load (design §2.5, P0-6): one device_minutes row per device and
// minute the NMS was polling — what the device's counters covered, what its
// CPU and memory did, and what the NMS itself sent it. "Is it us?" is then a
// chart: the quiet-window CPU next to the CPU right after a counter read and
// the commands of that minute.

const (
	// defaultLivenessSpan is how long a liveness result stands without a
	// newer one before the device's reachability counts as unknown. The
	// manager sets three health intervals (SetLivenessSpan).
	defaultLivenessSpan = 2 * time.Minute

	// maxLoadBackfill bounds how far back one flush writes rows. The flush
	// boundary normally advances a minute at a time; after a stall (a
	// suspended host, a clock step) the older minutes stay absent rather than
	// being written as hours of gaps in one transaction.
	maxLoadBackfill = 2 * time.Hour
)

// Retention of the device-load tables (design §2.5): minutes for two weeks —
// long enough to look back at an incident, and the hourly rollup re-reads at
// most a day of them — hours for two years.
const (
	deviceMinutesRetention = 14 * 24 * time.Hour
	deviceHoursRetention   = 730 * 24 * time.Hour
)

// sweepDeviceLoad applies that retention and removes the rows of devices
// that no longer exist (the tables carry no foreign key). Deletes run in
// bounded chunks, like the port stats'.
func sweepDeviceLoad(db *sql.DB, now time.Time) {
	if n, err := queries.DeleteOldDeviceLoad(db, queries.DeviceMinutes, now.Add(-deviceMinutesRetention)); err != nil {
		log.Printf("poller retention: device minutes: %v", err)
	} else if n > 0 {
		log.Printf("poller retention: deleted %d old device-minutes", n)
	}
	if n, err := queries.DeleteOldDeviceLoad(db, queries.DeviceHours, now.Add(-deviceHoursRetention)); err != nil {
		log.Printf("poller retention: device hours: %v", err)
	} else if n > 0 {
		log.Printf("poller retention: deleted %d old device-hours", n)
	}
	if n, err := queries.DeleteOrphanDeviceLoad(db); err != nil {
		log.Printf("poller retention: device load of deleted devices: %v", err)
	} else if n > 0 {
		log.Printf("poller retention: deleted %d device-load rows of deleted devices", n)
	}
}

// CPUReading is a device's latest quiet-window CPU sample.
type CPUReading struct {
	CPU int // percent
	At  time.Time
}

// loadMinute accumulates one device-minute.
type loadMinute struct {
	paused    bool
	liveKnown bool    // a liveness result applies to (part of) the minute
	reachable float64 // seconds a successful liveness result covers

	cpuSum, cpuSamples, cpuMax int64
	cpuPost                    sql.NullInt64
	freeMin                    sql.NullInt64
	uptime                     sql.NullInt64
	rebooted                   bool

	cmds, wallMs, rows, timeouts, reconnects int64
}

// loadDevice is what the recorder remembers of a device between minutes.
type loadDevice struct {
	lastEnd time.Time // when its last command ended (the quiet window counts from here)
	dialled bool      // a pooled session was opened since the start: the next one is a reconnect

	// The last liveness result, and how far its span has been accounted.
	liveAt       time.Time
	liveOK       bool
	liveCredited time.Time

	// The last uptime it reported; uptimeSeeded once the stored one was
	// looked up (so a reboot during an NMS restart is still noticed).
	uptime       int64
	uptimeKnown  bool
	uptimeSeeded bool

	latest    CPUReading
	hasLatest bool
}

// LoadRecorder aggregates, per device and minute, the NMS's own commands
// (it is the routeros.Observer), the liveness results, the admin pause and
// the CPU samples, and writes them — joined with the port-stats collector's
// Tier 0 coverage — as device_minutes rows when a minute becomes final
// (it is the collector's CoverageSink). Everything before that is in memory:
// the observer callbacks only take a mutex and add to counters. Safe for
// concurrent use.
type LoadRecorder struct {
	db *sql.DB

	flushMu sync.Mutex // one flush at a time

	mu             sync.Mutex
	started        time.Time // rows are written from this minute on
	liveSpan       time.Duration
	minutes        map[int64]map[string]*loadMinute // minute start (unix s) → device
	devs           map[string]*loadDevice
	flushedThrough time.Time
}

// NewLoadRecorder returns a recorder that writes to db from now on.
func NewLoadRecorder(db *sql.DB) *LoadRecorder {
	return &LoadRecorder{
		db:       db,
		started:  time.Now().UTC(),
		liveSpan: defaultLivenessSpan,
		minutes:  make(map[int64]map[string]*loadMinute),
		devs:     make(map[string]*loadDevice),
	}
}

// SetLivenessSpan sets how long a liveness result stands without a newer one
// (at least defaultLivenessSpan).
func (r *LoadRecorder) SetLivenessSpan(d time.Duration) {
	r.mu.Lock()
	r.liveSpan = max(d, defaultLivenessSpan)
	r.mu.Unlock()
}

// loadDeviceID maps a pool key to the device it belongs to: the verification
// login of an address move runs under "<id>:verify", and its commands load
// the same device. "" stays "" (a session tied to no device).
func loadDeviceID(poolKey string) string {
	id, _, _ := strings.Cut(poolKey, ":")
	return id
}

func (r *LoadRecorder) deviceLocked(id string) *loadDevice {
	d := r.devs[id]
	if d == nil {
		d = &loadDevice{}
		r.devs[id] = d
	}
	return d
}

// minuteLocked returns (creating) id's accumulator of the minute holding t.
// An event for a minute that is already written lands in the first open one.
func (r *LoadRecorder) minuteLocked(id string, t time.Time) *loadMinute {
	m := t.UTC().Truncate(time.Minute)
	if m.Before(r.flushedThrough) {
		m = r.flushedThrough
	}
	devs := r.minutes[m.Unix()]
	if devs == nil {
		devs = make(map[string]*loadMinute)
		r.minutes[m.Unix()] = devs
	}
	acc := devs[id]
	if acc == nil {
		acc = &loadMinute{}
		devs[id] = acc
	}
	return acc
}

// Command implements routeros.Observer: one finished command, counted in the
// minute it ended in.
func (r *LoadRecorder) Command(ev routeros.CommandEvent) {
	id := loadDeviceID(ev.DeviceID)
	if id == "" {
		return
	}
	end := ev.Start.Add(ev.Wall)
	r.mu.Lock()
	defer r.mu.Unlock()
	if d := r.deviceLocked(id); end.After(d.lastEnd) {
		d.lastEnd = end
	}
	acc := r.minuteLocked(id, end)
	acc.cmds++
	acc.wallMs += ev.Wall.Milliseconds()
	acc.rows += int64(ev.Rows)
	if ev.Timeout {
		acc.timeouts++
	}
}

// Session implements routeros.Observer: a pooled dial. A successful one
// after the first since the start is a reconnect.
func (r *LoadRecorder) Session(ev routeros.SessionEvent) {
	id := loadDeviceID(ev.DeviceID)
	if id == "" || ev.Err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deviceLocked(id)
	if d.dialled {
		r.minuteLocked(id, ev.At).reconnects++
	}
	d.dialled = true
}

// LastCommandEnd is when deviceID's last command ended (zero = none seen):
// the CPU sample waits for a quiet window after it.
func (r *LoadRecorder) LastCommandEnd(deviceID string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d := r.devs[deviceID]; d != nil {
		return d.lastEnd
	}
	return time.Time{}
}

// NotePaused records that an admin pause was in force for deviceID at t.
func (r *LoadRecorder) NotePaused(deviceID string, t time.Time) {
	r.mu.Lock()
	r.minuteLocked(deviceID, t).paused = true
	r.mu.Unlock()
}

// NoteLiveness records a liveness check of deviceID at t. A result stands
// until the next check (at most the liveness span): the time since the
// previous one is credited as reachable when that one succeeded.
func (r *LoadRecorder) NoteLiveness(deviceID string, t time.Time, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deviceLocked(deviceID)
	r.creditLivenessLocked(deviceID, d, t)
	d.liveAt, d.liveOK, d.liveCredited = t, ok, t
	r.minuteLocked(deviceID, t).liveKnown = true
}

// creditLivenessLocked accounts the device's current liveness result up to
// until: the minutes it spans know the device's state, and count the seconds
// as reachable when the result was a success. r.mu held.
func (r *LoadRecorder) creditLivenessLocked(id string, d *loadDevice, until time.Time) {
	if d.liveAt.IsZero() {
		return
	}
	from := maxTime(d.liveAt, d.liveCredited)
	to := minTime(until, d.liveAt.Add(r.liveSpan))
	if !to.After(from) {
		return
	}
	for m := from.Truncate(time.Minute); m.Before(to); m = m.Add(time.Minute) {
		overlap := minTime(to, m.Add(time.Minute)).Sub(maxTime(from, m))
		if overlap <= 0 || m.Before(r.flushedThrough) {
			continue
		}
		acc := r.minuteLocked(id, m)
		acc.liveKnown = true
		if d.liveOK {
			acc.reachable += overlap.Seconds()
		}
	}
	d.liveCredited = to
}

// AddSample records a quiet-window load sample of deviceID taken at t.
func (r *LoadRecorder) AddSample(deviceID string, t time.Time, s routeros.LoadSample) {
	r.seedUptime(deviceID)

	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.deviceLocked(deviceID)
	acc := r.minuteLocked(deviceID, t)
	cpu := int64(s.CPULoad)
	acc.cpuSum += cpu
	acc.cpuSamples++
	acc.cpuMax = max(acc.cpuMax, cpu)
	if s.HasFreeMemory && (!acc.freeMin.Valid || s.FreeMemory < acc.freeMin.Int64) {
		acc.freeMin = sql.NullInt64{Int64: s.FreeMemory, Valid: true}
	}
	if s.HasUptime {
		if d.uptimeKnown && s.UptimeSeconds < d.uptime {
			acc.rebooted = true
		}
		d.uptime, d.uptimeKnown = s.UptimeSeconds, true
		acc.uptime = sql.NullInt64{Int64: s.UptimeSeconds, Valid: true}
	}
	d.latest, d.hasLatest = CPUReading{CPU: s.CPULoad, At: t}, true
}

// seedUptime loads, once per device, the last uptime stored for it: the
// baseline that lets the first sample after an NMS restart show a reboot.
func (r *LoadRecorder) seedUptime(deviceID string) {
	r.mu.Lock()
	d := r.deviceLocked(deviceID)
	seeded := d.uptimeSeeded
	d.uptimeSeeded = true
	r.mu.Unlock()
	if seeded || r.db == nil {
		return
	}
	uptime, _, ok, err := queries.LastDeviceUptime(r.db, deviceID)
	if err != nil || !ok {
		return
	}
	r.mu.Lock()
	if d := r.deviceLocked(deviceID); !d.uptimeKnown {
		d.uptime, d.uptimeKnown = uptime, true
	}
	r.mu.Unlock()
}

// AddPostSample records the CPU load of deviceID read at t, 3 s after a
// Tier 0 read. A minute keeps the highest.
func (r *LoadRecorder) AddPostSample(deviceID string, t time.Time, cpu int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acc := r.minuteLocked(deviceID, t)
	if !acc.cpuPost.Valid || int64(cpu) > acc.cpuPost.Int64 {
		acc.cpuPost = sql.NullInt64{Int64: int64(cpu), Valid: true}
	}
}

// LatestCPU returns every device's latest quiet-window CPU sample (a copy).
// A nil recorder has none.
func (r *LoadRecorder) LatestCPU() map[string]CPUReading {
	out := map[string]CPUReading{}
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, d := range r.devs {
		if d.hasLatest {
			out[id] = d.latest
		}
	}
	return out
}

// FlushMinutes implements CoverageSink: the minutes before through are
// final, and every managed device gets its row for each of them — covered
// when coverage says a Tier 0 interval spans part of the minute, otherwise an
// explicit gap with its reason. Minutes are written once: a boundary that did
// not advance writes nothing. On a database error the minutes are logged and
// dropped, like the port rows of the same flush.
func (r *LoadRecorder) FlushMinutes(through time.Time, coverage []DeviceMinuteCoverage) {
	through = through.UTC().Truncate(time.Minute)
	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	r.mu.Lock()
	from := r.flushedThrough
	if from.IsZero() {
		from = r.started.UTC().Truncate(time.Minute)
	}
	r.mu.Unlock()
	if !through.After(from) {
		return
	}
	if through.Sub(from) > maxLoadBackfill {
		from = through.Add(-maxLoadBackfill)
	}

	devices, err := queries.ListDevices(r.db)
	if err != nil {
		log.Printf("poller load: list devices: %v", err)
	}

	type covKey struct {
		minute int64
		device string
	}
	cov := make(map[covKey]DeviceMinuteCoverage, len(coverage))
	for _, c := range coverage {
		cov[covKey{c.Minute.Unix(), c.DeviceID}] = c
	}

	r.mu.Lock()
	keep := make(map[string]bool, len(devices))
	for _, dev := range devices {
		keep[dev.ID] = true
		if d := r.devs[dev.ID]; d != nil {
			r.creditLivenessLocked(dev.ID, d, through)
		}
	}
	rows := make([]queries.DeviceMinute, 0, len(devices)*int(through.Sub(from)/time.Minute))
	for m := from; m.Before(through); m = m.Add(time.Minute) {
		accs := r.minutes[m.Unix()]
		for _, dev := range devices {
			rows = append(rows, deviceMinuteRow(dev.ID, m, accs[dev.ID], cov[covKey{m.Unix(), dev.ID}]))
		}
	}
	r.flushedThrough = through
	for unix := range r.minutes {
		if unix < through.Unix() {
			delete(r.minutes, unix)
		}
	}
	if err == nil {
		for id := range r.devs {
			if !keep[id] {
				delete(r.devs, id)
			}
		}
	}
	r.mu.Unlock()

	if err := queries.UpsertDeviceMinutes(r.db, rows); err != nil {
		log.Printf("poller load: write %d device-minutes: %v", len(rows), err)
	}
}

// deviceMinuteRow builds one row from what the recorder accumulated (acc, nil
// = nothing happened) and the collector's Tier 0 coverage of the minute.
func deviceMinuteRow(deviceID string, minute time.Time, acc *loadMinute, cov DeviceMinuteCoverage) queries.DeviceMinute {
	if acc == nil {
		acc = &loadMinute{}
	}
	row := queries.DeviceMinute{
		DeviceID:   deviceID,
		Bucket:     minute,
		PolledS:    wholeSeconds(cov.PolledSeconds),
		Tier0OK:    int64(cov.Reads),
		ReachableS: wholeSeconds(acc.reachable),
		Paused:     acc.paused,
		CPUSum:     acc.cpuSum, CPUSamples: acc.cpuSamples, CPUMax: acc.cpuMax,
		CPUPost: acc.cpuPost, FreeMemoryMin: acc.freeMin, UptimeS: acc.uptime, Rebooted: acc.rebooted,
		NMSCmds: acc.cmds, NMSWallMs: acc.wallMs, NMSRows: acc.rows,
		NMSTimeouts: acc.timeouts, NMSReconnects: acc.reconnects,
	}
	if row.PolledS > 0 {
		return row
	}
	switch {
	case acc.paused:
		row.GapReason = queries.GapPaused
	case acc.liveKnown && row.ReachableS == 0:
		row.GapReason = queries.GapUnreachable
	case cov.Failed > 0:
		row.GapReason = queries.GapReadFailed
	case cov.NoSession > 0:
		row.GapReason = queries.GapNoSession
	case cov.Reads > 0:
		row.GapReason = queries.GapNoBaseline
	default:
		row.GapReason = queries.GapNotPolled
	}
	return row
}

// wholeSeconds rounds the seconds of a minute to 0..60; any coverage at all
// counts as at least one second, so a sliver is never rounded into a gap.
func wholeSeconds(s float64) int64 {
	if s <= 0 {
		return 0
	}
	return min(max(int64(math.Round(s)), 1), 60)
}
