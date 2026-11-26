package poller

import (
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// The exact sentences Tier 1 and 1c put on the wire.
const (
	tier1Call  = "/system/resource/print =.proplist=cpu-load,free-memory,uptime"
	tier1cCall = "/system/resource/print =.proplist=cpu-load"
)

// cpuRunner scripts the resource print of a device: cpu % with 20 MiB free
// and a growing uptime.
func cpuRunner(cpu string) *fakeRunner {
	return &fakeRunner{byCmd: map[string]fakeReply{
		"/system/resource/print": rep(map[string]string{"cpu-load": cpu, "free-memory": "20971520", "uptime": "1h2m3s"}),
	}}
}

// samplerHarness is a CPUSampler on a fake clock: scripted devices, a
// settable "last command ended" and "last Tier 0 read" per device, slots on
// the full minute and no jitter.
type samplerHarness struct {
	s      *CPUSampler
	db     *sql.DB
	load   *LoadRecorder
	clock  *testClock
	cs     *stubCadence
	hub    *ws.Hub
	mu     sync.Mutex
	runner map[string]*fakeRunner
	cmdEnd map[string]time.Time
	tier0  map[string]time.Time
}

func newSamplerHarness(t *testing.T, ids ...string) *samplerHarness {
	t.Helper()
	db := pollerTestDB(t)
	h := &samplerHarness{
		db: db, clock: &testClock{t: at(12, 0, 20)}, hub: ws.NewHub(),
		runner: map[string]*fakeRunner{}, cmdEnd: map[string]time.Time{}, tier0: map[string]time.Time{},
		cs: &stubCadence{
			iv:     map[devclass.Tier]time.Duration{devclass.TierCPU: time.Minute, devclass.TierCPUPost: 10 * time.Minute},
			paused: map[string]bool{},
		},
	}
	for i, id := range ids {
		mustCreateDevice(t, db, &queries.Device{ID: id, Identity: id, Address: "192.0.2." + string(rune('1'+i)), Status: "online"})
		if _, err := db.Exec(`UPDATE devices SET memory_total = ? WHERE id = ?`, int64(64<<20), id); err != nil {
			t.Fatal(err)
		}
		h.runner[id] = cpuRunner("12")
	}
	h.load = NewLoadRecorder(db)
	h.load.started = at(12, 0, 0)
	s := NewCPUSampler(db, nil, h.hub, h.load, nil)
	s.UseCadence(h.cs)
	s.now = h.clock.now
	s.spawn = func(f func()) { f() }
	s.phase = func(string) uint64 { return 0 }
	s.reg = NewScheduleRegistry()
	s.postClock = newTierClock(s.reg)
	s.postClock.phase = noJitter
	s.runner = func(id string) routeros.Runner {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r := h.runner[id]; r != nil {
			return r
		}
		return nil
	}
	s.lastCommandEnd = func(id string) time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cmdEnd[id]
	}
	s.tier0At = func(id string) time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.tier0[id]
	}
	h.s = s
	return h
}

// tickAt moves the clock to ts and runs one tick.
func (h *samplerHarness) tickAt(ts time.Time) time.Time {
	h.clock.set(ts)
	return h.s.tick(ts)
}

func (h *samplerHarness) calls(id string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.runner[id].calls...)
}

func (h *samplerHarness) setCmdEnd(id string, ts time.Time) {
	h.mu.Lock()
	h.cmdEnd[id] = ts
	h.mu.Unlock()
}

func (h *samplerHarness) setTier0(id string, ts time.Time) {
	h.mu.Lock()
	h.tier0[id] = ts
	h.mu.Unlock()
}

// Tier 1: one three-field resource print per device and minute, at the
// device's slot, held back until the device has been left alone for 3 s — and
// taken anyway after 10 s of waiting, on a device that is never quiet.
func TestCPUSampleOncePerMinuteInAQuietWindow(t *testing.T) {
	h := newSamplerHarness(t, "sw")
	h.cs.iv[devclass.TierCPUPost] = 0 // the quiet-window sample on its own
	var health []map[string]interface{}
	defer h.hub.Listen("device.health", func(v any) { health = append(health, v.(map[string]interface{})) })()

	// First sight mid-slot: the sample waits for the device's next slot.
	h.tickAt(at(12, 0, 20))
	if n := len(h.calls("sw")); n != 0 {
		t.Fatalf("a device first seen mid-slot was sampled at once: %q", h.calls("sw"))
	}
	if next := h.s.reg.NextRuns("sw")["cpu"]; !next.Equal(at(12, 1, 0)) {
		t.Errorf("next cpu run = %v, want the 12:01:00 slot", next)
	}

	// The slot opens one second after a counter read ended: not quiet yet.
	h.setCmdEnd("sw", at(12, 0, 59))
	wake := h.tickAt(at(12, 1, 0))
	if n := len(h.calls("sw")); n != 0 {
		t.Fatalf("sampled 1 s after the device's last command: %q", h.calls("sw"))
	}
	if !wake.Equal(at(12, 1, 2)) {
		t.Errorf("wake = %v, want 12:01:02 — three seconds after the last command", wake)
	}
	h.tickAt(at(12, 1, 1))
	if n := len(h.calls("sw")); n != 0 {
		t.Fatalf("sampled 2 s after the device's last command")
	}
	h.tickAt(at(12, 1, 2))
	if got := h.calls("sw"); len(got) != 1 || got[0] != tier1Call {
		t.Fatalf("calls = %q, want exactly the three-field resource print", got)
	}
	// Nothing more in this slot, however often the loop wakes.
	for s := 3; s < 60; s++ {
		h.tickAt(at(12, 1, s))
	}
	if n := len(h.calls("sw")); n != 1 {
		t.Fatalf("calls in one slot = %d, want 1", n)
	}

	// A device that is never left alone: the sample is taken after 10 s.
	for s := 0; s < 10; s++ {
		h.setCmdEnd("sw", at(12, 2, s).Add(-time.Second))
		h.tickAt(at(12, 2, s))
	}
	if n := len(h.calls("sw")); n != 1 {
		t.Fatalf("sampled a busy device before the 10 s were up (calls %d)", n)
	}
	h.setCmdEnd("sw", at(12, 2, 9))
	h.tickAt(at(12, 2, 10))
	if n := len(h.calls("sw")); n != 2 {
		t.Fatalf("calls = %d, want the sample taken after waiting 10 s", n)
	}

	// The sample refreshes the device row — not its status or last_seen —
	// and goes out on device.health.
	d, err := queries.GetDevice(h.db, "sw")
	if err != nil {
		t.Fatal(err)
	}
	if d.CPULoad == nil || *d.CPULoad != 12 || d.MemoryUsed == nil || *d.MemoryUsed != (64<<20)-20971520 || d.Uptime == nil || *d.Uptime != "1h2m3s" {
		t.Errorf("device row = cpu %v mem %v uptime %v", d.CPULoad, d.MemoryUsed, d.Uptime)
	}
	if d.LastSeen != nil {
		t.Errorf("last_seen = %v: a load sample is not a liveness result", d.LastSeen)
	}
	if len(health) != 2 {
		t.Fatalf("device.health publications = %d, want one per sample", len(health))
	}
	if p := health[0]; p["device_id"] != "sw" || p["status"] != "online" || p["cpu_load"] != 12 || p["memory_pct"] != 68 || p["uptime"] != "1h2m3s" {
		t.Errorf("device.health payload = %v", p)
	}

	h.load.FlushMinutes(at(12, 3, 0), nil)
	for _, minute := range []time.Time{at(12, 1, 0), at(12, 2, 0)} {
		if m := minuteRow(t, h.db, "sw", minute); m.CPUSamples != 1 || m.CPUSum != 12 || m.FreeMemoryMin != nullInt(20971520) || m.UptimeS != nullInt(3723) {
			t.Errorf("%s = %+v, want one sample", minute.Format("15:04"), m)
		}
	}
	if m := minuteRow(t, h.db, "sw", at(12, 0, 0)); m.CPUSamples != 0 {
		t.Errorf("12:00 = %+v, want no sample", m)
	}
}

// Nothing is sampled on a paused device, an offline one, or one without a
// live pooled session; the pause is recorded, and a session that comes back
// mid-slot is sampled in that slot.
func TestCPUSampleSkipsPausedOfflineAndSessionless(t *testing.T) {
	h := newSamplerHarness(t, "paused", "offline", "nosession", "ok")
	h.cs.paused["paused"] = true
	if err := queries.MarkDeviceUnreachable(h.db, "offline", "offline", nil); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	nosession := h.runner["nosession"]
	h.runner["nosession"] = nil
	h.mu.Unlock()

	h.tickAt(at(12, 0, 20))
	for s := 0; s < 30; s += 5 {
		h.tickAt(at(12, 1, s))
	}
	if n := len(h.calls("ok")); n != 1 {
		t.Fatalf("the healthy device has %d samples, want 1", n)
	}
	for _, id := range []string{"paused", "offline"} {
		if n := len(h.calls(id)); n != 0 {
			t.Errorf("%s was sampled: %q", id, h.calls(id))
		}
		if _, ok := h.s.reg.NextRuns(id)["cpu"]; ok {
			t.Errorf("%s lists a next cpu run", id)
		}
	}
	if n := len(nosession.calls); n != 0 {
		t.Errorf("a device without a session was sampled: %q", nosession.calls)
	}

	// The session comes back at 12:01:30: still this slot's sample.
	h.mu.Lock()
	h.runner["nosession"] = nosession
	h.mu.Unlock()
	h.tickAt(at(12, 1, 30))
	if n := len(nosession.calls); n != 1 {
		t.Errorf("samples after the session came back = %d, want 1", n)
	}

	h.load.FlushMinutes(at(12, 2, 0), nil)
	if m := minuteRow(t, h.db, "paused", at(12, 1, 0)); !m.Paused || m.GapReason != queries.GapPaused || m.CPUSamples != 0 {
		t.Errorf("paused device 12:01 = %+v", m)
	}
}

// 1c: every 10 min, one cpu-load print 3 s after a Tier 0 read, stored
// apart from the quiet-window samples.
func TestCPUPostSampleFollowsATier0Read(t *testing.T) {
	h := newSamplerHarness(t, "sw")
	h.cs.iv[devclass.TierCPU] = 0 // the post-poll sample on its own

	posts := func() int {
		n := 0
		for _, c := range h.calls("sw") {
			if c == tier1cCall {
				n++
			}
		}
		return n
	}

	// Due from the start, but there is no counter read to follow yet.
	h.tickAt(at(12, 0, 20))
	if posts() != 0 {
		t.Fatal("post-poll sample without a Tier 0 read")
	}
	h.setTier0("sw", at(12, 0, 30))
	if wake := h.tickAt(at(12, 0, 31)); posts() != 0 || !wake.Equal(at(12, 0, 33)) {
		t.Fatalf("1 s after the read: posts %d, wake %v; want none and a wake-up at 12:00:33", posts(), wake)
	}
	h.tickAt(at(12, 0, 33))
	if got := h.calls("sw"); len(got) != 1 || got[0] != tier1cCall {
		t.Fatalf("calls = %q, want exactly the one-field resource print", got)
	}

	// Not again for 10 minutes, whatever is read meanwhile.
	for ts := at(12, 0, 45); ts.Before(at(12, 10, 30)); ts = ts.Add(15 * time.Second) {
		h.setTier0("sw", ts)
		h.tickAt(ts.Add(3 * time.Second))
	}
	if posts() != 1 {
		t.Fatalf("post-poll samples within 10 min = %d, want 1", posts())
	}
	// Due again at 12:10:33. A read the loop only sees long after it ended
	// is not followed: the sample would no longer show its effect.
	h.setTier0("sw", at(12, 10, 20))
	h.tickAt(at(12, 10, 40))
	if posts() != 1 {
		t.Fatal("followed a Tier 0 read 20 s after it ended")
	}
	h.setTier0("sw", at(12, 10, 45))
	h.tickAt(at(12, 10, 48))
	if posts() != 2 {
		t.Fatalf("post-poll samples = %d, want the second one after the next read", posts())
	}
	if next := h.s.reg.NextRuns("sw")["cpu_post"]; !next.Equal(at(12, 20, 48)) {
		t.Errorf("next cpu_post run = %v, want 10 min after the last", next)
	}

	h.load.FlushMinutes(at(12, 11, 0), nil)
	if m := minuteRow(t, h.db, "sw", at(12, 0, 0)); m.CPUPost != nullInt(12) || m.CPUSamples != 0 {
		t.Errorf("12:00 = %+v, want cpu_post only", m)
	}
	if m := minuteRow(t, h.db, "sw", at(12, 5, 0)); m.CPUPost.Valid {
		t.Errorf("12:05 = %+v, want no post-poll sample", m)
	}
}

// The quiet-window sample counts the sampler's own commands too: it never
// follows the post-poll sample by less than 3 s.
func TestCPUSampleWaitsAfterThePostPollSample(t *testing.T) {
	h := newSamplerHarness(t, "sw")
	h.tickAt(at(12, 0, 20))
	h.setTier0("sw", at(12, 0, 57))
	h.tickAt(at(12, 1, 0)) // the post-poll sample and the slot fall together
	if got := h.calls("sw"); len(got) != 1 || got[0] != tier1cCall {
		t.Fatalf("calls = %q, want the post-poll sample first", got)
	}
	h.tickAt(at(12, 1, 2))
	if n := len(h.calls("sw")); n != 1 {
		t.Fatal("the quiet-window sample ran 2 s after the post-poll sample")
	}
	h.tickAt(at(12, 1, 3))
	if got := h.calls("sw"); len(got) != 2 || got[1] != tier1Call {
		t.Fatalf("calls = %q, want the quiet-window sample 3 s later", got)
	}
}

// The whole budget of this loop: one cheap command per device per minute
// plus one per 10 minutes, and nothing else on the wire.
func TestCPUSamplerCommandBudget(t *testing.T) {
	h := newSamplerHarness(t, "sw")
	h.tickAt(at(12, 0, 20))
	// Half an hour, the loop waking every second, a counter read every 15 s.
	for ts := at(12, 0, 21); ts.Before(at(12, 30, 21)); ts = ts.Add(time.Second) {
		if ts.Second()%15 == 5 {
			h.setTier0("sw", ts)
			h.setCmdEnd("sw", ts)
		}
		h.tickAt(ts)
	}
	var quiet, post int
	for _, c := range h.calls("sw") {
		switch c {
		case tier1Call:
			quiet++
		case tier1cCall:
			post++
		default:
			t.Errorf("unexpected command %q", c)
		}
	}
	if quiet != 30 || post != 3 {
		t.Errorf("30 minutes cost %d quiet-window and %d post-poll samples, want 30 and 3", quiet, post)
	}
}

// End to end on a pooled session: the sample is one command on the wire, and
// the observer counts it — with the reconnect that followed a dropped
// session — in the device's minute.
func TestCPUSamplerOnAPooledSessionFeedsTheSelfMetrics(t *testing.T) {
	db := pollerTestDB(t)
	fake := routerostest.NewDevice(t)
	fake.SetReply("/system/resource/print", map[string]string{"cpu-load": "33", "free-memory": "1048576", "uptime": "5m"})
	mustCreateDevice(t, db, &queries.Device{ID: "sw", Identity: "sw", Address: fake.Host, APIPort: fake.Port, Status: "online"})

	load := NewLoadRecorder(db)
	start := time.Now().UTC()
	load.started = start
	routeros.SetObserver(load)
	t.Cleanup(func() { routeros.SetObserver(nil) })

	pool := routeros.NewPool(false)
	fake.Dial(t, pool, "sw")
	pool.Close("sw")
	fake.Dial(t, pool, "sw") // a reconnect

	s := NewCPUSampler(db, pool, ws.NewHub(), load, nil)
	s.spawn = func(f func()) { f() }
	s.reg = NewScheduleRegistry()
	s.postClock = newTierClock(s.reg)
	s.tick(start)                  // first sight
	s.tick(start.Add(time.Minute)) // the next slot: nothing ran since, so it is quiet

	if got := fake.Commands(); len(got) != 1 || got[0] != "/system/resource/print" {
		t.Fatalf("device commands = %q, want one resource print", got)
	}
	if args := fake.Args(); strings.Join(args[0], " ") != "=.proplist=cpu-load,free-memory,uptime" {
		t.Errorf("args = %q", args[0])
	}
	if end := load.LastCommandEnd("sw"); end.Before(start) {
		t.Errorf("LastCommandEnd = %v, want the sample's end", end)
	}

	load.FlushMinutes(time.Now().UTC().Add(2*time.Minute), nil)
	rows, err := queries.ListDeviceMinutes(db, "sw", start.Add(-time.Minute), start.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var cmds, reconnects, samples, cpu int64
	for _, m := range rows {
		cmds += m.NMSCmds
		reconnects += m.NMSReconnects
		samples += m.CPUSamples
		cpu += m.CPUSum
	}
	if cmds != 1 || reconnects != 1 || samples != 1 || cpu != 33 {
		t.Errorf("recorded cmds %d reconnects %d samples %d cpu %d, want 1 / 1 / 1 / 33", cmds, reconnects, samples, cpu)
	}
}
