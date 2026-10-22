package api

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// The traffic role graph judges FDB and egress rows per device, as the
// pollers that wrote them do: a weak switch dumping its FDB every 15 min (and
// reading its routes every 5) keeps its 20-minute-old FDB rows and its
// 12-minute-old egress, which the fleet-wide windows dropped between reads; a
// router's rows that old are stale; a paused device's rows never age.
func TestTrafficTopologyFreshnessPerDevice(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i, d := range []struct {
		id    string
		class devclass.Class
	}{{"weak", devclass.C1}, {"fast", devclass.C3}, {"held", devclass.C3}} {
		if err := queries.CreateDevice(db, &queries.Device{ID: d.id, Identity: d.id, Address: "192.0.2." + string(rune('1'+i)),
			Username: "nms", APIPort: 8728, Status: "online"}); err != nil {
			t.Fatal(err)
		}
		if err := queries.SetDeviceClass(db, d.id, d.class, devclass.ResourceFacts{}, now); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"ether1", "ether2"} {
			if err := queries.UpsertInterface(db, &queries.Interface{ID: d.id + ":" + name, DeviceID: d.id, Name: name, Type: "ether", Running: true}); err != nil {
				t.Fatal(err)
			}
		}
		if err := queries.ReplaceDeviceUplinks(db, d.id, []queries.DeviceUplink{{DeviceID: d.id, Kind: "default-route",
			Interface: "ether1", IfaceType: "ether", GatewayIP: "203.0.113.1"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := queries.PauseDevicePolling(db, "held", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for id, age := range map[string]time.Duration{"weak": 12 * time.Minute, "fast": 12 * time.Minute, "held": 3 * time.Hour} {
		if _, err := db.Exec(`UPDATE device_uplinks SET last_seen = ? WHERE device_id = ?`,
			now.Add(-age).Format("2006-01-02 15:04:05"), id); err != nil {
			t.Fatal(err)
		}
	}
	for id, d := range map[string]struct {
		mac string
		age time.Duration
	}{"weak": {"02:00:00:00:00:01", 20 * time.Minute}, "fast": {"02:00:00:00:00:02", 20 * time.Minute}, "held": {"02:00:00:00:00:03", 2 * time.Hour}} {
		if err := queries.UpsertPortHosts(db, id, []queries.PortHost{{InterfaceName: "ether2", MACAddress: d.mac}}, now.Add(-d.age)); err != nil {
			t.Fatal(err)
		}
	}

	topo, err := loadTrafficTopo(db, poller.NewCadence(db, nil), poller.PortSnapshot{Ports: []poller.PortRate{}}, now)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"weak": true, "fast": false, "held": true} {
		if got := len(topo.hosts[ifaceKey{id, "ether2"}]) == 1; got != want {
			t.Errorf("%s: FDB row behind ether2 current = %v, want %v", id, got, want)
		}
		if got := topo.graph.Role(id, "ether1").Role == topology.RoleWAN; got != want {
			t.Errorf("%s: ether1 is the WAN = %v (role %q), want %v", id, got, topo.graph.Role(id, "ether1").Role, want)
		}
	}
}
