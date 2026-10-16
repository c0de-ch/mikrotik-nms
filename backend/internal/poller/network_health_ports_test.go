package poller

import (
	"context"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// fakeSnapshots serves canned interface snapshots and records who asked.
type fakeSnapshots struct {
	rows  map[string][]routeros.InterfaceSnapshot
	at    time.Time
	asked map[string]int
}

func (f *fakeSnapshots) DeviceSnapshot(id string) ([]routeros.InterfaceSnapshot, time.Time, bool) {
	f.asked[id]++
	r, ok := f.rows[id]
	return r, f.at, ok
}

// The port monitor runs from the shared interface snapshot: with a pool that
// holds no connection at all (so no device command can be issued), a
// link-downs delta between two snapshots still records a port_link_flap and
// the interface_state rows.
func TestNetworkHealthPortMonitorReadsSnapshot(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "sw", Identity: "sw", Address: "192.0.2.10"})
	mustCreateDevice(t, db, &queries.Device{ID: "cold", Identity: "cold", Address: "192.0.2.11"})
	mustCreateDevice(t, db, &queries.Device{ID: "off", Identity: "off", Address: "192.0.2.12", Status: "offline"})

	n := NewNetworkHealthPoller(db, routeros.NewPool(false), ws.NewHub(), time.Minute)
	t0 := time.Now().UTC().Truncate(time.Second)
	src := &fakeSnapshots{
		rows:  map[string][]routeros.InterfaceSnapshot{"sw": {linkRow("ether1", true, 10), linkRow("ether2", false, 0)}},
		at:    t0,
		asked: map[string]int{},
	}
	n.UseInterfaceSnapshots(src)

	n.poll(context.Background())
	states, err := queries.ListInterfaceStates(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].InterfaceName != "ether1" || states[1].InterfaceName != "ether2" {
		t.Fatalf("interface_state = %+v, want ether1 and ether2 of sw", states)
	}
	if src.asked["off"] != 0 {
		t.Error("an offline device's snapshot must not be read")
	}
	if src.asked["cold"] != 1 {
		t.Errorf("a device without a snapshot is asked once and skipped, asked %d", src.asked["cold"])
	}

	// Three link-downs between the two snapshots, the port reading running
	// both times: the flap happened between reads.
	src.rows["sw"] = []routeros.InterfaceSnapshot{linkRow("ether1", true, 13), linkRow("ether2", false, 0)}
	src.at = t0.Add(15 * time.Second)
	n.poll(context.Background())

	events, err := queries.ListLoopEvents(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventType != "port_link_flap" || events[0].DeviceID != "sw" ||
		events[0].PortInterface != "ether1" || events[0].Severity != "critical" {
		t.Fatalf("loop events = %+v, want one critical port_link_flap on sw/ether1", events)
	}
	states, _ = queries.ListInterfaceStates(db)
	if states[0].FlapCountWindow != 3 {
		t.Errorf("ether1 flap_count_window = %d, want 3", states[0].FlapCountWindow)
	}

	// Without a snapshot source the port monitor does not run (it never
	// falls back to a device print).
	quiet := NewNetworkHealthPoller(db, routeros.NewPool(false), ws.NewHub(), time.Minute)
	if events, ports := quiet.monitorPorts(queries.Device{ID: "sw"}, portMonitorDefaults); events != 0 || ports != 0 {
		t.Errorf("monitorPorts without a source = %d events, %d ports", events, ports)
	}
}
