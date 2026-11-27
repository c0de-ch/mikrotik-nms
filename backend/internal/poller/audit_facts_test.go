package poller

import (
	"net/netip"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// The offload rules need hw (offload requested) and horizon next to
// hw-offload. The bridge port print already returns both; the network-health
// read stores them — no command of the audit's own.
func TestBridgeReadStoresHWAndHorizonForTheAudit(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "sw", Identity: "sw", Address: "192.0.2.1", ROSVersion: "7.24.5"})
	run := &fakeRunner{byCmd: map[string]fakeReply{
		"/interface/bridge/print": rep(map[string]string{".id": "*1", "name": "bridge", "protocol-mode": "rstp", "running": "true"}),
		"/interface/bridge/port/print": rep(
			map[string]string{".id": "*1", "bridge": "bridge", "interface": "ether1", "hw": "true", "hw-offload": "false", "horizon": "none",
				"status": "in-bridge", "role": "designated-port", "forwarding": "true", "learning": "true", "topology-changes": "0"},
			map[string]string{".id": "*2", "bridge": "bridge", "interface": "ether2", "hw": "false", "hw-offload": "false", "horizon": "3",
				"status": "in-bridge", "role": "designated-port", "forwarding": "true", "learning": "true", "topology-changes": "0"},
			// A RouterOS that does not say: unknown stays unknown.
			map[string]string{".id": "*3", "bridge": "bridge", "interface": "wifi1",
				"status": "in-bridge", "role": "designated-port", "forwarding": "true", "learning": "true", "topology-changes": "0"},
		),
		"/interface/bridge/monitor": rep(map[string]string{"bridge-id": "0x8000.02:00:00:00:00:01", "root-bridge": "yes", "port-count": "3"}),
	}}
	n := NewNetworkHealthPoller(db, nil, ws.NewHub(), time.Minute)
	dev, _ := queries.GetDevice(db, "sw")
	if !n.readBridges(*dev, run, at(12, 0, 0), 100) {
		t.Fatalf("the bridge read failed (calls: %v)", run.calls)
	}
	ports, err := queries.ListBridgePortStatusByDevice(db, "sw")
	if err != nil || len(ports) != 3 {
		t.Fatalf("ports = %+v, %v", ports, err)
	}
	by := map[string]queries.BridgePortStatus{}
	for _, p := range ports {
		by[p.PortInterface] = p
	}
	if p := by["ether1"]; p.HW == nil || !*p.HW || p.HWOffload == nil || *p.HWOffload || p.Horizon != "none" {
		t.Errorf("ether1 = %+v", p)
	}
	if p := by["ether2"]; p.HW == nil || *p.HW || p.Horizon != "3" {
		t.Errorf("ether2 = %+v", p)
	}
	if p := by["wifi1"]; p.HW != nil || p.HWOffload != nil || p.Horizon != "" {
		t.Errorf("wifi1 = %+v, want hw and hw-offload unknown", p)
	}
}

// "Carries traffic" is the port's rate over its last counter interval — and
// only a rate that rests on two comparable readings: a first read, a counter
// reset and a gap leave the port without one.
func TestDeviceRatesOnlyFromTwoComparableReadings(t *testing.T) {
	c := NewPortStatsCollector(nil, nil, nil)
	const iv = 15 * time.Second
	ingest := func(ts time.Time, rows ...routeros.InterfaceStats) { c.ingest("d1", rows, ts, iv) }

	ingest(at(12, 0, 0), ifStats("ether1", 1_000, 0, 10, 0), ifStats("ether2", 5_000, 5_000, 50, 50))
	if got := c.DeviceRates("d1"); len(got) != 0 {
		t.Fatalf("rates after one reading: %v", got)
	}
	ingest(at(12, 0, 10), ifStats("ether1", 13_500, 2_500, 110, 5), ifStats("ether2", 5_000, 5_000, 50, 50))
	got := c.DeviceRates("d1")
	if got["ether1"] != 12_000 { // 10,000 in + 2,000 out bits per second
		t.Errorf("ether1 = %d bps, want 12000", got["ether1"])
	}
	if rate, ok := got["ether2"]; !ok || rate != 0 { // idle is a rate too
		t.Errorf("ether2 = %d, %v; want a known rate of 0", rate, ok)
	}
	// A counter that went backwards (a reboot): no rate until the next pair.
	ingest(at(12, 0, 20), ifStats("ether1", 500, 100, 1, 1), ifStats("ether2", 5_000, 5_000, 50, 50))
	if _, ok := c.DeviceRates("d1")["ether1"]; ok {
		t.Error("ether1 has a rate right after its counters were reset")
	}
	// A gap longer than three intervals: the same.
	ingest(at(12, 5, 0), ifStats("ether1", 900, 100, 1, 1), ifStats("ether2", 6_000, 5_000, 50, 50))
	if got := c.DeviceRates("d1"); len(got) != 0 {
		t.Errorf("rates across a gap: %v", got)
	}
	if got := c.DeviceRates("other"); len(got) != 0 {
		t.Errorf("another device's rates: %v", got)
	}
	var none *PortStatsCollector
	if got := none.DeviceRates("d1"); len(got) != 0 {
		t.Errorf("a nil collector has rates: %v", got)
	}
}

// The addresses the NMS's sessions come from: the outbound address towards
// the device first, then the configured ones.
func TestLoginWatchSelfAddresses(t *testing.T) {
	db := pollerTestDB(t)
	if err := queries.SetSetting(db, "nms_source_addresses", "198.51.100.5, 192.0.2.10, junk"); err != nil {
		t.Fatal(err)
	}
	w := NewLoginWatch(db)
	w.outboundFn = func(string, int) (addr netip.Addr, ok bool) { return netip.MustParseAddr("192.0.2.10"), true }
	dev := &queries.Device{ID: "d", Address: "192.0.2.1", APIPort: 8728}
	got := w.SelfAddresses(dev, at(12, 0, 0))
	if len(got) != 2 || got[0] != netip.MustParseAddr("192.0.2.10") || got[1] != netip.MustParseAddr("198.51.100.5") {
		t.Fatalf("self addresses = %v", got)
	}
	// Nothing known: nothing claimed.
	if err := queries.SetSetting(db, "nms_source_addresses", ""); err != nil {
		t.Fatal(err)
	}
	w2 := NewLoginWatch(db)
	w2.outboundFn = func(string, int) (netip.Addr, bool) { return netip.Addr{}, false }
	if got := w2.SelfAddresses(dev, at(12, 0, 0)); len(got) != 0 {
		t.Fatalf("self addresses without any source = %v", got)
	}
}
