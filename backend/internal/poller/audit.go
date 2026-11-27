package poller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/alerts"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/hygiene"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// The hygiene audit and the diagnose pass (design §2.7, §9.6; Appendix B
// Tier 7 and 7d).
//
// The audit reads a device's configuration every 6 hours and reports the
// settings known to cost CPU or to be unsafe. The diagnose pass makes the
// shorter list of reads that cover the usual device-side causes at the
// moment a device's CPU is high — exactly when the audit stays away — so the
// alert can name a likely cause. Both only read: every command is a print
// with a .proplist (or =count-only=) on the device's pooled session, and
// nothing is ever changed on a device.
//
// What a device is sent, and how fast, is the point:
//
//   - one command at a time, spaced by the device's class: an audit read
//     every 20 s on a weak (C1) device, a diagnose read every 30 s;
//   - a scheduled audit is not started — and one that runs steps aside —
//     while the device's last quiet-window CPU sample is at or above 80 %,
//     while it is paused, and without a pooled session; it is tried again,
//     never dropped;
//   - one audit and one diagnose pass per device per hour at most, whoever
//     asks, counted from the stored passes so a restart does not reset it.
const (
	// auditCPULimit is the quiet-window CPU (percent) at and above which a
	// scheduled audit stays away from a device.
	auditCPULimit = 80
	// auditCPUFresh is how old the CPU sample may be to be believed; without
	// a fresh one the audit waits (the sampler delivers one a minute).
	auditCPUFresh = 5 * time.Minute
	// Retries of a scheduled audit that could not start, or stopped early.
	auditBusyRetry     = 10 * time.Minute // the CPU was high
	auditNoSampleRetry = time.Minute      // no fresh CPU sample yet
	auditHoldRetry     = time.Minute      // the CPU went up mid-pass: look again
	auditPartialRetry  = time.Hour        // a read failed: the audit is run again

	// PassBudget is the least time between two passes of one kind on one
	// device, whoever asks for them.
	PassBudget = time.Hour

	// A pass that cannot go on is ended: without a session for
	// passNoSessionLimit, or passMaxDuration after it began.
	passNoSessionLimit = 5 * time.Minute
	passMaxDuration    = 45 * time.Minute

	// After a start the first audits wait for the pool and the CPU sampler,
	// then spread over auditStartupSpread by the device's phase.
	auditLoopDelay     = 15 * time.Second
	auditStartupDelay  = 2 * time.Minute
	auditStartupSpread = 20 * time.Minute

	// alertScanEvery is how often the open alerts are read for the ones that
	// start a diagnose pass.
	alertScanEvery = 30 * time.Second

	// flowBaselineWindow is the span the CPU baseline of a flow-export watch
	// is taken over, and flowBaselineMinMinutes how many sampled minutes it
	// needs to count.
	flowBaselineWindow     = 24 * time.Hour
	flowBaselineMinMinutes = 30
)

// Retention of the audit tables: passes for 30 days, superseded snapshots
// for a year (a device's newest one is always kept), flow-export watches
// for a week.
const (
	auditRunRetention      = 30 * 24 * time.Hour
	auditSnapshotRetention = 365 * 24 * time.Hour
	flowWatchRetention     = 7 * 24 * time.Hour
)

// Reasons a pass runs for (device_audit_runs.reason); a diagnose pass an
// alert started carries the alert's rule name.
const (
	PassScheduled = "scheduled"
	PassAdmin     = "admin"
)

// Errors of an admin request.
var (
	// ErrPassRunning: a pass is already running on the device.
	ErrPassRunning = errors.New("a pass is already running on this device")
	// ErrPassNoSession: the device is not online, or the pool holds no
	// session for it.
	ErrPassNoSession = errors.New("the device is not reachable over the API right now")
)

// PassPausedError refuses a pass on a device whose polling is paused.
type PassPausedError struct{ Until time.Time }

func (e *PassPausedError) Error() string {
	return "polling paused until " + e.Until.UTC().Format(time.RFC3339)
}

// PassBudgetError refuses a pass that would be the second of its kind on the
// device within PassBudget; Next is when the next one may start.
type PassBudgetError struct {
	Kind string
	Next time.Time
}

func (e *PassBudgetError) Error() string {
	what := "audit"
	if e.Kind == queries.AuditKindDiagnose {
		what = "diagnose pass"
	}
	return fmt.Sprintf("the last %s of this device started less than an hour ago; the next one may start at %s",
		what, e.Next.UTC().Format("15:04 UTC"))
}

// passGap is the pause after each read of a pass, by the class the device is
// budgeted as.
func passGap(class devclass.Class, kind string) time.Duration {
	diagnose := kind == queries.AuditKindDiagnose
	switch class.Budgeted() {
	case devclass.C1:
		if diagnose {
			return 30 * time.Second
		}
		return 20 * time.Second
	case devclass.C2:
		if diagnose {
			return 10 * time.Second
		}
		return 5 * time.Second
	}
	return 2 * time.Second
}

// passRequest is why a pass runs.
type passRequest struct {
	kind    string // queries.AuditKindAudit | AuditKindDiagnose
	reason  string // PassScheduled | PassAdmin | the alert's rule
	alertID int64
	by      string
}

// pass is one audit or diagnose pass in progress. Guarded by Auditor.mu.
type pass struct {
	req      passRequest
	deviceID string
	runID    int64
	facts    hygiene.Facts
	plan     []hygiene.Read
	idx      int // the next read of plan
	sections hygiene.Sections
	reads    int
	failed   int
	gap      time.Duration

	startedAt time.Time
	nextAt    time.Time // no read before this
	noSession time.Time // since when the device has had no session (zero = it has one)
	busy      bool      // a read is in flight, or the pass is being set up
	waiting   bool      // a scheduled audit that stepped aside for a high CPU
}

func (p *pass) mode() hygiene.Mode {
	if p.req.kind == queries.AuditKindDiagnose {
		return hygiene.ModeDiagnose
	}
	return hygiene.ModeAudit
}

// acceptedExporter is one call of the flow collector's accept hook.
type acceptedExporter struct {
	exporter queries.FlowExporter
	at       time.Time
}

// Auditor runs the scheduled audits, the diagnose passes the alerts ask for
// and the passes an admin requests, and starts the CPU watch of a device
// whose flow exporter was accepted.
type Auditor struct {
	db   *sql.DB
	pool *routeros.Pool

	cadence CadenceSource
	clock   *tierClock
	// collectorPort is the flow collector's listen port (0 = none).
	collectorPort int

	// Seams: the clock, the device link (nil = no pooled session), the
	// device's latest quiet-window CPU sample, its interface snapshot and
	// port rates, its capabilities, the addresses the NMS's sessions to it
	// come from, how a read is run (a goroutine; tests run it inline), the
	// startup spread, the polling fence, and the role lookup (nil = from
	// the database).
	now            func() time.Time
	roles          func(now time.Time) (map[string]topology.DeviceRole, error)
	runner         func(deviceID string) routeros.Runner
	cpu            func(deviceID string) (CPUReading, bool)
	ifaces         func(deviceID string) (DeviceIfaces, bool)
	rates          func(deviceID string) map[string]int64
	capsOf         func(deviceID string) (Caps, bool)
	self           func(dev *queries.Device, now time.Time) []netip.Addr
	spawn          func(func())
	spread         func(deviceID string) time.Duration
	pollingAllowed func() error

	kick     chan struct{}         // an admin asked: look now
	accepted chan acceptedExporter // the flow collector's accept hook

	mu        sync.Mutex
	started   time.Time            // zero until the first tick
	passes    map[string]*pass     // device → the pass in progress
	seeded    map[string]bool      // devices whose audit schedule was set up
	lastAudit map[string]time.Time // when each device last finished an audit (from the database, at start)
	triggers  map[string]queries.Alert
	scannedAt time.Time
	lastLog   map[string]time.Time
}

// NewAuditor wires the auditor; Run starts it. load supplies the CPU
// samples, portStats the interface snapshot and rates, caps the capability
// probes; any of them may be nil (the facts they would give stay unknown).
func NewAuditor(db *sql.DB, pool *routeros.Pool, load *LoadRecorder, portStats *PortStatsCollector, caps *CapabilityCache) *Auditor {
	a := &Auditor{
		db: db, pool: pool,
		cadence:        newFleetCadence(db),
		clock:          newTierClock(Schedule()),
		now:            time.Now,
		spawn:          func(f func()) { go f() },
		pollingAllowed: routeros.PollingAllowed,
		kick:           make(chan struct{}, 1),
		accepted:       make(chan acceptedExporter, 32),
		passes:         make(map[string]*pass),
		seeded:         make(map[string]bool),
		lastAudit:      make(map[string]time.Time),
		lastLog:        make(map[string]time.Time),
	}
	a.runner = func(id string) routeros.Runner { return poolRunner(pool, id) }
	a.cpu = load.CPUOf
	a.ifaces = portStats.DeviceIfaces
	a.rates = portStats.DeviceRates
	a.capsOf = func(id string) (Caps, bool) {
		if caps == nil {
			return Caps{}, false
		}
		return caps.Get(id)
	}
	watch := NewLoginWatch(db)
	a.self = watch.SelfAddresses
	a.spread = func(id string) time.Duration {
		return time.Duration(devclass.Phase(id)%uint64(auditStartupSpread/time.Second)) * time.Second
	}
	return a
}

// UseCadence schedules the audit by cs (the per-class plan and the pause).
// Call before Run.
func (a *Auditor) UseCadence(cs CadenceSource) {
	if cs != nil {
		a.cadence = cs
	}
}

// UseCollectorPort tells the auditor the flow collector's listen port, for
// the rollback text of its findings and alerts. Call before Run.
func (a *Auditor) UseCollectorPort(port int) { a.collectorPort = port }

// NoteExporterAccepted is the flow collector's accept hook
// (flow.Collector.UseAcceptHook): the collector started accepting an
// exporter, and the CPU watch of its device begins (G6). It never blocks —
// a full queue drops the watch rather than hold the collector's worker up.
func (a *Auditor) NoteExporterAccepted(e queries.FlowExporter, at time.Time) {
	select {
	case a.accepted <- acceptedExporter{exporter: e, at: at}:
	default:
		log.Printf("poller audit: the flow-export watch of %s could not be queued", e.Address)
	}
}

// Run drives the loop until ctx is done.
func (a *Auditor) Run(ctx context.Context) {
	if !sleepCtx(ctx, auditLoopDelay) {
		return
	}
	for {
		now := a.now()
		wake := a.safeTick(now)
		wait := min(max(wake.Sub(now), 200*time.Millisecond), pollerTick)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.kick:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (a *Auditor) safeTick(now time.Time) (wake time.Time) {
	wake = now.Add(pollerTick)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller audit: panic in tick: %v\n%s", r, debug.Stack())
		}
	}()
	return a.tick(now)
}

// begun runs once, on the first tick: nothing is running after a restart,
// and the schedule continues from the audits the database remembers.
func (a *Auditor) begun(now time.Time) {
	a.mu.Lock()
	first := a.started.IsZero()
	if first {
		a.started = now
	}
	a.mu.Unlock()
	if !first || a.db == nil {
		return
	}
	if n, err := queries.AbortRunningAuditRuns(a.db, now, "the NMS restarted during the pass"); err != nil {
		log.Printf("poller audit: close the passes of the last run: %v", err)
	} else if n > 0 {
		log.Printf("poller audit: %d pass(es) were cut off by the restart", n)
	}
	last, err := queries.ListLastAuditTimes(a.db)
	if err != nil {
		log.Printf("poller audit: read the last audits: %v", err)
		return
	}
	a.mu.Lock()
	a.lastAudit = last
	a.mu.Unlock()
}

// tick advances every pass by at most one read, starts the passes that are
// due, and returns when the loop should look again.
func (a *Auditor) tick(now time.Time) time.Time {
	a.begun(now)
	a.startWatches(now)

	wake := now.Add(pollerTick)
	devices, err := queries.ListDevices(a.db)
	if err != nil {
		log.Printf("poller audit: list devices: %v", err)
		return wake
	}
	a.retain(devices)
	triggers := a.diagnoseTriggers(devices, now)
	for _, dev := range devices {
		trigger, asked := triggers[dev.ID]
		var alert *queries.Alert
		if asked {
			alert = &trigger
		}
		if t := a.tickDevice(dev, alert, now); !t.IsZero() && t.Before(wake) {
			wake = t
		}
	}
	return wake
}

// retain forgets the devices that no longer exist.
func (a *Auditor) retain(devices []queries.Device) {
	keep := deviceIDs(devices)
	a.clock.retain(keep)
	a.mu.Lock()
	for id := range a.seeded {
		if !keep[id] {
			delete(a.seeded, id)
			delete(a.lastAudit, id)
		}
	}
	for id := range a.passes {
		if !keep[id] {
			delete(a.passes, id) // its run row went with the device
		}
	}
	a.mu.Unlock()
}

// tickDevice is one device's share of a tick. It returns when the device
// needs to be looked at again (zero = at the next regular tick).
func (a *Auditor) tickDevice(dev queries.Device, trigger *queries.Alert, now time.Time) time.Time {
	online := dev.Status == "online"
	paused := a.cadence.Paused(dev, now)

	// A diagnose pass for an open alert: also — above all — while the CPU
	// is high. A scheduled audit that stepped aside because of it must not
	// keep that pass waiting: it ends with what it has, and makes way.
	if running, waiting := a.inProgress(dev.ID); trigger != nil && (running == "" || waiting) &&
		online && !paused && a.runner(dev.ID) != nil {
		// The alert was owed a pass when the alerts were last read, up to
		// alertScanEvery ago. Whether it still is — an admin's "Diagnose
		// now" may have run since — is asked again here, right before the
		// pass begins: the hourly budget is the device's, whoever asks.
		if !a.owed(*trigger, now) {
			a.dropTrigger(dev.ID)
		} else if a.endWaitingAudit(dev, now) {
			if p, err := a.begin(dev, passRequest{kind: queries.AuditKindDiagnose, reason: trigger.Rule, alertID: trigger.ID}, now); err == nil {
				a.dropTrigger(dev.ID)
				return a.advance(p, dev, now)
			}
		}
	}
	a.mu.Lock()
	p := a.passes[dev.ID]
	a.mu.Unlock()
	if p != nil {
		return a.advance(p, dev, now)
	}

	// The scheduled audit.
	var interval time.Duration
	if online {
		interval = runInterval(a.cadence, dev, devclass.TierAudit, now) // 0 while paused
	}
	if interval > 0 {
		a.seed(dev, interval)
	}
	if !a.clock.due(dev.ID, devclass.TierAudit, interval, now) {
		return time.Time{}
	}
	retry := func(d time.Duration) time.Time {
		a.clock.retryAt(dev.ID, devclass.TierAudit, now.Add(d))
		return time.Time{}
	}
	if next, free := a.budget(dev.ID, queries.AuditKindAudit, now); !free {
		a.clock.retryAt(dev.ID, devclass.TierAudit, next)
		return time.Time{}
	}
	if a.runner(dev.ID) == nil {
		return retry(noClientRetry)
	}
	sample, ok := a.cpu(dev.ID)
	switch {
	case !ok || now.Sub(sample.At) > auditCPUFresh:
		return retry(auditNoSampleRetry)
	case sample.CPU >= auditCPULimit:
		a.logThrottled("busy:"+dev.ID, now, "poller audit: %s is at %d %% CPU — its audit waits (the diagnose pass covers a busy device)",
			nonempty(dev.Identity, dev.Address), sample.CPU)
		return retry(auditBusyRetry)
	}
	p, err := a.begin(dev, passRequest{kind: queries.AuditKindAudit, reason: PassScheduled}, now)
	if err != nil {
		log.Printf("poller audit: start the audit of %s: %v", nonempty(dev.Identity, dev.Address), err)
		return retry(auditNoSampleRetry)
	}
	return a.advance(p, dev, now)
}

// seed sets a device's first audit of this process: six hours after its last
// one when the database remembers it, and never before the start-up delay
// plus the device's share of the spread — a restart must not audit the whole
// fleet at once, nor each device again.
func (a *Auditor) seed(dev queries.Device, interval time.Duration) {
	a.mu.Lock()
	if a.seeded[dev.ID] {
		a.mu.Unlock()
		return
	}
	a.seeded[dev.ID] = true
	last, known := a.lastAudit[dev.ID]
	started := a.started
	a.mu.Unlock()

	at := started.Add(auditStartupDelay + a.spread(dev.ID))
	if known {
		if next := devclass.NextRun(last, interval, a.clock.phase(dev.ID)); next.After(at) {
			at = next
		}
	}
	a.clock.retryAt(dev.ID, devclass.TierAudit, at)
}

// budget reports whether a pass of kind may start on the device at now, and
// otherwise when: PassBudget after the last one started, whoever asked for
// it and however it ended.
func (a *Auditor) budget(deviceID, kind string, now time.Time) (next time.Time, free bool) {
	last, err := queries.LatestAuditRun(a.db, deviceID, kind)
	if err != nil || last == nil {
		return now, true
	}
	if next = last.StartedAt.Add(PassBudget); now.Before(next) {
		return next, false
	}
	return now, true
}

// NextAllowed is when the next pass of kind may start on the device (now or
// earlier = at once).
func (a *Auditor) NextAllowed(deviceID, kind string, now time.Time) time.Time {
	next, _ := a.budget(deviceID, kind, now)
	return next
}

// Running reports the kind of pass in progress on the device ("" = none).
func (a *Auditor) Running(deviceID string) string {
	kind, _ := a.inProgress(deviceID)
	return kind
}

// inProgress reports the kind of pass in progress on the device ("" = none)
// and whether it is a scheduled audit that stepped aside for a high CPU and
// is doing nothing but wait.
func (a *Auditor) inProgress(deviceID string) (kind string, waiting bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.passes[deviceID]
	if p == nil {
		return "", false
	}
	return p.req.kind, p.waiting && !p.busy
}

// endWaitingAudit ends a scheduled audit that is waiting for the device's
// CPU to drop, so that the diagnose pass — the one that reads a busy device
// — can run: the audit ends with what it has read. It reports whether the
// device is free now: true when there was no pass or the waiting audit was
// ended, false when a pass is in the way that is not just waiting.
func (a *Auditor) endWaitingAudit(dev queries.Device, now time.Time) bool {
	a.mu.Lock()
	p := a.passes[dev.ID]
	if p == nil {
		a.mu.Unlock()
		return true
	}
	if !p.waiting || p.busy {
		a.mu.Unlock()
		return false
	}
	p.busy = true // claimed: the loop leaves it alone until it is finished
	a.mu.Unlock()
	a.finish(p, dev, "ended for the diagnose pass: the CPU stayed high", false, now)
	return true
}

// Request starts the pass an admin asked for — "Audit now" or "Diagnose
// now" — and returns its run. Unlike a scheduled audit it runs whatever the
// device's CPU says; it is refused while polling is off (routeros.ErrPollersOff),
// while the device is paused (*PassPausedError), without a session
// (ErrPassNoSession), while another pass runs (ErrPassRunning) and within an
// hour of the device's last pass of that kind (*PassBudgetError). A
// scheduled audit that is only waiting for the device's CPU to drop is not
// "another pass running" for a diagnose request: it is ended with what it
// has read, and the diagnose pass begins.
func (a *Auditor) Request(deviceID, kind, user string) (*queries.AuditRun, error) {
	if kind != queries.AuditKindAudit && kind != queries.AuditKindDiagnose {
		return nil, fmt.Errorf("unknown kind of pass %q", kind)
	}
	if err := a.pollingAllowed(); err != nil {
		return nil, err
	}
	dev, err := queries.GetDevice(a.db, deviceID)
	if err != nil {
		return nil, err
	}
	now := a.now()
	if a.cadence.Paused(*dev, now) && dev.PollingPausedUntil != nil {
		return nil, &PassPausedError{Until: *dev.PollingPausedUntil}
	}
	// A scheduled audit that stepped aside for a high CPU holds the device
	// until the CPU drops. The diagnose pass is the one that reads a busy
	// device: a request for it does not wait for that audit, it ends it —
	// once nothing else stands in the request's way.
	running, waiting := a.inProgress(deviceID)
	yields := waiting && kind == queries.AuditKindDiagnose
	if running != "" && !yields {
		return nil, ErrPassRunning
	}
	if next, free := a.budget(deviceID, kind, now); !free {
		return nil, &PassBudgetError{Kind: kind, Next: next}
	}
	if dev.Status != "online" || a.runner(deviceID) == nil {
		return nil, ErrPassNoSession
	}
	if yields && !a.endWaitingAudit(*dev, now) {
		return nil, ErrPassRunning // the audit went on meanwhile
	}
	a.begun(now)
	p, err := a.begin(*dev, passRequest{kind: kind, reason: PassAdmin, by: user}, now)
	if err != nil {
		return nil, err
	}
	select {
	case a.kick <- struct{}{}:
	default:
	}
	return &queries.AuditRun{ID: p.runID, DeviceID: deviceID, Kind: kind, Reason: PassAdmin, RequestedBy: user,
		StartedAt: now.UTC().Truncate(time.Second), Status: queries.AuditRunning}, nil
}

// begin sets a pass up: the facts the NMS has about the device, the plan,
// the pace, and its row. The first read is the next advance's.
func (a *Auditor) begin(dev queries.Device, req passRequest, now time.Time) (*pass, error) {
	p := &pass{req: req, deviceID: dev.ID, busy: true, startedAt: now, nextAt: now, sections: hygiene.Sections{}}
	a.mu.Lock()
	if a.passes[dev.ID] != nil {
		a.mu.Unlock()
		return nil, ErrPassRunning
	}
	a.passes[dev.ID] = p
	a.mu.Unlock()

	facts := a.facts(dev, now)
	id, err := queries.InsertAuditRun(a.db, &queries.AuditRun{DeviceID: dev.ID, Kind: req.kind, Reason: req.reason,
		AlertID: req.alertID, RequestedBy: req.by, StartedAt: now})

	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		delete(a.passes, dev.ID)
		return nil, err
	}
	p.runID, p.facts = id, facts
	p.plan = hygiene.Plan(p.mode())
	p.gap = passGap(liveClassOf(&dev), req.kind)
	p.busy = false
	return p, nil
}

// advance gives a pass its next step when its time has come: at most one
// read. It returns when the pass wants to be looked at again.
func (a *Auditor) advance(p *pass, dev queries.Device, now time.Time) time.Time {
	a.mu.Lock()
	if p.busy {
		a.mu.Unlock()
		return now.Add(time.Second)
	}
	switch {
	case a.cadence.Paused(dev, now):
		a.mu.Unlock()
		a.finish(p, dev, "polling of the device was paused during the pass", true, now)
		return time.Time{}
	case now.Sub(p.startedAt) > passMaxDuration:
		a.mu.Unlock()
		a.finish(p, dev, "the pass was ended after "+passMaxDuration.String()+" without finishing", false, now)
		return time.Time{}
	case now.Before(p.nextAt):
		at := p.nextAt
		a.mu.Unlock()
		return at
	}
	// A scheduled audit steps aside while the CPU is high: the reads it has
	// made stand, the rest waits. (An admin's audit and every diagnose pass
	// go on — that is what they are for.)
	if p.req.reason == PassScheduled {
		if s, ok := a.cpu(dev.ID); ok && now.Sub(s.At) <= auditCPUFresh && s.CPU >= auditCPULimit {
			p.waiting, p.nextAt = true, now.Add(auditHoldRetry)
			at := p.nextAt
			a.mu.Unlock()
			return at
		}
	}
	p.waiting = false
	read := a.nextReadLocked(p)
	if read == nil {
		a.mu.Unlock()
		a.finish(p, dev, "", false, now)
		return time.Time{}
	}
	p.busy = true
	a.mu.Unlock()

	a.spawn(func() { a.step(p, dev, *read) })

	a.mu.Lock()
	defer a.mu.Unlock()
	if p.busy {
		return now.Add(time.Second)
	}
	return p.nextAt
}

// nextReadLocked returns the next read of the pass that applies to its
// device, or nil when the plan is through. The reads that do not apply
// (Read.When) are recorded as skipped: they cost the device nothing. a.mu
// held.
func (a *Auditor) nextReadLocked(p *pass) *hygiene.Read {
	for p.idx < len(p.plan) {
		r := p.plan[p.idx]
		if r.When == nil || r.When(p.facts, p.sections) {
			return &r
		}
		p.sections[r.Key] = hygiene.Section{State: hygiene.StateSkipped}
		p.idx++
	}
	return nil
}

// step sends one read and records the answer; after the last read of the
// plan the pass is finished at once.
func (a *Auditor) step(p *pass, dev queries.Device, read hygiene.Read) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller audit: panic reading %s from %s: %v\n%s", read.Command, nonempty(dev.Identity, dev.Address), r, debug.Stack())
			a.mu.Lock()
			p.busy, p.nextAt = false, a.now().Add(p.gap)
			a.mu.Unlock()
		}
	}()
	run := a.runner(dev.ID)
	if run == nil {
		// No session: the pool redials on its own schedule; the pass waits
		// for it, but not for ever.
		now := a.now()
		a.mu.Lock()
		if p.noSession.IsZero() {
			p.noSession = now
		}
		gone := now.Sub(p.noSession) >= passNoSessionLimit
		p.busy, p.nextAt = false, now.Add(noClientRetry)
		a.mu.Unlock()
		if gone {
			a.finish(p, dev, "the device had no API session for "+passNoSessionLimit.String(), false, now)
		}
		return
	}
	reply := routeros.ReadConfig(run, read.ConfigRead)
	end := a.now()
	through := func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		p.noSession = time.Time{}
		p.sections[read.Key] = hygiene.SectionOf(read, reply)
		p.reads++
		if reply.State == routeros.ConfigFailed {
			p.failed++
		}
		p.idx++
		p.busy, p.nextAt = false, end.Add(p.gap)
		return a.nextReadLocked(p) == nil
	}()
	if through {
		a.finish(p, dev, "", false, end)
	}
}

// storedFinding is what a pass keeps of a finding with its run.
type storedFinding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// finish ends a pass: what it read is judged and stored, its row is closed
// and — for an audit — the next one is scheduled. abort ends it without
// judging anything (the device was paused).
func (a *Auditor) finish(p *pass, dev queries.Device, detail string, abort bool, now time.Time) {
	a.mu.Lock()
	if a.passes[dev.ID] != p {
		a.mu.Unlock()
		return // ended already
	}
	delete(a.passes, dev.ID)
	sections, facts := p.sections, p.facts
	reads, failed, done := p.reads, p.failed, p.idx >= len(p.plan)
	a.mu.Unlock()

	name := nonempty(dev.Identity, dev.Address)
	run := &queries.AuditRun{ID: p.runID, FinishedAt: now, Reads: reads, FailedReads: failed, Detail: detail, Findings: "[]"}
	switch {
	case abort || reads == 0:
		// Nothing was judged: every finding stays as it was.
		run.Status = queries.AuditAborted
		if run.Detail == "" {
			run.Detail = "no read was made"
		}
	default:
		facts.Now = now
		res := hygiene.Evaluate(p.mode(), sections, facts)
		a.storeFindings(dev, res, now)
		kept := make([]storedFinding, 0, len(res.Findings))
		for _, f := range res.Findings {
			kept = append(kept, storedFinding{Kind: f.Kind, Severity: f.Severity, Title: f.Title, Detail: f.Detail})
		}
		if b, err := json.Marshal(kept); err == nil {
			run.Findings = string(b)
		}
		run.Checks = hygiene.ChecksJSON(res.Checks)
		if rt := hygiene.RuntimeOf(sections, now); rt.CPUForwarded != nil {
			run.Runtime = rt.JSON()
		}
		run.Status = queries.AuditPartial
		if done && failed == 0 {
			run.Status = queries.AuditComplete
		}
		if run.Detail == "" && failed > 0 {
			run.Detail = fmt.Sprintf("%d of %d reads failed (%s)", failed, reads, strings.Join(sections.Failed(), ", "))
		}
		if facts.RoleMissing {
			// The pass says what it left alone.
			note := "the checks that depend on the device's role were not made: " + facts.RoleReason
			if run.Detail != "" {
				note = run.Detail + "; " + note
			}
			run.Detail = note
		}
		// The snapshot is the device's configuration as a whole audit read
		// it. Without the role, the reads only a switch or an access point
		// gets were not sent: what was read is judged, and is not stored as
		// the whole (the hash would move, and move back with the next audit).
		changed := false
		if p.req.kind == queries.AuditKindAudit && run.Status == queries.AuditComplete && !facts.RoleMissing {
			run.Hash, changed = a.storeSnapshot(dev, sections, run.Findings, now)
		}
		worst := res.Worst()
		if worst == "" {
			worst = "none"
		}
		what := "audit"
		if p.req.kind == queries.AuditKindDiagnose {
			what = "diagnose pass"
		}
		note := ""
		if changed {
			note = ", configuration changed"
		}
		log.Printf("poller audit: %s of %s (%s): %s, %d reads, %d findings (worst: %s)%s",
			what, name, p.req.reason, run.Status, reads, len(res.Findings), worst, note)
	}
	if err := queries.FinishAuditRun(a.db, run); err != nil {
		log.Printf("poller audit: store the pass of %s: %v", name, err)
	}
	if run.Status == queries.AuditAborted {
		log.Printf("poller audit: pass of %s (%s) aborted: %s", name, p.req.reason, run.Detail)
	}

	if p.req.kind != queries.AuditKindAudit {
		return
	}
	// When this device is audited next.
	switch run.Status {
	case queries.AuditComplete:
		a.mu.Lock()
		a.lastAudit[dev.ID] = now
		a.mu.Unlock()
		a.clock.ran(dev.ID, devclass.TierAudit, a.cadence.Interval(dev, devclass.TierAudit, now), now)
	case queries.AuditPartial:
		a.clock.retryAt(dev.ID, devclass.TierAudit, now.Add(auditPartialRetry))
	default:
		if p.req.reason == PassScheduled {
			a.clock.retryAt(dev.ID, devclass.TierAudit, now.Add(auditBusyRetry))
		}
	}
}

// storeFindings writes what a pass found, each finding with its explanation
// and fix. The audit's own findings of the kinds it evaluated and no longer
// found are removed. A foreign API session is held: it stands until a pass
// that read the sessions no longer sees it, which then only lets go of it —
// the row is the login watch's too, and counts by when it was last seen from
// then on.
func (a *Auditor) storeFindings(dev queries.Device, res hygiene.Result, now time.Time) {
	keep := make([]queries.FindingKey, 0, len(res.Findings))
	held := map[string][]string{} // shared kind → the details this pass saw
	for _, f := range res.Findings {
		row := &queries.DeviceFinding{DeviceID: dev.ID, Kind: f.Kind, Severity: f.Severity, Detail: f.Detail,
			Title: f.Title, Explanation: f.Explanation}
		for _, step := range f.Fix {
			row.Fix = append(row.Fix, queries.FindingFix{Text: step.Text, Commands: step.Commands, ReadOnly: step.ReadOnly, Window: step.Window})
		}
		if err := queries.UpsertAuditFinding(a.db, row, now); err != nil {
			log.Printf("poller audit: record finding: %v", err)
			continue
		}
		if f.Foreign {
			held[f.Kind] = append(held[f.Kind], f.Detail)
			continue
		}
		keep = append(keep, queries.FindingKey{Kind: f.Kind, Detail: f.Detail})
	}
	name := nonempty(dev.Identity, dev.Address)
	if _, err := queries.DeleteAuditFindingsExcept(a.db, dev.ID, res.Evaluated, keep); err != nil {
		log.Printf("poller audit: remove the findings that are gone from %s: %v", name, err)
	}
	for _, kind := range res.Shared {
		if _, err := queries.ReleaseAuditFindings(a.db, dev.ID, kind, held[kind]); err != nil {
			log.Printf("poller audit: let go of the %s findings that are gone from %s: %v", kind, name, err)
		}
	}
}

// storeSnapshot stores the configuration a complete audit read — only when
// it differs from the device's newest stored one — and returns its hash.
func (a *Auditor) storeSnapshot(dev queries.Device, sections hygiene.Sections, findings string, now time.Time) (hash string, changed bool) {
	snap, complete := hygiene.SnapshotOf(sections)
	if !complete {
		return "", false
	}
	hash = snap.Hash()
	last, err := queries.LatestDeviceAudit(a.db, dev.ID)
	if err != nil {
		log.Printf("poller audit: read the last snapshot of %s: %v", nonempty(dev.Identity, dev.Address), err)
		return hash, false
	}
	if last != nil && last.Hash == hash {
		return hash, false
	}
	if err := queries.InsertDeviceAudit(a.db, &queries.DeviceAudit{DeviceID: dev.ID, TakenAt: now, Hash: hash,
		ROSVersion: dev.ROSVersion, Snapshot: snap.JSON(), Findings: findings}); err != nil {
		log.Printf("poller audit: store the snapshot of %s: %v", nonempty(dev.Identity, dev.Address), err)
		return hash, false
	}
	return hash, last != nil
}

// diagnoseRules are the alert rules whose alerts start a diagnose pass.
var diagnoseRules = map[string]bool{
	alerts.RuleCPUHigh:            true,
	alerts.RuleFlowExportCPUHigh:  true,
	alerts.RuleFlowExportOnSwitch: true, // once the export is confirmed on the device
}

// diagnoseTriggers returns, per device, the open alert that is owed a
// diagnose pass now: an alert of a diagnoseRules rule that no pass has
// answered yet — none that judged something started since the alert opened —
// on a device whose hourly budget is free. One pass per episode; an alert
// that opens within the hour of the last pass gets its own once the hour is
// over, if it is still open then. Read from the database every
// alertScanEvery; between scans the last answer stands.
func (a *Auditor) diagnoseTriggers(devices []queries.Device, now time.Time) map[string]queries.Alert {
	a.mu.Lock()
	if !a.scannedAt.IsZero() && now.Sub(a.scannedAt) < alertScanEvery && !now.Before(a.scannedAt) {
		out := a.triggers
		a.mu.Unlock()
		return out
	}
	a.scannedAt = now
	a.mu.Unlock()

	out := map[string]queries.Alert{}
	open, err := queries.ListOpenAlerts(a.db)
	if err != nil {
		log.Printf("poller audit: list the open alerts: %v", err)
	} else {
		a.endWatches(open, now)
	}
	known := deviceIDs(devices)
	for _, al := range open { // oldest first
		if !diagnoseRules[al.Rule] || al.DeviceID == "" || !known[al.DeviceID] {
			continue
		}
		if al.Rule == alerts.RuleFlowExportOnSwitch && (!al.Confirmed || al.Severity != alerts.SeverityCritical) {
			continue // a source address alone starts nothing
		}
		if _, taken := out[al.DeviceID]; taken {
			continue
		}
		if a.owed(al, now) {
			out[al.DeviceID] = al
		}
	}
	a.mu.Lock()
	a.triggers = out
	a.mu.Unlock()
	return out
}

// owed reports whether an open alert is owed a diagnose pass at now: no pass
// that judged something has started since the alert opened (or that cannot
// be told), and the device's hourly budget is free. It is asked when the
// alerts are read and again right before the pass begins.
func (a *Auditor) owed(al queries.Alert, now time.Time) bool {
	last, err := queries.LatestFinishedAuditRun(a.db, al.DeviceID, queries.AuditKindDiagnose)
	if err != nil || (last != nil && !last.StartedAt.Before(al.FirstAt)) {
		return false // answered already (or cannot tell)
	}
	_, free := a.budget(al.DeviceID, queries.AuditKindDiagnose, now)
	return free
}

// dropTrigger forgets the alert that asked for a pass on the device until
// the alerts are read again: its pass has begun, or it is not owed one.
func (a *Auditor) dropTrigger(deviceID string) {
	a.mu.Lock()
	delete(a.triggers, deviceID)
	a.mu.Unlock()
}

// endWatches removes the CPU watches that are over: the half hour after the
// accept has passed (alerts.FlowExportWatchRunning) and no
// flow_export_cpu_high alert is open for the device — it never fired, or it
// has resolved. From then on the rule could only keep an open alert, and
// there is none: the row has nothing left to say. open are the open alerts.
// (A removed exporter takes its watch with it at once: queries.DeleteFlowExporter.)
func (a *Auditor) endWatches(open []queries.Alert, now time.Time) {
	watches, err := queries.ListFlowExportWatches(a.db)
	if err != nil || len(watches) == 0 {
		return
	}
	alerting := map[string]bool{}
	for _, al := range open {
		if al.Rule == alerts.RuleFlowExportCPUHigh {
			alerting[al.DeviceID] = true
		}
	}
	for _, w := range watches {
		if alerts.FlowExportWatchRunning(w.AcceptedAt, now) || alerting[w.DeviceID] {
			continue
		}
		// By the accept it belongs to: a watch started again meanwhile stays.
		if err := queries.EndFlowExportWatch(a.db, w.DeviceID, w.AcceptedAt); err != nil {
			log.Printf("poller audit: end the flow-export watch of %s: %v", w.Address, err)
		}
	}
}

// startWatches turns the exporters the flow collector accepted since the
// last tick into CPU watches (flow_export_watches): the device, when it was
// accepted, and its average quiet-window CPU of the 24 hours before. The
// flow_export_cpu_high rule does the watching; it reads the database only.
func (a *Auditor) startWatches(now time.Time) {
	for {
		var ev acceptedExporter
		select {
		case ev = <-a.accepted:
		default:
			return
		}
		deviceID := ev.exporter.DeviceID
		if deviceID == "" {
			// An exporter an admin added by address: it is a managed
			// device's when the address is one.
			devices, err := queries.ListDevices(a.db)
			if err != nil {
				continue
			}
			for _, d := range devices {
				if d.Address == ev.exporter.Address {
					deviceID = d.ID
				}
			}
		}
		if deviceID == "" {
			continue // not a managed device: nothing to watch
		}
		w := &queries.FlowExportWatch{DeviceID: deviceID, ExporterID: ev.exporter.ID, Address: ev.exporter.Address,
			CollectorPort: a.collectorPort, AcceptedAt: ev.at}
		avg, minutes, err := queries.DeviceCPUBaseline(a.db, deviceID, ev.at.Add(-flowBaselineWindow), ev.at.Truncate(time.Minute))
		if err == nil && minutes >= flowBaselineMinMinutes {
			w.Baseline = &avg
		}
		if err := queries.StartFlowExportWatch(a.db, w); err != nil {
			log.Printf("poller audit: start the flow-export watch of %s: %v", ev.exporter.Address, err)
			continue
		}
		baseline := "no CPU baseline yet"
		if w.Baseline != nil {
			baseline = fmt.Sprintf("24 h CPU baseline %.0f %%", avg)
		}
		log.Printf("poller audit: watching the CPU of %s for 30 min after its flow export was accepted (%s)", ev.exporter.Address, baseline)
	}
}

// auditLogEvery bounds how often the same waiting audit is logged: a device
// that stays busy for a day is retried every ten minutes, and said so once
// an hour.
const auditLogEvery = time.Hour

// logThrottled logs at most once per auditLogEvery per key.
func (a *Auditor) logThrottled(key string, now time.Time, format string, args ...any) {
	a.mu.Lock()
	last, seen := a.lastLog[key]
	if seen && now.Sub(last) < auditLogEvery && !now.Before(last) {
		a.mu.Unlock()
		return
	}
	a.lastLog[key] = now
	a.mu.Unlock()
	log.Printf(format, args...)
}

// sweepAudit applies the retention of the audit tables (database only).
func sweepAudit(db *sql.DB, now time.Time) {
	if n, err := queries.DeleteOldAuditRuns(db, now.Add(-auditRunRetention)); err != nil {
		log.Printf("poller retention: audit passes: %v", err)
	} else if n > 0 {
		log.Printf("poller retention: deleted %d old audit passes", n)
	}
	if n, err := queries.DeleteOldDeviceAudits(db, now.Add(-auditSnapshotRetention)); err != nil {
		log.Printf("poller retention: audit snapshots: %v", err)
	} else if n > 0 {
		log.Printf("poller retention: deleted %d old audit snapshots", n)
	}
	if _, err := queries.DeleteOldFlowExportWatches(db, now.Add(-flowWatchRetention)); err != nil {
		log.Printf("poller retention: flow-export watches: %v", err)
	}
}
