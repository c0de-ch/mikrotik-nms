package poller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// testClock is a settable clock safe to read from poll goroutines.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (k *testClock) now() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.t
}

func (k *testClock) set(t time.Time) {
	k.mu.Lock()
	k.t = t
	k.mu.Unlock()
}

// The exact sentences Tier 0 and Tier 4a put on the wire.
const (
	tier0Call = "/interface/print =.proplist=.id,name,type,mac-address,mtu,running,disabled,slave,comment," +
		"last-link-up-time,last-link-down-time,link-downs,rx-byte,tx-byte,rx-packet,tx-packet,rx-error,tx-error," +
		"rx-drop,tx-drop,tx-queue-drop"
	tier4aCall = "/interface/ethernet/print =.proplist=name,switch,loop-protect,loop-protect-status," +
		"rx-flow-control,tx-flow-control,tx-drop-packet,tx-drop-byte,rx-overflow,rx-fcs-error,rx-pause,tx-pause," +
		"driver-rx-packet,driver-tx-packet,rx-broadcast,rx-multicast"
)

// switchRunner scripts a small switch: two ether ports and a bridge.
func switchRunner(rx string) *fakeRunner {
	return &fakeRunner{byCmd: map[string]fakeReply{
		"/interface/print": rep(
			map[string]string{".id": "*1", "name": "ether1", "type": "ether", "mac-address": "02:00:00:00:00:01", "mtu": "1500",
				"running": "true", "disabled": "false", "link-downs": "3", "rx-byte": rx, "tx-byte": "0"},
			map[string]string{".id": "*2", "name": "ether2", "type": "ether", "mac-address": "02:00:00:00:00:02", "mtu": "1500",
				"running": "false", "disabled": "false", "slave": "true", "comment": "spare", "link-downs": "0", "rx-byte": "0", "tx-byte": "0"},
			map[string]string{".id": "*A", "name": "bridge", "type": "bridge", "mac-address": "02:00:00:00:00:01", "mtu": "1500",
				"running": "true", "disabled": "false", "rx-byte": "0", "tx-byte": "0"},
		),
		"/interface/ethernet/print": rep(
			map[string]string{"name": "ether1", "switch": "switch1", "loop-protect": "on", "loop-protect-status": "on", "rx-fcs-error": "4"},
			map[string]string{"name": "ether2", "switch": "switch1", "loop-protect": "on", "loop-protect-status": "on"},
		),
	}}
}

func newTestPortStats(t *testing.T, run routeros.Runner) (*PortStatsCollector, *testClock) {
	t.Helper()
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "sw", Identity: "sw", Address: "192.0.2.10"})
	c := NewPortStatsCollector(db, nil, nil)
	clock := &testClock{t: at(12, 0, 5)}
	c.now = clock.now
	c.runner = func(string) routeros.Runner { return run }
	return c, clock
}

// One cycle at a time: Tier 0 every counter interval, Tier 4a only every
// ethernetInterval, each one proplisted print — and every command counted.
func TestPortStatsCycleRunsTier0AndTier4aOnTheirCadence(t *testing.T) {
	run := switchRunner("1000")
	c, clock := newTestPortStats(t, run)
	const iv = 15 * time.Second

	next := c.cycle(context.Background(), iv)
	if len(run.calls) != 2 || run.calls[0] != tier0Call || run.calls[1] != tier4aCall {
		t.Fatalf("first cycle calls = %q, want the Tier 0 then the Tier 4a print", run.calls)
	}
	if !next.Equal(at(12, 0, 20)) {
		t.Errorf("next wake-up = %v, want one interval after the cycle start", next)
	}

	// 15 s later only Tier 0 is due.
	for i := 1; i <= 19; i++ {
		clock.set(at(12, 0, 5).Add(time.Duration(i) * iv))
		c.cycle(context.Background(), iv)
	}
	if got := run.countCalls("/interface/print"); got != 20 {
		t.Errorf("Tier 0 prints = %d over 20 cycles, want 20", got)
	}
	if got := run.countCalls("/interface/ethernet/print"); got != 1 {
		t.Errorf("Tier 4a prints = %d within 5 min, want 1", got)
	}
	clock.set(at(12, 5, 5))
	c.cycle(context.Background(), iv)
	if got := run.countCalls("/interface/ethernet/print"); got != 2 {
		t.Errorf("Tier 4a prints = %d after 5 min, want 2", got)
	}
	for _, call := range run.calls {
		if call != tier0Call && call != tier4aCall {
			t.Errorf("unexpected command %q", call)
		}
	}

	// 12:05:05 is in the minute after 12:04, which saw 4 Tier 0 prints.
	d, ok := c.DeviceIfaces("sw")
	if !ok || d.CommandsLastMinute != 4 {
		t.Errorf("CommandsLastMinute = %d (ok %v), want 4", d.CommandsLastMinute, ok)
	}
	if !d.EthernetAt.Equal(at(12, 5, 5)) {
		t.Errorf("EthernetAt = %v", d.EthernetAt)
	}
}

// DeviceSnapshot serves the last Tier 0 read with the Tier 4a rows merged by
// name, as a copy, and stops serving it once it is older than 3× the
// device's interval.
func TestPortStatsDeviceSnapshot(t *testing.T) {
	var nilCollector *PortStatsCollector
	if _, _, ok := nilCollector.DeviceSnapshot("sw"); ok {
		t.Fatal("a nil collector has no snapshot")
	}

	c, clock := newTestPortStats(t, switchRunner("1000"))
	if _, _, ok := c.DeviceSnapshot("sw"); ok {
		t.Fatal("no snapshot before the first read")
	}
	const iv = 15 * time.Second
	c.cycle(context.Background(), iv)

	rows, readAt, ok := c.DeviceSnapshot("sw")
	if !ok || !readAt.Equal(at(12, 0, 5)) || len(rows) != 3 {
		t.Fatalf("snapshot ok=%v at=%v rows=%d", ok, readAt, len(rows))
	}
	e1, e2, br := rows[0], rows[1], rows[2]
	if e1.Name != "ether1" || e1.MAC != "02:00:00:00:00:01" || e1.MTU != 1500 || e1.LinkDowns != 3 || !e1.HasLinkDowns ||
		!e1.HasEthernet || e1.Ethernet.LoopProtectStatus != "on" || e1.Ethernet.RxFCSErrors != 4 || e1.Ethernet.Switch != "switch1" {
		t.Errorf("ether1 = %+v", e1)
	}
	if !e2.Slave || e2.Comment != "spare" || e2.Running || !e2.HasEthernet {
		t.Errorf("ether2 = %+v", e2)
	}
	if br.Name != "bridge" || br.HasEthernet {
		t.Errorf("bridge = %+v: no ethernet row to merge", br)
	}

	rows[0].Name = "mutated"
	if again, _, _ := c.DeviceSnapshot("sw"); again[0].Name != "ether1" {
		t.Error("DeviceSnapshot must return a copy")
	}

	clock.set(at(12, 0, 50)) // 45 s = 3× the interval: still served
	if _, _, ok := c.DeviceSnapshot("sw"); !ok {
		t.Error("a snapshot 3 intervals old is still fresh")
	}
	clock.set(at(12, 0, 51))
	if _, _, ok := c.DeviceSnapshot("sw"); ok {
		t.Error("a snapshot older than 3 intervals must not be served")
	}
	if _, ok := c.DeviceIfaces("sw"); ok {
		t.Error("DeviceIfaces must agree")
	}
}

// The ethernet rows expire on their own clock: a Tier 0 read keeps the
// snapshot fresh, but a stale Tier 4a read is no longer merged.
func TestPortStatsStaleEthernetNotMerged(t *testing.T) {
	c, clock := newTestPortStats(t, switchRunner("1000"))
	c.tierInterval = func(_ queries.Device, tier devclass.Tier, _ time.Time) time.Duration {
		if tier == devclass.TierCounters {
			return 15 * time.Second
		}
		return time.Minute // Tier 4a every minute for the test
	}
	c.cycle(context.Background(), 15*time.Second)

	c.ingest("sw", []routeros.InterfaceStats{{Name: "ether1", Type: "ether"}}, at(12, 3, 4), 15*time.Second)
	clock.set(at(12, 3, 5))
	rows, _, ok := c.DeviceSnapshot("sw")
	if !ok || len(rows) != 1 || !rows[0].HasEthernet {
		t.Fatalf("ethernet read 3 min old at a 1 min cadence must still merge: %+v", rows)
	}
	clock.set(at(12, 3, 6))
	c.ingest("sw", []routeros.InterfaceStats{{Name: "ether1", Type: "ether"}}, at(12, 3, 6), 15*time.Second)
	if rows, _, _ := c.DeviceSnapshot("sw"); len(rows) != 1 || rows[0].HasEthernet {
		t.Fatalf("a stale ethernet read must not merge: %+v", rows)
	}
}

// A device that does not answer Tier 0 is not asked for Tier 4a, and Tier
// 4a is due again at the next wake-up rather than five minutes later.
func TestPortStatsTier0FailureDefersEthernet(t *testing.T) {
	run := switchRunner("1000")
	run.byCmd["/interface/print"] = fakeReply{err: errors.New("i/o timeout")}
	c, clock := newTestPortStats(t, run)
	const iv = 15 * time.Second

	c.cycle(context.Background(), iv)
	if len(run.calls) != 1 || run.calls[0] != tier0Call {
		t.Fatalf("calls = %q, want only the failed Tier 0 print", run.calls)
	}
	run.byCmd["/interface/print"] = switchRunner("1000").byCmd["/interface/print"]
	clock.set(at(12, 0, 20))
	c.cycle(context.Background(), iv)
	if got := run.countCalls("/interface/ethernet/print"); got != 1 {
		t.Errorf("Tier 4a prints after the recovery = %d, want 1", got)
	}
}

// The interfaces table follows the Tier 0 read: every row on the first
// read, then only changed rows (and all of them hourly); interfaces gone
// from the print are pruned, and an empty print never wipes the device.
func TestPortStatsInventorySync(t *testing.T) {
	run := switchRunner("1000")
	c, clock := newTestPortStats(t, run)
	const iv = 15 * time.Second
	list := func() map[string]queries.Interface {
		t.Helper()
		rows, err := queries.ListInterfacesByDevice(c.db, "sw")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]queries.Interface{}
		for _, r := range rows {
			out[r.Name] = r
		}
		return out
	}

	// A row the device no longer has, from before the restart.
	if err := queries.UpsertInterface(c.db, &queries.Interface{ID: "sw:wifi9", DeviceID: "sw", Name: "wifi9"}); err != nil {
		t.Fatal(err)
	}
	c.cycle(context.Background(), iv)
	got := list()
	if len(got) != 3 || got["wifi9"].Name != "" {
		t.Fatalf("after the first read: %+v, want the 3 printed interfaces and wifi9 pruned", got)
	}
	e2 := got["ether2"]
	if e2.ID != "sw:ether2" || e2.Type != "ether" || e2.MACAddress != "02:00:00:00:00:02" || e2.MTU == nil || *e2.MTU != 1500 ||
		e2.Running || e2.Comment != "spare" {
		t.Errorf("ether2 row = %+v", e2)
	}

	// An unchanged read writes nothing: a row edited behind the
	// collector's back stays as it is...
	tampered := queries.Interface{ID: "sw:ether1", DeviceID: "sw", Name: "ether1", Type: "ether", Comment: "tampered"}
	if err := queries.UpsertInterface(c.db, &tampered); err != nil {
		t.Fatal(err)
	}
	clock.set(at(12, 0, 20))
	c.cycle(context.Background(), iv)
	if got := list(); got["ether1"].Comment != "tampered" {
		t.Fatalf("an unchanged read rewrote ether1: %+v", got["ether1"])
	}

	// ...until a field of that row changes on the device.
	run.byCmd["/interface/print"] = rep(
		map[string]string{".id": "*1", "name": "ether1", "type": "ether", "mac-address": "02:00:00:00:00:01", "mtu": "1500",
			"running": "false", "disabled": "false", "rx-byte": "1000", "tx-byte": "0"},
		map[string]string{".id": "*A", "name": "bridge", "type": "bridge", "mac-address": "02:00:00:00:00:01", "mtu": "1500",
			"running": "true", "disabled": "false"},
	)
	clock.set(at(12, 0, 35))
	c.cycle(context.Background(), iv)
	got = list()
	if got["ether1"].Running || got["ether1"].Comment != "" {
		t.Errorf("changed ether1 = %+v, want rewritten from the print", got["ether1"])
	}
	if _, ok := got["ether2"]; ok || len(got) != 2 {
		t.Errorf("ether2 left the print and must be pruned: %+v", got)
	}

	// An empty print changes nothing.
	run.byCmd["/interface/print"] = rep()
	clock.set(at(12, 0, 50))
	c.cycle(context.Background(), iv)
	if got := list(); len(got) != 2 {
		t.Errorf("an empty print touched the table: %+v", got)
	}
}

func TestPortStatsInventoryHourlyRewrite(t *testing.T) {
	run := switchRunner("1000")
	c, clock := newTestPortStats(t, run)
	const iv = 15 * time.Second
	c.cycle(context.Background(), iv)
	if err := queries.UpsertInterface(c.db, &queries.Interface{ID: "sw:ether1", DeviceID: "sw", Name: "ether1", Comment: "tampered"}); err != nil {
		t.Fatal(err)
	}
	clock.set(at(13, 0, 5))
	c.cycle(context.Background(), iv)
	rows, _ := queries.ListInterfacesByDevice(c.db, "sw")
	for _, r := range rows {
		if r.Name == "ether1" && (r.Comment != "" || r.Type != "ether") {
			t.Errorf("hourly rewrite missed ether1: %+v", r)
		}
	}
}

func TestCountCommandPerMinute(t *testing.T) {
	c := NewPortStatsCollector(nil, nil, nil)
	clock := &testClock{t: at(12, 0, 0)}
	c.now = clock.now
	count := func(ts time.Time, n int) {
		for range n {
			c.countCommand("sw", ts)
		}
	}
	commands := func(ts time.Time) int {
		clock.set(ts)
		d, _ := c.DeviceIfaces("sw")
		return d.CommandsLastMinute
	}

	count(at(12, 0, 10), 3)
	count(at(12, 0, 50), 2)
	if got := commands(at(12, 0, 55)); got != 0 {
		t.Errorf("during the first minute = %d, want 0 (no complete minute yet)", got)
	}
	if got := commands(at(12, 1, 30)); got != 5 {
		t.Errorf("in 12:01 = %d, want 12:00's 5", got)
	}
	count(at(12, 1, 40), 7)
	if got := commands(at(12, 1, 45)); got != 5 {
		t.Errorf("12:01 still running = %d, want 12:00's 5", got)
	}
	if got := commands(at(12, 2, 1)); got != 7 {
		t.Errorf("in 12:02 = %d, want 12:01's 7", got)
	}
	if got := commands(at(12, 3, 1)); got != 0 {
		t.Errorf("after a quiet minute = %d, want 0", got)
	}
	count(at(12, 9, 0), 1) // after a gap the old counts are gone
	if got := commands(at(12, 9, 59)); got != 0 {
		t.Errorf("first minute after a gap = %d, want 0", got)
	}
}

// The FDB and stacking reads run through the counted link too.
func TestPortStatsHostsLoopCountsCommands(t *testing.T) {
	run := &fakeRunner{byCmd: map[string]fakeReply{
		"/interface/bridge/host/print":  rep(map[string]string{"mac-address": "02:00:00:00:01:01", "on-interface": "ether1", "bridge": "bridge"}),
		"/interface/bonding/print":      rep(),
		"/interface/vlan/print":         rep(),
		"/interface/pppoe-client/print": rep(),
	}}
	c, clock := newTestPortStats(t, run)
	c.collectHosts(context.Background(), nil)
	if len(run.calls) != 4 || !strings.HasPrefix(run.calls[0], "/interface/bridge/host/print ?local=false") {
		t.Fatalf("calls = %q, want the FDB dump then the three stacking prints", run.calls)
	}
	clock.set(at(12, 1, 0))
	if d, _ := c.DeviceIfaces("sw"); d.CommandsLastMinute != 4 {
		t.Errorf("CommandsLastMinute = %d, want 4", d.CommandsLastMinute)
	}
}
