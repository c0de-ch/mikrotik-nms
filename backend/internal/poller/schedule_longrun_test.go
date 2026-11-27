package poller

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// The long run: two days of one device's scheduled reads — the C1 48-port
// switch of the call plans (sw48) above all, and an approximation of each of
// the other call-plan personalities — on the tier clocks the loops use, to
// show that the arrangement of its reads holds for as long as the process
// runs.
// The call-plan hour cannot show it: there every read starts on time and
// takes no time. Here each loop wakes as its Run does — after its start delay,
// then pollerTick after the end of its pass — and every read takes the time
// it takes on a CRS354-class switch, so a read starts up to a tick plus the
// reads before it late, as in production. A schedule that counts from the end
// of each run lets that lateness add up; the groups of reads then drift back
// into shared minutes within hours.
//
// What is simulated is the scheduling, one device: each loop's own due rule
// on the real tierClock (topology, info, network health, hosts, clients,
// wifi, the post-poll CPU sample and the audit, each its own clock as the
// manager wires them), the counter read on devclass.NextRun and Tier 4a on
// the same slot rule as the port-stats collector, the quiet-window CPU sample
// once per minute, the post-poll sample 3 s after a counter read, the wifi
// tier's capability probes through the real CapabilityCache, and the hygiene
// audit's pass of reads 20 s apart. Commands are counted per calendar minute.

// lrStart is the simulated start: on a full minute, so minute n is a calendar
// minute.
var lrStart = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

const (
	lrDevice   = "sw48"
	lrDuration = 48 * time.Hour
	// lrBudget is the plan's budget for a 48-port C1 switch: at most 12
	// commands in any minute.
	lrBudget = 12
)

// lrPersonality is what the simulation sends one kind of device: its class
// (the cadences) and the commands of one run of each read, from its call
// plan (testdata/callplan). sw48 is the one the arrangement is proved for;
// the others are approximations for the record (their logged busiest
// minutes), held only to their class ceilings.
type lrPersonality struct {
	id          string
	class       devclass.Class
	bridgePorts int // bridge print, port print, monitor
	relations   int // the stacking menus the device has
	regs        int // registration-table prints per wifi run (0: no wifi role)
	dhcp        int // DHCP lease (+ server) prints, C3
	auditReads  int // reads of one scheduled audit pass
	pings       int // connectivity probes from the device every 30 s
}

var lrPersonalities = []lrPersonality{
	{id: "sw48", class: devclass.C1, bridgePorts: 3, relations: 2, auditReads: 20},
	{id: "ap-ac", class: devclass.C1, bridgePorts: 3, regs: 1, auditReads: 12},
	{id: "sw24", class: devclass.C2, bridgePorts: 3, relations: 1, auditReads: 17},
	{id: "ap-ax", class: devclass.C3, bridgePorts: 3, regs: 1, dhcp: 1, auditReads: 12},
	{id: "rtr", class: devclass.C3, bridgePorts: 3, dhcp: 2, auditReads: 14, pings: 2},
}

// Groups of reads: the ones that start up together and must keep their own
// minutes, and the reads that may land in any minute (lrBase: the counter
// read, the CPU samples; lrAudit: the hygiene audit's pass).
const (
	lrGroupA  = "A"  // read at once: log, ethernet, wifi (capability probes), ARP, inventory
	lrGroupBC = "BC" // bridge ports, neighbors, routes
	lrGroupDE = "DE" // bridge VLANs, FDB, stacking
	lrBase    = "base"
	lrAudit   = "audit"
)

// lrRead is one scheduled read of the switch: its tier and group, the
// commands of one run, and how long one run takes on the device.
type lrRead struct {
	tier  devclass.Tier
	group string
	cmds  int
	took  time.Duration
}

// lrClock is what the simulation needs of a schedule: the tier clock's due /
// ran / anchorAt. *tierClock has it; lrEndClock is the end-based schedule
// this branch replaced.
type lrClock interface {
	due(deviceID string, tier devclass.Tier, interval time.Duration, now time.Time) bool
	ran(deviceID string, tier devclass.Tier, interval time.Duration, end time.Time)
	anchorAt(deviceID string, tier devclass.Tier, at time.Time)
}

// lrCmd is one command the switch received.
type lrCmd struct {
	at    time.Time
	group string
	what  string // the read it belongs to
}

// lrSim is one simulated run.
type lrSim struct {
	p        lrPersonality
	phase    uint64
	newClock func() lrClock
	cmds     []lrCmd
	tier0    time.Time // end of the latest counter read
}

func (s *lrSim) send(at time.Time, n int, took time.Duration, group, what string) time.Time {
	for i := 0; i < n; i++ {
		s.cmds = append(s.cmds, lrCmd{at: at.Add(took * time.Duration(i) / time.Duration(n)), group: group, what: what})
	}
	return at.Add(took)
}

func (s *lrSim) interval(tier devclass.Tier) time.Duration { return devclass.Cadence(s.p.class, tier) }

// readsLoop is a loop that visits the device every pollerTick after its pass
// and runs the reads that are due at the start of the pass, in order.
func (s *lrSim) readsLoop(clock lrClock, reads ...lrRead) func(time.Time) time.Time {
	return func(now time.Time) time.Time {
		var due []lrRead
		for _, r := range reads {
			if r.cmds > 0 && clock.due(lrDevice, r.tier, s.interval(r.tier), now) {
				due = append(due, r)
			}
		}
		at := now
		for _, r := range due {
			at = s.send(at, r.cmds, r.took, r.group, r.tier.String())
			clock.ran(lrDevice, r.tier, s.interval(r.tier), at)
		}
		return at.Add(pollerTick)
	}
}

// infoLoop is the inventory read: three prints, and the pool's keep-alive in
// front of them once the session is open.
func (s *lrSim) infoLoop(clock lrClock) func(time.Time) time.Time {
	runs := 0
	return func(now time.Time) time.Time {
		at := now
		if clock.due(lrDevice, devclass.TierInventory, s.interval(devclass.TierInventory), now) {
			if runs > 0 {
				at = s.send(at, 1, 50*time.Millisecond, lrGroupA, "keep-alive")
			}
			at = s.send(at, 3, 600*time.Millisecond, lrGroupA, "inventory")
			runs++
			clock.ran(lrDevice, devclass.TierInventory, s.interval(devclass.TierInventory), at)
		}
		return at.Add(pollerTick)
	}
}

// countersLoop is the port-stats collector's cycle: the counter read on
// devclass.NextRun from its end, Tier 4a on its own clock (the collector's
// slot rule), the cycle waking about every second.
func (s *lrSim) countersLoop(eth lrClock) func(time.Time) time.Time {
	var nextStats time.Time
	return func(now time.Time) time.Time {
		at := now
		if isDue(nextStats, now, s.interval(devclass.TierCounters)) {
			at = s.send(at, 1, time.Second, lrBase, "counters")
			nextStats = devclass.NextRun(at, s.interval(devclass.TierCounters), s.phase)
			s.tier0 = at
		}
		if eth.due(lrDevice, devclass.TierEthernet, s.interval(devclass.TierEthernet), now) {
			at = s.send(at, 1, 800*time.Millisecond, lrGroupA, "ethernet")
			eth.ran(lrDevice, devclass.TierEthernet, s.interval(devclass.TierEthernet), at)
		}
		return at.Add(time.Second)
	}
}

// cpuLoop is the CPU sampler: the quiet-window sample once per minute (its
// slot) and the post-poll sample on its clock, taken 3 s after a counter
// read once it is due (CPUSampler.tickPost).
func (s *lrSim) cpuLoop(post lrClock) func(time.Time) time.Time {
	var slot, followed time.Time
	return func(now time.Time) time.Time {
		wake := now.Add(pollerTick)
		if post.due(lrDevice, devclass.TierCPUPost, s.interval(devclass.TierCPUPost), now) {
			age := now.Sub(s.tier0)
			switch {
			case s.tier0.IsZero() || !s.tier0.After(followed) || age >= cpuPostDelay+cpuPostSlack:
				wake = now.Add(cpuFastTick)
			case age < cpuPostDelay:
				wake = s.tier0.Add(cpuPostDelay)
			default:
				followed = s.tier0
				end := s.send(now, 1, 100*time.Millisecond, lrBase, "cpu_post")
				post.ran(lrDevice, devclass.TierCPUPost, s.interval(devclass.TierCPUPost), end)
			}
		}
		if cur := devclass.SlotAt(now, time.Minute, s.phase); cur.After(slot) {
			if !slot.IsZero() {
				s.send(now, 1, 100*time.Millisecond, lrBase, "cpu")
			}
			slot = cur
		}
		next := slot.Add(time.Minute)
		if next.Before(wake) {
			wake = next
		}
		return wake
	}
}

// wifiLoop is the wifi tracker: on an access point the registration read
// every run, after the capability probe at start-up and every 24 h; on a
// switch the tier runs only while the probe is due — the full probe, the
// hourly recheck of the empty wifi menu — through the real CapabilityCache.
func (s *lrSim) wifiLoop(clock lrClock) func(time.Time) time.Time {
	caps := NewCapabilityCache()
	const version = "7.24.5 (stable)"
	return func(now time.Time) time.Time {
		iv := s.interval(devclass.TierWifiRegistrations)
		if c, known := caps.Get(lrDevice); known && !c.HasWifiRole() && !probeDue(c, version, now) {
			iv = 0
		}
		at := now
		if clock.due(lrDevice, devclass.TierWifiRegistrations, iv, now) {
			run := &lrProbeRunner{sim: s, at: now}
			c := caps.Ensure(lrDevice, version, run, now)
			at = run.at
			if c.HasWifiRole() {
				at = s.send(at, s.p.regs, 300*time.Millisecond, lrGroupA, "registrations")
			}
			clock.ran(lrDevice, devclass.TierWifiRegistrations, iv, at)
		}
		return at.Add(pollerTick)
	}
}

// pingLoop is the connectivity probes a router runs: every 30 s, end-based.
func (s *lrSim) pingLoop() func(time.Time) time.Time {
	return func(now time.Time) time.Time {
		return s.send(now, s.p.pings, 2*time.Second, lrBase, "ping").Add(30 * time.Second)
	}
}

// auditLoop is the hygiene audit: seeded at the start-up delay plus the
// device's spread, a pass of reads spaced by class (passGap), the next pass
// on the audit's clock.
func (s *lrSim) auditLoop(clock lrClock) func(time.Time) time.Time {
	seeded, left := false, 0
	var nextRead time.Time
	iv := s.interval(devclass.TierAudit)
	return func(now time.Time) time.Time {
		if !seeded {
			clock.anchorAt(lrDevice, devclass.TierAudit, lrStart.Add(auditStartupDelay+time.Duration(s.phase%uint64(auditStartupSpread/time.Second))*time.Second))
			seeded = true
		}
		if left == 0 && clock.due(lrDevice, devclass.TierAudit, iv, now) {
			left, nextRead = s.p.auditReads, now
		}
		if left > 0 && !now.Before(nextRead) {
			end := s.send(now, 1, 200*time.Millisecond, lrAudit, "audit")
			if left--; left == 0 {
				clock.ran(lrDevice, devclass.TierAudit, iv, end)
			}
			nextRead = end.Add(passGap(s.p.class, queries.AuditKindAudit))
		}
		wake := now.Add(pollerTick)
		if left > 0 && nextRead.Before(wake) {
			wake = maxTime(nextRead, now.Add(200*time.Millisecond))
		}
		return wake
	}
}

// run simulates lrDuration and returns the commands sent.
func (s *lrSim) run() []lrCmd {
	type loop struct {
		next time.Time
		tick func(time.Time) time.Time
	}
	loops := []*loop{
		{lrStart.Add(cpuStartupDelay), s.cpuLoop(s.newClock())},
		{lrStart.Add(8 * time.Second), s.infoLoop(s.newClock())},
		{lrStart.Add(5 * time.Second), s.readsLoop(s.newClock(),
			lrRead{devclass.TierNeighbors, lrGroupBC, 1, 200 * time.Millisecond},
			lrRead{devclass.TierRoutes, lrGroupBC, 1, 100 * time.Millisecond})},
		{lrStart.Add(portStatsStartupDelay), s.countersLoop(s.newClock())},
		{lrStart.Add(portHostsStartupDelay), s.readsLoop(s.newClock(),
			lrRead{devclass.TierBridgeHosts, lrGroupDE, 1, 2 * time.Second},
			lrRead{devclass.TierRelations, lrGroupDE, s.p.relations, 200 * time.Millisecond})},
		{lrStart.Add(15 * time.Second), s.wifiLoop(s.newClock())},
		{lrStart.Add(10 * time.Second), s.readsLoop(s.newClock(),
			lrRead{devclass.TierARP, lrGroupA, 1, 100 * time.Millisecond},
			lrRead{devclass.TierDHCPLeases, lrGroupA, s.p.dhcp, 100 * time.Millisecond})},
		{lrStart.Add(20 * time.Second), s.readsLoop(s.newClock(),
			lrRead{devclass.TierBridgePorts, lrGroupBC, s.p.bridgePorts, 1500 * time.Millisecond},
			lrRead{devclass.TierBridgeVLAN, lrGroupDE, 1, 300 * time.Millisecond},
			lrRead{devclass.TierLogs, lrGroupA, 1, 300 * time.Millisecond})},
		{lrStart.Add(auditLoopDelay), s.auditLoop(s.newClock())},
	}
	if s.p.pings > 0 {
		loops = append(loops, &loop{lrStart.Add(20 * time.Second), s.pingLoop()})
	}
	end := lrStart.Add(lrDuration)
	for {
		due := loops[0]
		for _, l := range loops[1:] {
			if l.next.Before(due.next) {
				due = l
			}
		}
		if !due.next.Before(end) {
			break
		}
		due.next = due.tick(due.next)
	}
	sort.SliceStable(s.cmds, func(i, j int) bool { return s.cmds[i].at.Before(s.cmds[j].at) })
	return s.cmds
}

// lrProbeRunner answers the capability probe as a switch does (an empty wifi
// menu and DHCP server, a bridge, no CAPsMAN or legacy wireless) — an access
// point with interfaces in its wifi menu, a C3 device with a DHCP server —
// and records each print as a command of group A.
type lrProbeRunner struct {
	sim *lrSim
	at  time.Time
}

func (r *lrProbeRunner) Run(command string, args ...string) (*ros.Reply, error) {
	r.at = r.sim.send(r.at, 1, 20*time.Millisecond, lrGroupA, "probe")
	count := map[string]string{
		routeros.WifiProbeCommand:       fmt.Sprint(r.sim.p.regs),
		routeros.DHCPServerProbeCommand: fmt.Sprint(min(r.sim.p.dhcp, 1)),
		routeros.BridgeProbeCommand:     "1",
	}
	if n, ok := count[command]; ok {
		return &ros.Reply{Done: &proto.Sentence{Word: "!done", Map: map[string]string{"ret": n}}}, nil
	}
	return nil, trapErr("no such command prefix")
}

// lrMinute is one calendar minute of a run: its commands, their groups and
// reads.
type lrMinute struct {
	n      int
	groups map[string]int
	reads  map[string]int
}

// lrReport is what a run did, minute by minute.
type lrReport struct {
	minutes []lrMinute
	// over: minutes after start-up above lrBudget; shared: minutes holding
	// two groups.
	over, shared []int
}

func lrAnalyse(cmds []lrCmd) lrReport {
	r := lrReport{minutes: make([]lrMinute, int(lrDuration/time.Minute))}
	for i := range r.minutes {
		r.minutes[i].groups = map[string]int{}
		r.minutes[i].reads = map[string]int{}
	}
	for _, c := range cmds {
		m := int(c.at.Sub(lrStart) / time.Minute)
		if m < 0 || m >= len(r.minutes) {
			continue
		}
		r.minutes[m].n++
		r.minutes[m].groups[c.group]++
		r.minutes[m].reads[c.what]++
	}
	for m, min := range r.minutes {
		if m < planStartupMinutes {
			continue
		}
		if min.n > lrBudget {
			r.over = append(r.over, m)
		}
		groups := 0
		for _, g := range []string{lrGroupA, lrGroupBC, lrGroupDE} {
			if min.groups[g] > 0 {
				groups++
			}
		}
		if groups > 1 {
			r.shared = append(r.shared, m)
		}
	}
	return r
}

// busiest is the busiest minute after start-up within hours [from, to).
func (r lrReport) busiest(from, to int) (n, at int) {
	for m := max(from*60, planStartupMinutes); m < to*60 && m < len(r.minutes); m++ {
		if r.minutes[m].n > n {
			n, at = r.minutes[m].n, m
		}
	}
	return n, at
}

func (r lrReport) describe(m int) string {
	min := r.minutes[m]
	var parts []string
	for _, g := range []string{lrGroupA, lrGroupBC, lrGroupDE, lrBase, lrAudit} {
		if min.groups[g] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", g, min.groups[g]))
		}
	}
	var reads []string
	for what, n := range min.reads {
		reads = append(reads, fmt.Sprintf("%s %d", what, n))
	}
	sort.Strings(reads)
	return fmt.Sprintf("minute %d (%s): %d commands — %s [%s]", m, time.Duration(m)*time.Minute, min.n,
		strings.Join(parts, ", "), strings.Join(reads, ", "))
}

// lrPhases are the device phases the long run is held to: the jitter range's
// ends, its middle, and sw48's own.
var lrPhases = map[string]uint64{
	"-10 %":   0,
	"-5 %":    500,
	"0":       1000,
	"+5 %":    1500,
	"+10 %":   2000,
	"of sw48": devclass.Phase("sw48"),
}

// The arrangement holds: on its slot grid, whatever its jitter, the C1
// switch is never sent more than 12 commands in a minute after start-up, in
// any of the 48 hours, and no two of its groups of reads share a minute.
func TestLongRunTierArrangementHolds(t *testing.T) {
	for name, phase := range lrPhases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lrHolds(t, name, phase)
		})
	}
}

func lrHolds(t *testing.T, name string, phase uint64) {
	r := lrRun(lrPersonalities[0], phase, lrSlots)
	first, firstAt := r.busiest(0, 1)
	last, lastAt := r.busiest(int(lrDuration/time.Hour)-1, int(lrDuration/time.Hour))
	t.Logf("jitter %s: busiest minute %d in hour 1 (%s), %d in hour 48 (%s)", name, first,
		time.Duration(firstAt)*time.Minute, last, time.Duration(lastAt)*time.Minute)
	if testing.Verbose() {
		idx := make([]int, 0, len(r.minutes))
		for m := planStartupMinutes; m < len(r.minutes); m++ {
			idx = append(idx, m)
		}
		sort.SliceStable(idx, func(i, j int) bool { return r.minutes[idx[i]].n > r.minutes[idx[j]].n })
		for _, m := range idx[:4] {
			t.Logf("    %s", r.describe(m))
		}
		for _, m := range []int{1440, 1441} {
			t.Logf("    at 24 h: %s", r.describe(m))
		}
	}
	for i, m := range r.over {
		if i == 5 {
			t.Errorf("jitter %s: … and %d more minutes above %d", name, len(r.over)-5, lrBudget)
			break
		}
		t.Errorf("jitter %s: %s, above the budget of %d", name, r.describe(m), lrBudget)
	}
	for i, m := range r.shared {
		if i == 5 {
			t.Errorf("jitter %s: … and %d more minutes shared by two groups", name, len(r.shared)-5)
			break
		}
		t.Errorf("jitter %s: %s — two groups in one minute", name, r.describe(m))
	}
}

// lrEndClock is the schedule the slot grid replaced (the branch before it):
// a tier's first run its lead after the first look — the leads of that
// schedule, one group a minute — and every later one devclass.NextRun from
// the end of the run before, so each run's lateness carries into the next.
type lrEndClock struct {
	phase uint64
	runs  map[devclass.Tier]tierRun
}

// lrEndLeads are the first-run leads the end-based schedule had.
var lrEndLeads = map[devclass.Tier]time.Duration{
	devclass.TierBridgePorts: 1 * time.Minute,
	devclass.TierNeighbors:   2 * time.Minute,
	devclass.TierRoutes:      2 * time.Minute,
	devclass.TierBridgeHosts: 3 * time.Minute,
	devclass.TierBridgeVLAN:  3 * time.Minute,
	devclass.TierRelations:   4 * time.Minute,
	devclass.TierCPUPost:     4 * time.Minute,
}

func (c *lrEndClock) due(_ string, tier devclass.Tier, interval time.Duration, now time.Time) bool {
	r := c.runs[tier]
	if r.first.IsZero() {
		r.first = now
		c.runs[tier] = r
	}
	if interval <= 0 {
		return false
	}
	var next time.Time
	switch {
	case !r.retry.IsZero():
		next = r.retry
	case r.end.IsZero():
		next = r.first.Add(min(lrEndLeads[tier], devclass.FirstRunBound(interval)))
	default:
		next = devclass.NextRun(r.end, interval, c.phase)
	}
	return !now.Before(next)
}

func (c *lrEndClock) ran(_ string, tier devclass.Tier, _ time.Duration, end time.Time) {
	c.runs[tier] = tierRun{first: c.runs[tier].first, end: end}
}

// anchorAt is the audit's seed, a retry time in that schedule.
func (c *lrEndClock) anchorAt(_ string, tier devclass.Tier, at time.Time) {
	r := c.runs[tier]
	r.retry = at
	c.runs[tier] = r
}

// The same two days on the end-based schedule: the arrangement holds in the
// first hour and is gone within the day — tick lateness moves the 5-minute
// reads against the 15-minute ones (and every group against the next) until
// groups share minutes again and the switch gets more than its budget in
// one. This is what the slot grid is for; if this test starts failing, the
// simulation no longer shows the drift and TestLongRunTierArrangementHolds
// proves less than it says.
func TestLongRunEndBasedScheduleDrifts(t *testing.T) {
	r := lrRun(lrPersonalities[0], devclass.Phase(lrDevice), lrEndBased)
	firstShared, lastOver := -1, -1
	for _, m := range r.shared {
		if firstShared < 0 {
			firstShared = m
		}
	}
	for _, m := range r.over {
		lastOver = m
	}
	hourly := make([]string, 0, lrDuration/time.Hour)
	for h := 0; h < int(lrDuration/time.Hour); h++ {
		n, _ := r.busiest(h, h+1)
		hourly = append(hourly, fmt.Sprint(n))
	}
	t.Logf("end-based: busiest minute per hour %s; %d minutes shared by two groups (first at %s), %d above %d",
		strings.Join(hourly, " "), len(r.shared), time.Duration(firstShared)*time.Minute, len(r.over), lrBudget)
	if testing.Verbose() && lastOver >= 0 {
		t.Logf("    e.g. %s", r.describe(lastOver))
	}
	if n, _ := r.busiest(0, 1); n > lrBudget {
		t.Errorf("hour 1 already has %d commands in a minute: the start-up leads, not drift", n)
	}
	if len(r.shared) == 0 || firstShared < 60 {
		t.Errorf("groups shared %d minutes, the first at minute %d: want none in hour 1 and some later (drift)", len(r.shared), firstShared)
	}
	if len(r.over) == 0 {
		t.Errorf("no minute above the budget of %d in %v: the simulation no longer shows the drift", lrBudget, lrDuration)
	}
}

// lrSlots and lrEndBased pick the schedule a run is simulated on.
const (
	lrSlots    = "slots"
	lrEndBased = "end-based"
)

// lrRun simulates personality p at phase on the slot grid or the end-based
// schedule.
func lrRun(p lrPersonality, phase uint64, schedule string) lrReport {
	s := &lrSim{p: p, phase: phase}
	s.newClock = func() lrClock {
		if schedule == lrEndBased {
			return &lrEndClock{phase: phase, runs: map[devclass.Tier]tierRun{}}
		}
		k := newTierClock(NewScheduleRegistry())
		k.phase = func(string) uint64 { return phase }
		return k
	}
	return lrAnalyse(s.run())
}

// Every personality, approximated, on the slot grid: never above its
// ceiling (planBusiestCeilings) in two days, and on C1 no two groups in one
// minute. Logged: the busiest minute in hour 1 and in hours 2-48 of both
// schedules, for the record.
func TestLongRunEveryPersonality(t *testing.T) {
	for _, p := range lrPersonalities {
		t.Run(p.id, func(t *testing.T) {
			t.Parallel()
			lrEvery(t, p)
		})
	}
}

func lrEvery(t *testing.T, p lrPersonality) {
	phase := devclass.Phase(p.id)
	var line []string
	for _, schedule := range []string{lrEndBased, lrSlots} {
		r := lrRun(p, phase, schedule)
		first, _ := r.busiest(0, 1)
		worst := 0
		for h := 1; h < int(lrDuration/time.Hour); h++ {
			worst = max(worst, func() int { n, _ := r.busiest(h, h+1); return n }())
		}
		line = append(line, fmt.Sprintf("%s: hour 1 %d, hours 2-48 at most %d", schedule, first, worst))
		if schedule == lrSlots && worst > planBusiestCeilings[p.id] {
			t.Errorf("%s: %d commands in a minute on the slot grid, above its ceiling of %d", p.id, worst, planBusiestCeilings[p.id])
		}
		if schedule == lrSlots && p.class == devclass.C1 && len(r.shared) > 0 {
			t.Errorf("%s: %s — two groups in one minute on the slot grid", p.id, r.describe(r.shared[0]))
		}
	}
	t.Logf("%s (%s): %s", p.id, p.class, strings.Join(line, "; "))
}
