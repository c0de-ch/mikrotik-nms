package topology

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

func roleDevice(id, addr, board, class string) queries.Device {
	return queries.Device{ID: id, Address: addr, Identity: id, Board: board, DeviceClass: class, PollProfile: "auto"}
}

func TestDeviceRolesFromUplinks(t *testing.T) {
	devices := []queries.Device{
		roleDevice("edge", "192.0.2.1", "RB5009UG+S+", "C3"),
		roleDevice("core", "192.0.2.2", "CCR2004-16G-2S+", "C3"),
		roleDevice("inner", "192.0.2.3", "RB5009UG+S+", "C3"),
		roleDevice("sw", "192.0.2.4", "CRS326-24G-2S+", "C2"),
		roleDevice("ap", "192.0.2.5", "cAP ax", "C3"),
		roleDevice("lte", "192.0.2.6", "hEX S", "C1"),
		roleDevice("vpn", "192.0.2.7", "RB4011iGS+", "C2"),
		roleDevice("attach", "192.0.2.8", "RB5009UG+S+", "C3"),
		roleDevice("new", "192.0.2.9", "", ""),
	}
	uplinks := []queries.DeviceUplink{
		// A public (CGNAT) next hop: the internet edge.
		{DeviceID: "edge", Kind: "default-route", Interface: "ether1", IfaceType: "ether", GatewayIP: "100.64.0.1"},
		{DeviceID: "edge", Kind: "vpn", Interface: "wg0", IfaceType: "wg"},
		// A default route into a tunnel.
		{DeviceID: "core", Kind: "default-route", Interface: "wg-site", IfaceType: "wg"},
		// A private, unmanaged gateway and a managed next hop: not an egress of its own.
		{DeviceID: "inner", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.168.1.1"},
		{DeviceID: "sw", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.0.2.1"},
		{DeviceID: "ap", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.0.2.1"},
		// An interface-only route out an LTE modem.
		{DeviceID: "lte", Kind: "default-route", Interface: "lte1", IfaceType: "lte"},
		// A running tunnel interface.
		{DeviceID: "vpn", Kind: "vpn", Interface: "ovpn-out1", IfaceType: "ovpn-out"},
		// Where a gateway is plugged in says nothing about the device's own routes.
		{DeviceID: "attach", Kind: "gateway-host", Interface: "ether3", IfaceType: "ether", GatewayIP: "192.168.1.1"},
	}
	roles := DeviceRoles(devices, uplinks, map[string]int{"ap": 2}, map[string]int{"sw": 24, "inner": 8})
	for id, want := range map[string]struct {
		role   devclass.Role
		egress devclass.Egress
		ifaces []string
	}{
		"edge":   {devclass.RoleRouter, devclass.EgressInternet, []string{"ether1", "wg0"}},
		"core":   {devclass.RoleRouter, devclass.EgressVPN, []string{"wg-site"}},
		"inner":  {devclass.RoleUnknown, devclass.EgressNone, nil},
		"sw":     {devclass.RoleSwitch, devclass.EgressNone, nil},
		"ap":     {devclass.RoleAPCPE, devclass.EgressNone, nil},
		"lte":    {devclass.RoleWeakRouter, devclass.EgressInternet, []string{"lte1"}},
		"vpn":    {devclass.RoleWeakRouter, devclass.EgressVPN, []string{"ovpn-out1"}},
		"attach": {devclass.RoleUnknown, devclass.EgressNone, nil},
		"new":    {devclass.RoleUnknown, devclass.EgressNone, nil},
	} {
		got := roles[id]
		if got.Role != want.role || got.Egress != want.egress || !slices.Equal(got.EgressIfaces, want.ifaces) || got.Reason == "" {
			t.Errorf("%s: role %s egress %q ifaces %v (%s), want %s %q %v", id, got.Role, got.Egress, got.EgressIfaces,
				got.Reason, want.role, want.egress, want.ifaces)
		}
	}
	if roles["edge"].Probed != devclass.C3 || roles["edge"].Budget != devclass.C3 || roles["new"].Probed != devclass.C0Unknown ||
		roles["new"].Budget != devclass.C1 {
		t.Errorf("classes: edge %+v, new %+v", roles["edge"], roles["new"])
	}
}

// The role is what the hardware is (probed class); the budget follows the
// admin's gentler override and the light profile.
func TestDeviceRolesBudgetFollowsOverrideAndProfile(t *testing.T) {
	up := func(id string) queries.DeviceUplink {
		return queries.DeviceUplink{DeviceID: id, Kind: "default-route", Interface: "ether1", IfaceType: "ether", GatewayIP: "203.0.113.1"}
	}
	over := roleDevice("over", "192.0.2.1", "RB5009UG+S+", "C3")
	over.ClassOverride = "C1"
	light := roleDevice("light", "192.0.2.2", "RB5009UG+S+", "C3")
	light.PollProfile = "light"
	roles := DeviceRoles([]queries.Device{over, light}, []queries.DeviceUplink{up("over"), up("light")}, nil, nil)
	if r := roles["over"]; r.Role != devclass.RoleRouter || r.Probed != devclass.C3 || r.Budget != devclass.C1 || !r.Overridden || r.Light {
		t.Errorf("override: %+v", r)
	}
	if r := roles["light"]; r.Role != devclass.RoleRouter || r.Budget != devclass.C1 || !r.Light || r.Overridden {
		t.Errorf("light: %+v", r)
	}
}

// LoadDeviceRoles reads everything from the database and drops stale routes:
// a router whose default route was last seen long ago is not a router now.
func TestLoadDeviceRoles(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "roles.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, d := range []queries.Device{
		{ID: "edge", Address: "192.0.2.1", Identity: "edge", Board: "RB5009UG+S+", Username: "u", APIPort: 8728, Status: "online"},
		{ID: "stale", Address: "192.0.2.2", Identity: "stale", Board: "RB5009UG+S+", Username: "u", APIPort: 8728, Status: "online"},
		{ID: "ap", Address: "192.0.2.3", Identity: "ap", Board: "SomeOtherBoard", Username: "u", APIPort: 8728, Status: "online"},
	} {
		if err := queries.CreateDevice(db, &d); err != nil {
			t.Fatal(err)
		}
		if err := queries.SetDeviceClass(db, d.ID, devclass.C3, devclass.ResourceFacts{}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"edge", "stale"} {
		if err := queries.ReplaceDeviceUplinks(db, id, []queries.DeviceUplink{
			{DeviceID: id, Kind: "default-route", Interface: "ether1", IfaceType: "ether", GatewayIP: "203.0.113.1"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := queries.ReplaceDeviceUplinks(db, "ap", []queries.DeviceUplink{
		{DeviceID: "ap", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.0.2.1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE device_uplinks SET last_seen = '2000-01-01 00:00:00' WHERE device_id = 'stale'`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wifi1", "wifi2"} {
		if err := queries.UpsertInterface(db, &queries.Interface{ID: uuid.NewString(), DeviceID: "ap", Name: name, Type: "wifi"}); err != nil {
			t.Fatal(err)
		}
	}
	roles, devices, err := LoadDeviceRoles(db, nil, time.Now())
	if err != nil || len(devices) != 3 {
		t.Fatalf("load: %d devices, %v", len(devices), err)
	}
	if roles["edge"].Role != devclass.RoleRouter {
		t.Errorf("edge = %+v", roles["edge"])
	}
	if roles["stale"].Role != devclass.RoleUnknown || roles["stale"].Egress != devclass.EgressNone {
		t.Errorf("a stale default route still made a router: %+v", roles["stale"])
	}
	if roles["ap"].Role != devclass.RoleAPCPE {
		t.Errorf("unknown board with wifi interfaces behind a router = %+v", roles["ap"])
	}
}
