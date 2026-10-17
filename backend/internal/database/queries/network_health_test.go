package queries

import "testing"

// The optional runtime columns round-trip as "unknown" (nil) or a value; a
// device that reports no counter must never read back as 0 changes.
func TestBridgePortStatusOptionalFieldsRoundTrip(t *testing.T) {
	db := testDB(t)
	if err := CreateDevice(db, &Device{ID: "d1", Address: "192.0.2.1", Username: "u", APIPort: 8728, Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertBridgeStatus(db, &BridgeStatus{ID: "d1:bridge", DeviceID: "d1", BridgeName: "bridge", Protocol: "rstp",
		STPEnabled: true, TopologyChanges: 9, PortCount: 2, STPSource: "print"}); err != nil {
		t.Fatal(err)
	}
	tcn, fwd, pvid := 3, true, 28
	known := &BridgePortStatus{ID: "d1:bridge:ether1", DeviceID: "d1", BridgeName: "bridge", PortInterface: "ether1",
		Role: "root", Status: "forwarding", Edge: false, PointToPoint: true, PathCost: 10, DesignatedBridge: "0x8000.AA",
		TopologyChanges: &tcn, Forwarding: &fwd, Learning: &fwd, Inactive: new(bool), Disabled: new(bool), HWOffload: &fwd,
		HWOffloadGroup: "switch1", PVID: &pvid, PortState: "in-bridge", EdgeConfig: "auto"}
	unknown := &BridgePortStatus{ID: "d1:bridge:ether2", DeviceID: "d1", BridgeName: "bridge", PortInterface: "ether2", Edge: true}
	for _, p := range []*BridgePortStatus{known, unknown} {
		if err := UpsertBridgePortStatus(db, p); err != nil {
			t.Fatal(err)
		}
	}

	bridges, err := ListBridgeStatus(db)
	if err != nil || len(bridges) != 1 || bridges[0].STPSource != "print" || bridges[0].TopologyChanges != 9 {
		t.Fatalf("bridges = %+v, err %v", bridges, err)
	}
	ports, err := ListBridgePortStatus(db)
	if err != nil || len(ports) != 2 {
		t.Fatalf("ports = %+v, err %v", ports, err)
	}
	k := ports[0]
	if k.TopologyChanges == nil || *k.TopologyChanges != 3 || k.Forwarding == nil || !*k.Forwarding ||
		k.Inactive == nil || *k.Inactive || k.HWOffload == nil || !*k.HWOffload || k.PVID == nil || *k.PVID != 28 ||
		k.HWOffloadGroup != "switch1" || k.PortState != "in-bridge" || k.EdgeConfig != "auto" || !k.PointToPoint {
		t.Fatalf("known port = %+v", k)
	}
	u := ports[1]
	if u.TopologyChanges != nil || u.Forwarding != nil || u.Learning != nil || u.Inactive != nil || u.Disabled != nil ||
		u.HWOffload != nil || u.PVID != nil || u.HWOffloadGroup != "" || u.PortState != "" || !u.Edge {
		t.Fatalf("unknown port = %+v", u)
	}

	// An update that no longer knows a field clears it to NULL again.
	known.TopologyChanges = nil
	if err := UpsertBridgePortStatus(db, known); err != nil {
		t.Fatal(err)
	}
	ports, _ = ListBridgePortStatus(db)
	if ports[0].TopologyChanges != nil {
		t.Fatalf("topology_changes should be NULL after an update without it, got %d", *ports[0].TopologyChanges)
	}
}
