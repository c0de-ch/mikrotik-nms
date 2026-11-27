package routerostest

import (
	"testing"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

// The tripwire is only worth something if it fires: a command sent over the
// pooled client must show up, and the login must not.
func TestDeviceRecordsCommandsAfterLogin(t *testing.T) {
	dev := NewDevice(t)
	pool := routeros.NewPool()
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

// A query word (?key=value) is part of a request like any attribute word,
// and =count-only= is answered with the count in the !done sentence — as a
// RouterOS device does both.
func TestDeviceTakesQueryWordsAndAnswersCountOnly(t *testing.T) {
	dev := NewDevice(t)
	dev.SetReply("/ip/firewall/filter/print", map[string]string{"chain": "forward"}, map[string]string{"chain": "forward"})
	dev.SetCount("/ip/firewall/filter/print", 2)
	pool := routeros.NewPool()
	dev.Dial(t, pool, "d1")
	run := routeros.ClientRunner{Client: pool.GetLive("d1")}

	got := routeros.ReadConfig(run, routeros.ConfigRead{Command: "/ip/firewall/filter/print", Query: []string{"?chain=forward"}, CountOnly: true})
	if got.State != routeros.ConfigOK || got.Count != 2 || len(got.Rows) != 0 {
		t.Fatalf("count-only = %+v (%v)", got, got.Err)
	}
	// Without =count-only= the same command returns its rows.
	rows := routeros.ReadConfig(run, routeros.ConfigRead{Command: "/ip/firewall/filter/print", Fields: []string{"chain"}, Query: []string{"?chain=forward"}})
	if rows.State != routeros.ConfigOK || len(rows.Rows) != 2 {
		t.Fatalf("rows = %+v (%v)", rows, rows.Err)
	}
	args := dev.Args()
	if len(args) != 2 || len(args[0]) != 2 || args[0][0] != "=count-only=" || args[0][1] != "?chain=forward" ||
		args[1][0] != "=.proplist=chain" || args[1][1] != "?chain=forward" {
		t.Fatalf("recorded words = %q", args)
	}
	// The session survived both.
	if _, err := routeros.RunCommand(pool.GetLive("d1"), "/system/identity/print"); err != nil {
		t.Fatalf("the session after a query: %v", err)
	}
}
