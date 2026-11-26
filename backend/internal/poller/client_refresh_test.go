package poller

import (
	"errors"
	"testing"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/leasesource"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// newTestRefresher is a refresher over scripted devices and a test clock,
// with no external lease source.
func newTestRefresher(t *testing.T, runners map[string]*fakeRunner) (*ClientRefresher, *testClock) {
	t.Helper()
	routeros.SetDialPolicy(nil)
	t.Cleanup(func() { routeros.SetDialPolicy(nil) })
	db := pollerTestDB(t)
	cr := NewClientRefresher(db, nil, nil, NewCapabilityCache(), NewCadence(db, nil))
	clock := &testClock{t: at(12, 0, 0)}
	cr.now = clock.now
	cr.runner = scriptedRunners(runners)
	cr.external = func() []leasesource.Lease { return nil }
	return cr, clock
}

func count(ret string) fakeReply { return fakeReply{done: map[string]string{"ret": ret}} }

// A refresh reads a device only as its plan allows: ARP everywhere, DHCP
// leases on C3 only, registration tables only from the menus the capability
// probe found (a switch is never asked for one), and nothing at all on a
// paused or offline device.
func TestClientRefreshReadsByPlanAndCapabilities(t *testing.T) {
	router := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/arp/print": rep(
			map[string]string{"address": "192.0.2.10", "mac-address": "02:00:00:00:00:0a", "interface": "vlan10"},
			map[string]string{"address": "192.0.2.11", "mac-address": "02:00:00:00:00:0B", "interface": "vlan10"},
		),
		"/ip/dhcp-server/lease/print": rep(
			map[string]string{"address": "192.0.2.10", "mac-address": "02:00:00:00:00:0A", "host-name": "laptop", "server": "dhcp10"},
		),
		routeros.WifiProbeCommand:       count("2"),
		routeros.DHCPServerProbeCommand: count("1"),
		routeros.BridgeProbeCommand:     count("1"),
		"/interface/wifi/registration-table/print": rep(
			map[string]string{"mac-address": "02:00:00:00:00:0B", "interface": "ap1/wifi1", "ssid": "lab", "band": "5ghz-ax", "signal": "-55", "last-ip": "192.0.2.11"},
			// A wired-looking row without any wireless field is not a client.
			map[string]string{"mac-address": "02:00:00:00:00:0C", "interface": "ether5"},
		),
	}}
	arpOnly := func(mac string) *fakeRunner {
		return &fakeRunner{byCmd: map[string]fakeReply{
			"/ip/arp/print":                 rep(map[string]string{"address": "192.0.2.50", "mac-address": mac}),
			"/ip/dhcp-server/lease/print":   rep(map[string]string{"address": "192.0.2.99", "mac-address": "02:00:00:00:00:99"}),
			routeros.DHCPServerProbeCommand: count("0"),
			routeros.BridgeProbeCommand:     count("1"),
		}}
	}
	runners := map[string]*fakeRunner{
		"r3": router, "s2": arpOnly("02:00:00:00:00:52"), "s1": arpOnly("02:00:00:00:00:51"),
		"held": arpOnly("02:00:00:00:00:61"), "down": arpOnly("02:00:00:00:00:62"),
	}
	cr, _ := newTestRefresher(t, runners)
	classFleet(t, cr.db, map[string]devclass.Class{
		"r3": devclass.C3, "s2": devclass.C2, "s1": devclass.C1, "held": devclass.C3, "down": devclass.C3,
	}, "held")
	if err := queries.MarkDeviceUnreachable(cr.db, "down", "offline", nil); err != nil {
		t.Fatal(err)
	}

	st, err := cr.Start()
	if err != nil || !st.Running || st.StartedAt == nil || st.FinishedAt != nil {
		t.Fatalf("Start = %+v, %v; want a running refresh", st, err)
	}
	cr.jobs.Wait()

	if n := router.countCalls("/ip/arp/print") + router.countCalls("/ip/dhcp-server/lease/print") +
		router.countCalls("/interface/wifi/registration-table/print"); n != 3 {
		t.Errorf("the C3 router got %d of its 3 reads: %v", n, router.calls)
	}
	for _, id := range []string{"s2", "s1"} {
		r := runners[id]
		if r.countCalls("/ip/arp/print") != 1 {
			t.Errorf("%s: ARP reads = %d, want 1", id, r.countCalls("/ip/arp/print"))
		}
		if r.countCalls("/ip/dhcp-server/lease/print") != 0 {
			t.Errorf("%s: DHCP leases were read on a class whose plan has none", id)
		}
		for _, c := range r.calls {
			if c == "/interface/wifi/registration-table/print" || c == "/caps-man/registration-table/print" ||
				c == "/interface/wireless/registration-table/print" {
				t.Errorf("%s: a device without a wireless package was asked for %s", id, c)
			}
		}
	}
	for _, id := range []string{"held", "down"} {
		if len(runners[id].calls) != 0 {
			t.Errorf("%s (paused/offline) was read: %v", id, runners[id].calls)
		}
	}

	st = cr.Status()
	if st.Running || st.FinishedAt == nil || st.DevicesRead != 3 || st.DevicesSkipped != 2 || st.Clients != 4 {
		t.Fatalf("status after the refresh = %+v; want 3 read, 2 skipped, 4 clients", st)
	}

	laptop, err := queries.GetMACLookup(cr.db, "02:00:00:00:00:0A")
	if err != nil {
		t.Fatalf("laptop not cached: %v", err)
	}
	if laptop.HostName != "laptop" || laptop.Source != "dhcp" || laptop.IPAddress != "192.0.2.10" || laptop.DeviceID != "r3" {
		t.Errorf("laptop = %+v; want the ARP row enriched by its lease", laptop)
	}
	phone, err := queries.GetMACLookup(cr.db, "02:00:00:00:00:0B")
	if err != nil {
		t.Fatalf("phone not cached: %v", err)
	}
	if phone.Source != "wifi" || phone.AP != "ap1" || phone.SSID != "lab" || phone.Signal == "" {
		t.Errorf("phone = %+v; want its wifi registration merged in", phone)
	}
	if _, err := queries.GetMACLookup(cr.db, "02:00:00:00:00:0C"); err == nil {
		t.Error("a wired registration row was cached as a client")
	}
	if _, err := queries.GetMACLookup(cr.db, "02:00:00:00:00:99"); err == nil {
		t.Error("a lease from a device whose plan has no DHCP read was cached")
	}
}

// One refresh per five minutes, counted from its start; while one runs a
// second is refused as running.
func TestClientRefreshRateLimit(t *testing.T) {
	gate := make(chan struct{})
	slow := &blockingRunner{fakeRunner: fakeRunner{byCmd: map[string]fakeReply{"/ip/arp/print": rep()}}, gate: gate}
	cr, clock := newTestRefresher(t, nil)
	cr.runner = func(string) routeros.Runner { return slow }
	classFleet(t, cr.db, map[string]devclass.Class{"s1": devclass.C1})

	if st := cr.Status(); st.Running || st.StartedAt != nil || st.NextAllowedAt != nil ||
		st.MinIntervalSeconds != 300 {
		t.Fatalf("status before any refresh = %+v", st)
	}
	if _, err := cr.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if st, err := cr.Start(); !errors.Is(err, ErrClientRefreshRunning) || !st.Running {
		t.Fatalf("Start while running = %+v, %v; want ErrClientRefreshRunning", st, err)
	}
	close(gate)
	cr.jobs.Wait()

	clock.set(at(12, 4, 59))
	st, err := cr.Start()
	if !errors.Is(err, ErrClientRefreshTooSoon) {
		t.Fatalf("Start after 4m59s = %v; want ErrClientRefreshTooSoon", err)
	}
	if st.NextAllowedAt == nil || !st.NextAllowedAt.Equal(at(12, 5, 0)) {
		t.Fatalf("next_allowed_at = %v, want 12:05:00", st.NextAllowedAt)
	}
	if slow.countCalls("/ip/arp/print") != 1 {
		t.Fatalf("a refused refresh read the device: %v", slow.calls)
	}

	clock.set(at(12, 5, 0))
	if _, err := cr.Start(); err != nil {
		t.Fatalf("Start after 5 min: %v", err)
	}
	cr.jobs.Wait()
	if slow.countCalls("/ip/arp/print") != 2 {
		t.Fatalf("ARP reads = %d, want 2", slow.countCalls("/ip/arp/print"))
	}
	if st := cr.Status(); st.NextAllowedAt == nil || !st.NextAllowedAt.Equal(at(12, 10, 0)) {
		t.Fatalf("next_allowed_at after the second refresh = %v, want 12:10:00", st.NextAllowedAt)
	}
}

// An instance that polls nothing refuses the refresh before touching a
// device, and it does not use up the interval.
func TestClientRefreshRefusedWhilePollersOff(t *testing.T) {
	dev := &fakeRunner{byCmd: map[string]fakeReply{"/ip/arp/print": rep()}}
	cr, _ := newTestRefresher(t, map[string]*fakeRunner{"s1": dev})
	classFleet(t, cr.db, map[string]devclass.Class{"s1": devclass.C1})

	routeros.SetDialPolicy(&routeros.DialPolicy{Enabled: false})
	st, err := cr.Start()
	if !errors.Is(err, routeros.ErrPollersOff) || st.Running || st.StartedAt != nil {
		t.Fatalf("Start with pollers off = %+v, %v; want ErrPollersOff and no refresh", st, err)
	}
	if len(dev.calls) != 0 {
		t.Fatalf("the device was read with pollers off: %v", dev.calls)
	}
}

// External leases (Kea / OPNsense) add clients no device reported and
// enrich the ones a device did.
func TestClientRefreshMergesExternalLeases(t *testing.T) {
	dev := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/arp/print": rep(map[string]string{"address": "192.0.2.20", "mac-address": "02:00:00:00:00:20"}),
	}}
	cr, _ := newTestRefresher(t, map[string]*fakeRunner{"s2": dev})
	classFleet(t, cr.db, map[string]devclass.Class{"s2": devclass.C2})
	cr.external = func() []leasesource.Lease {
		return []leasesource.Lease{
			{MAC: "02:00:00:00:00:20", IP: "192.0.2.20", Hostname: "printer", Origin: "kea"},
			{MAC: "02:00:00:00:00:21", IP: "192.0.2.21", Hostname: "camera", Origin: "kea"},
		}
	}
	if _, err := cr.Start(); err != nil {
		t.Fatal(err)
	}
	cr.jobs.Wait()

	printer, err := queries.GetMACLookup(cr.db, "02:00:00:00:00:20")
	if err != nil || printer.HostName != "printer" || printer.Source != "arp" || printer.DeviceID != "s2" {
		t.Errorf("printer = %+v, %v; want the device's ARP row with the lease's host name", printer, err)
	}
	camera, err := queries.GetMACLookup(cr.db, "02:00:00:00:00:21")
	if err != nil || camera.HostName != "camera" || camera.Source != "dhcp" || camera.DeviceName != "kea" {
		t.Errorf("camera = %+v, %v; want the lease-only client", camera, err)
	}
}

// blockingRunner answers like its fakeRunner once gate is closed.
type blockingRunner struct {
	fakeRunner
	gate chan struct{}
}

func (b *blockingRunner) Run(command string, args ...string) (*ros.Reply, error) {
	<-b.gate
	return b.fakeRunner.Run(command, args...)
}
