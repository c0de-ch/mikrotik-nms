package routerostest

import (
	"testing"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

// The tripwire is only worth something if it fires: a command sent over the
// pooled client must show up, and the login must not.
func TestDeviceRecordsCommandsAfterLogin(t *testing.T) {
	dev := NewDevice(t)
	pool := routeros.NewPool(false)
	dev.Dial(t, pool, "d1")

	client := pool.GetLive("d1")
	if client == nil {
		t.Fatal("pool has no live client after Dial")
	}
	if got := dev.Commands(); len(got) != 0 {
		t.Fatalf("commands after login = %v, want none", got)
	}
	if _, err := routeros.RunCommand(client, "/interface/monitor-traffic", "=interface=ether1", "=once="); err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if got := dev.Commands(); len(got) != 1 || got[0] != "/interface/monitor-traffic" {
		t.Fatalf("commands = %v, want the monitor-traffic call", got)
	}
	if n := dev.Conns(); n != 1 {
		t.Errorf("connections = %d, want 1", n)
	}
}
