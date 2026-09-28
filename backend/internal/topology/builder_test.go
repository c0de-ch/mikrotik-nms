package topology

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func TestInferDeviceType(t *testing.T) {
	tests := []struct {
		board string
		want  string
	}{
		// Routers
		{"CCR2004-1G-12S+2XS", "router"},
		{"CCR1036-12G-4S", "router"},
		{"RB4011iGS+5HacQ2HnD", "router"},
		{"RB5009UG+S+IN", "router"},
		{"hEX S", "router"},
		{"RB750Gr3", "router"},

		// Switches
		{"CSS610-8G-2S+IN", "switch"},
		{"CRS326-24G-2S+RM", "switch"},
		{"CRS328-24P-4S+RM", "switch"},

		// Access points
		{"cAP ac", "ap"},
		{"wAP ac", "ap"},
		{"hAP ac3", "ap"},
		{"Audience", "ap"},

		// Empty board -> unknown
		{"", "unknown"},

		// Unrecognized board -> default router
		{"SomeOtherBoard", "router"},
	}

	for _, tt := range tests {
		t.Run(tt.board, func(t *testing.T) {
			got := inferDeviceType(tt.board)
			if got != tt.want {
				t.Errorf("inferDeviceType(%q) = %q, want %q", tt.board, got, tt.want)
			}
		})
	}
}

func TestIsWirelessType(t *testing.T) {
	tests := []struct {
		ifaceType string
		want      bool
	}{
		{"wlan", true},
		{"WLAN", true},
		{"wireless", true},
		{"Wireless", true},
		{"wifi-channel", true},
		{"60g-something", true},
		{"ether", false},
		{"bridge", false},
		{"vlan", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.ifaceType, func(t *testing.T) {
			got := isWirelessType(tt.ifaceType)
			if got != tt.want {
				t.Errorf("isWirelessType(%q) = %v, want %v", tt.ifaceType, got, tt.want)
			}
		})
	}
}

// buildFixtureDB seeds devices + egress rows that exercise every branch of
// appendUplinks: public next-hop, interface-only route (plain + tunnel),
// managed next-hop (skipped), private gateway with an FDB attachment and a
// client-cache label, a second private gateway (site link), a VPN row that
// duplicates the default route (skipped) and a standalone tunnel.
func buildFixtureDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/topology.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, d := range []queries.Device{
		{ID: "dev-r", Address: "10.0.0.1", Identity: "r-edge", Board: "RB5009UG+S+IN", Status: "online"},
		{ID: "dev-s", Address: "10.0.0.2", Identity: "s-core", Board: "CRS354-48G-4S+2Q+", Status: "online"},
		{ID: "dev-v", Address: "10.0.0.3", Identity: "v-remote", Board: "hAP ax3", Status: "offline"},
		{ID: "dev-x", Address: "10.0.0.4", Identity: "x-branch", Board: "", Status: "unknown"},
	} {
		d := d
		if err := queries.CreateDevice(db, &d); err != nil {
			t.Fatalf("create device %s: %v", d.ID, err)
		}
	}
	ups := map[string][]queries.DeviceUplink{
		"dev-r": {
			{DeviceID: "dev-r", Kind: "default-route", Interface: "ether1", IfaceType: "ether", GatewayIP: "203.0.113.1"},
			{DeviceID: "dev-r", Kind: "default-route", Interface: "lte1", IfaceType: "lte"},
		},
		"dev-s": {
			{DeviceID: "dev-s", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "10.0.0.1"},
			{DeviceID: "dev-s", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.168.1.1"},
		},
		"dev-v": {
			{DeviceID: "dev-v", Kind: "default-route", Interface: "wg0", IfaceType: "wg"},
			{DeviceID: "dev-v", Kind: "vpn", Interface: "wg0", IfaceType: "wg"},
			{DeviceID: "dev-v", Kind: "vpn", Interface: "eoip1", IfaceType: "eoip"},
		},
		"dev-x": {
			{DeviceID: "dev-x", Kind: "default-route", Interface: "ether1", IfaceType: "ether", GatewayIP: "192.168.2.1"},
			{DeviceID: "dev-x", Kind: "default-route", Interface: "", IfaceType: ""},
		},
	}
	for id, rows := range ups {
		if err := queries.ReplaceDeviceUplinks(db, id, rows); err != nil {
			t.Fatalf("uplinks %s: %v", id, err)
		}
	}
	if err := queries.ReplaceGatewayHosts(db, []queries.DeviceUplink{
		{DeviceID: "dev-s", Interface: "ether5", IfaceType: "ether", GatewayIP: "192.168.1.1"},
	}); err != nil {
		t.Fatalf("gateway hosts: %v", err)
	}
	if err := queries.UpsertMACLookup(db, &queries.MACLookup{
		MACAddress: "AA:BB:CC:00:00:01", IPAddress: "192.168.1.1", HostName: "fritz.box", Source: "dhcp",
	}); err != nil {
		t.Fatalf("mac lookup: %v", err)
	}
	return db
}

// TestBuildUplinksGolden pins Build()'s output for the egress synthesis so
// refactors of the shared classification cannot change what the map draws.
func TestBuildUplinksGolden(t *testing.T) {
	db := buildFixtureDB(t)
	g, err := NewBuilder(db).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	got, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != buildGolden {
		t.Errorf("Build() output changed:\n got  %s\n want %s", got, buildGolden)
	}
}

// buildGolden was captured from Build() before egress classification was
// shared with path.go; the map's output must stay byte-identical.
const buildGolden = `{"nodes":[{"data":{"id":"dev-r","label":"r-edge","type":"router","status":"online","model":"RB5009UG+S+IN","ros_version":"","cpu_load":null,"address":"10.0.0.1","managed":true}},{"data":{"id":"dev-s","label":"s-core","type":"switch","status":"online","model":"CRS354-48G-4S+2Q+","ros_version":"","cpu_load":null,"address":"10.0.0.2","managed":true}},{"data":{"id":"dev-v","label":"v-remote","type":"ap","status":"offline","model":"hAP ax3","ros_version":"","cpu_load":null,"address":"10.0.0.3","managed":true}},{"data":{"id":"dev-x","label":"x-branch","type":"unknown","status":"unknown","model":"","ros_version":"","cpu_load":null,"address":"10.0.0.4","managed":true}},{"data":{"id":"internet","label":"Internet","type":"internet","status":"up","model":"","ros_version":"","cpu_load":null,"address":"","managed":false}},{"data":{"id":"gw:192.168.1.1","label":"fritz","type":"gateway","status":"up","model":"","ros_version":"","cpu_load":null,"address":"192.168.1.1","managed":false,"attach_device_id":"dev-s","attach_port":"ether5"}},{"data":{"id":"vpn:dev-v:wg0","label":"wg0","type":"vpn","status":"up","model":"","ros_version":"","cpu_load":null,"address":"","managed":false}},{"data":{"id":"vpn:dev-v:eoip1","label":"eoip1","type":"vpn","status":"up","model":"","ros_version":"","cpu_load":null,"address":"","managed":false}},{"data":{"id":"gw:192.168.2.1","label":"192.168.2.1","type":"gateway","status":"up","model":"","ros_version":"","cpu_load":null,"address":"192.168.2.1","managed":false}}],"edges":[{"data":{"id":"up:dev-r:ether1:203.0.113.1","source":"dev-r","target":"internet","source_interface":"ether1","target_interface":"","link_type":"internet","status":"up"}},{"data":{"id":"up:dev-r:lte1:","source":"dev-r","target":"internet","source_interface":"lte1","target_interface":"","link_type":"internet","status":"up"}},{"data":{"id":"gwhost:192.168.1.1","source":"dev-s","target":"gw:192.168.1.1","source_interface":"ether5","target_interface":"","link_type":"gateway","status":"up"}},{"data":{"id":"gwnet:192.168.1.1","source":"gw:192.168.1.1","target":"internet","source_interface":"","target_interface":"","link_type":"internet","status":"up"}},{"data":{"id":"up:dev-s:bridge:192.168.1.1","source":"dev-s","target":"gw:192.168.1.1","source_interface":"bridge","target_interface":"","link_type":"gateway","status":"up"}},{"data":{"id":"up:dev-v:wg0:","source":"dev-v","target":"vpn:dev-v:wg0","source_interface":"wg0","target_interface":"","link_type":"vpn","status":"up"}},{"data":{"id":"up:dev-v:eoip1:","source":"dev-v","target":"vpn:dev-v:eoip1","source_interface":"eoip1","target_interface":"","link_type":"vpn","status":"up"}},{"data":{"id":"gwnet:192.168.2.1","source":"gw:192.168.2.1","target":"internet","source_interface":"","target_interface":"","link_type":"internet","status":"up"}},{"data":{"id":"up:dev-x:ether1:192.168.2.1","source":"dev-x","target":"gw:192.168.2.1","source_interface":"ether1","target_interface":"","link_type":"gateway","status":"up"}},{"data":{"id":"site:192.168.1.1:192.168.2.1","source":"gw:192.168.1.1","target":"gw:192.168.2.1","source_interface":"site link","target_interface":"","link_type":"vpn","status":"up"}}]}`
