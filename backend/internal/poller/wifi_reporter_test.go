package poller

import (
	"context"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// A wireless client the tracker reads is reported by the device whose
// registration table lists it — what the on-demand refresh records too, and
// what the Clients page shows as "Reported By" / "CAPsMAN Controller". The
// device a client watch pings from (device_id) is not moved by it.
func TestWifiTrackerRecordsTheReportingDevice(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"ctl": devclass.C3})
	const x = "02:00:00:00:D0:02"
	ctl := wifiController()
	ctl.byCmd["/interface/wifi/registration-table/print"] = rep(map[string]string{
		"mac-address": x, "interface": "wifi1", "ssid": "lab", "signal": "-60", "last-ip": "192.0.2.60"})
	// The client cache already knows the client: an ARP read named the
	// router, the on-demand refresh chose its probing device.
	if err := queries.UpsertMACLookup(db, &queries.MACLookup{MACAddress: x, IPAddress: "192.0.2.60", Source: "arp", DeviceName: "router", DeviceID: "probe"}); err != nil {
		t.Fatal(err)
	}
	wt := NewWifiTracker(db, nil, ws.NewHub(), 30*time.Second)
	clock := &testClock{t: at(12, 0, 0)}
	wt.now = clock.now
	wt.clock = newTierClock(NewScheduleRegistry())
	wt.clock.phase = noJitter
	wt.UseCadence(NewCadence(db, nil))
	wt.runner = scriptedRunners(map[string]*fakeRunner{"ctl": ctl})

	wt.poll(context.Background()) // first read: the client joins
	clock.set(at(12, 0, 31))
	wt.poll(context.Background()) // second read: the known client is refreshed in the cache

	m, err := queries.GetMACLookup(db, x)
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "wifi" || m.DeviceName != "ctl" || m.SSID != "lab" {
		t.Fatalf("client cache after the wifi read: source %q, reported by %q, ssid %q; want wifi, ctl, lab",
			m.Source, m.DeviceName, m.SSID)
	}
	// The device a client watch pings from is left as it was.
	if m.DeviceID != "probe" {
		t.Fatalf("device_id = %q after the wifi read, want it left at %q", m.DeviceID, "probe")
	}
}
