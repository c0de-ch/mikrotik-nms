package poller

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

func TestStatusAfterFailedPoll(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	threshold := 2 * time.Minute

	recent := now.Add(-30 * time.Second)
	stale := now.Add(-5 * time.Minute)
	atBoundary := now.Add(-2 * time.Minute)

	tests := []struct {
		name     string
		lastSeen *time.Time
		want     string
	}{
		{"never seen", nil, "offline"},
		{"seen recently reports unknown within grace", &recent, "unknown"},
		{"unreachable past threshold goes offline", &stale, "offline"},
		{"exactly at threshold goes offline", &atBoundary, "offline"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusAfterFailedPoll(tt.lastSeen, threshold, now); got != tt.want {
				t.Errorf("statusAfterFailedPoll() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInterSwitchPorts(t *testing.T) {
	links := []queries.Link{
		{DeviceAID: "sw1", InterfaceA: "ether1,bridge", DeviceBID: "sw2", InterfaceB: "sfp-sfpplus1"},
		{DeviceAID: "sw2", InterfaceA: " ether5 , ", DeviceBID: "ap1", InterfaceB: ""},
	}
	got := interSwitchPorts(links)

	for _, key := range []string{"sw1:ether1", "sw1:bridge", "sw2:sfp-sfpplus1", "sw2:ether5"} {
		if !got[key] {
			t.Errorf("interSwitchPorts() missing %q", key)
		}
	}
	for _, key := range []string{"sw1:ether1,bridge", "ap1:", "sw2:"} {
		if got[key] {
			t.Errorf("interSwitchPorts() unexpectedly contains %q", key)
		}
	}
	if len(got) != 4 {
		t.Errorf("interSwitchPorts() has %d keys, want 4: %v", len(got), got)
	}
}

// The uplink pass asks the device for its default routes only: interface
// types and the running VPN tunnels come from the interface snapshot, or
// from the interfaces table when there is none.
func TestCollectUplinksReadsInterfaceSnapshot(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "r1", Identity: "router", Address: "192.0.2.1"})
	m := &Manager{db: db, portStats: NewPortStatsCollector(db, nil, nil)}
	run := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/route/print": rep(map[string]string{"active": "true", "gateway": "198.51.100.1", "immediate-gw": "198.51.100.1%ether1"}),
	}}
	byKind := func() map[string][]queries.DeviceUplink {
		t.Helper()
		ups, err := queries.ListDeviceUplinks(db, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]queries.DeviceUplink{}
		for _, u := range ups {
			out[u.Kind] = append(out[u.Kind], u)
		}
		return out
	}

	// No snapshot yet: the interfaces table stands in.
	if err := queries.UpsertInterface(db, &queries.Interface{ID: "r1:ether1", DeviceID: "r1", Name: "ether1", Type: "ether", Running: true}); err != nil {
		t.Fatal(err)
	}
	m.collectUplinks(queries.Device{ID: "r1", Identity: "router"}, run)
	if got := byKind(); len(got["default-route"]) != 1 || got["default-route"][0].IfaceType != "ether" || len(got["vpn"]) != 0 {
		t.Fatalf("uplinks from the table = %+v", got)
	}

	m.portStats.ingest("r1", []routeros.InterfaceStats{
		{Name: "ether1", Type: "ether", Running: true},
		{Name: "wg0", Type: "wg", Running: true},
		{Name: "wg1", Type: "wg", Running: true, Disabled: true},
	}, time.Now().UTC(), 15*time.Second)
	m.collectUplinks(queries.Device{ID: "r1", Identity: "router"}, run)
	got := byKind()
	if len(got["vpn"]) != 1 || got["vpn"][0].Interface != "wg0" || got["vpn"][0].IfaceType != "wg" {
		t.Fatalf("vpn uplinks from the snapshot = %+v, want wg0 only", got["vpn"])
	}
	for _, c := range run.calls {
		if c != "/ip/route/print ?dst-address=0.0.0.0/0" {
			t.Errorf("the uplink pass issued %q; it must only print the default routes", c)
		}
	}
	if len(run.calls) != 2 {
		t.Errorf("route prints = %d, want one per pass", len(run.calls))
	}
}

// Gateway attachment reads the FDB dumps in port_hosts instead of asking
// every device about every gateway MAC: the edge-port observation of an
// online device's latest dump wins; transit ports, offline devices and MACs
// a device no longer has are not observations.
func TestGatewayAttachmentFromPortHosts(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "r1", Identity: "router", Address: "192.0.2.1"})
	mustCreateDevice(t, db, &queries.Device{ID: "sw1", Identity: "sw1", Address: "192.0.2.2"})
	mustCreateDevice(t, db, &queries.Device{ID: "sw2", Identity: "sw2", Address: "192.0.2.3", Status: "offline"})
	const gwIP, gwMAC = "10.0.0.1", "02:00:00:00:AA:01"

	if err := queries.UpsertLink(db, &queries.Link{ID: "l1", DeviceAID: "r1", InterfaceA: "ether1", DeviceBID: "sw1", InterfaceB: "sfp1", Status: "up"}); err != nil {
		t.Fatal(err)
	}
	if err := queries.ReplaceDeviceUplinks(db, "r1", []queries.DeviceUplink{{DeviceID: "r1", Kind: "default-route", Interface: "ether1", GatewayIP: gwIP}}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertMACLookup(db, &queries.MACLookup{MACAddress: gwMAC, IPAddress: gwIP, Source: "arp"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	dump := func(dev string, at time.Time, hosts ...queries.PortHost) {
		t.Helper()
		if err := queries.UpsertPortHosts(db, dev, hosts, at); err != nil {
			t.Fatal(err)
		}
	}
	dump("r1", now.Add(-time.Minute), queries.PortHost{InterfaceName: "ether1", MACAddress: gwMAC}) // learned via the link: transit
	dump("sw1", now.Add(-10*time.Minute), queries.PortHost{InterfaceName: "ether7", MACAddress: gwMAC})
	dump("sw1", now.Add(-2*time.Minute), queries.PortHost{InterfaceName: "ether5", MACAddress: gwMAC}) // moved: latest dump wins
	dump("sw2", now.Add(-time.Minute), queries.PortHost{InterfaceName: "ether3", MACAddress: gwMAC})   // offline device

	m := &Manager{db: db}
	devices, err := queries.ListDevices(db)
	if err != nil {
		t.Fatal(err)
	}
	gw := m.gatewayMACs(devices)
	if gw[gwMAC] != gwIP {
		t.Fatalf("gateway MACs = %v", gw)
	}
	obs := m.gatewayObservations(devices, gw, now)
	if len(obs) != 2 || obs[0] != (queries.DeviceUplink{DeviceID: "r1", Interface: "ether1", GatewayIP: gwIP}) ||
		obs[1] != (queries.DeviceUplink{DeviceID: "sw1", Interface: "ether5", GatewayIP: gwIP}) {
		t.Fatalf("observations = %+v, want r1/ether1 and sw1/ether5 only", obs)
	}

	m.resolveGatewayHosts(obs)
	ups, err := queries.ListDeviceUplinks(db, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var hosts []queries.DeviceUplink
	for _, u := range ups {
		if u.Kind == "gateway-host" {
			hosts = append(hosts, u)
		}
	}
	if len(hosts) != 1 || hosts[0].DeviceID != "sw1" || hosts[0].Interface != "ether5" || hosts[0].GatewayIP != gwIP {
		t.Fatalf("gateway-host rows = %+v, want sw1/ether5", hosts)
	}

	// No gateway MACs: nothing to look up.
	if obs := m.gatewayObservations(devices, nil, now); obs != nil {
		t.Errorf("observations without gateway MACs = %+v", obs)
	}
}
