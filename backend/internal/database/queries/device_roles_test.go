package queries

import (
	"testing"

	"github.com/google/uuid"
)

func TestDeviceRoleCounts(t *testing.T) {
	db := testDB(t)
	for id, addr := range map[string]string{"ap": "192.0.2.20", "sw": "192.0.2.21"} {
		if err := CreateDevice(db, &Device{ID: id, Address: addr, Identity: id, Username: "u", APIPort: 8728, Status: "online"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []Interface{
		{DeviceID: "ap", Name: "wifi1", Type: "wifi"}, {DeviceID: "ap", Name: "wifi2", Type: "wifi"},
		{DeviceID: "ap", Name: "ether1", Type: "ether"}, {DeviceID: "ap", Name: "wlan-legacy", Type: "WLAN"},
		{DeviceID: "sw", Name: "ether1", Type: "ether"}, {DeviceID: "sw", Name: "cap1", Type: "cap"},
	} {
		i.ID = uuid.NewString()
		if err := UpsertInterface(db, &i); err != nil {
			t.Fatal(err)
		}
	}
	for _, port := range []string{"ether1", "ether2", "ether3"} {
		if err := UpsertBridgePortStatus(db, &BridgePortStatus{ID: uuid.NewString(), DeviceID: "sw", BridgeName: "bridge",
			PortInterface: port}); err != nil {
			t.Fatal(err)
		}
	}
	radios, ports, err := DeviceRoleCounts(db)
	if err != nil {
		t.Fatal(err)
	}
	if radios["ap"] != 3 || radios["sw"] != 0 {
		t.Errorf("radios = %v, want ap:3 (wifi, wifi, WLAN) and none for the switch (a cap interface is the controller's)", radios)
	}
	if ports["sw"] != 3 || ports["ap"] != 0 {
		t.Errorf("bridge ports = %v", ports)
	}
}
