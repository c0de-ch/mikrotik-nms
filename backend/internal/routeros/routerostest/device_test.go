package routerostest

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

// The exchange log is what a test reads to say what a device was sent: every
// command with the test's clock time, the rows it got back, the trap it was
// refused with — and whether the test had given the device an answer at all.
func TestDeviceLogsExchangesOnTheTestClock(t *testing.T) {
	dev := NewDevice(t)
	// The clock is read on the device's goroutine and moved on the test's.
	first := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	now := first.Add(30 * time.Second)
	var later atomic.Bool
	dev.UseClock(func() time.Time {
		if later.Load() {
			return now
		}
		return first
	})
	dev.SetUnknownTrap("no such command prefix")
	dev.SetReply("/interface/print", map[string]string{"name": "ether1"}, map[string]string{"name": "ether2"})
	dev.SetCount("/interface/wifi/print", 0)
	dev.SetTrap("/caps-man/interface/print", "no such command prefix")
	pool := routeros.NewPool()
	dev.Dial(t, pool, "d1")
	run := routeros.ClientRunner{Client: pool.GetLive("d1")}

	if !dev.LastExchangeAt().IsZero() {
		t.Fatalf("last exchange before any command = %v", dev.LastExchangeAt())
	}
	if _, err := run.Run("/interface/print", "=.proplist=name"); err != nil {
		t.Fatal(err)
	}
	later.Store(true)
	if n, err := routeros.CountOnly(run, "/interface/wifi/print"); err != nil || n != 0 {
		t.Fatalf("count-only = %d, %v", n, err)
	}
	if _, err := run.Run("/caps-man/interface/print", "=count-only="); !routeros.IsNoSuchCommand(err) {
		t.Fatalf("a scripted trap = %v", err)
	}
	if _, err := run.Run("/tool/never/scripted"); !routeros.IsNoSuchCommand(err) {
		t.Fatalf("an unscripted command after SetUnknownTrap = %v, want the trap", err)
	}

	got := dev.Exchanges()
	if len(got) != 4 {
		t.Fatalf("exchanges = %+v, want 4", got)
	}
	if e := got[0]; e.Command != "/interface/print" || e.Rows != 2 || e.Trap != "" || !e.Scripted ||
		!e.At.Equal(first) || len(e.Words) != 1 || e.Words[0] != "=.proplist=name" {
		t.Errorf("the print = %+v", e)
	}
	if e := got[1]; e.Rows != 0 || e.Trap != "" || !e.Scripted || !e.At.Equal(now) {
		t.Errorf("the count-only = %+v", e)
	}
	if e := got[2]; e.Trap == "" || !e.Scripted {
		t.Errorf("the scripted trap = %+v", e)
	}
	if e := got[3]; e.Trap == "" || e.Scripted {
		t.Errorf("the unscripted command = %+v, want a trap and Scripted=false", e)
	}
	if !dev.LastExchangeAt().Equal(now) {
		t.Errorf("last exchange at %v, want %v", dev.LastExchangeAt(), now)
	}
	// The session is as usable after a trap as a device's is.
	if _, err := run.Run("/interface/print"); err != nil {
		t.Fatalf("the session after the traps: %v", err)
	}
}

// A device on its own loopback address, for tests that store several.
func TestDeviceOnAnotherLoopbackAddress(t *testing.T) {
	dev, err := NewDeviceOn(t, "127.0.0.9")
	if err != nil {
		t.Skipf("127.0.0.9 is not local on this platform: %v", err)
	}
	if dev.Host != "127.0.0.9" {
		t.Fatalf("host = %q", dev.Host)
	}
	pool := routeros.NewPool()
	dev.Dial(t, pool, "d1")
	if _, err := routeros.RunCommand(pool.GetLive("d1"), "/system/identity/print"); err != nil {
		t.Fatal(err)
	}
	if got := dev.Commands(); len(got) != 1 {
		t.Fatalf("commands = %v", got)
	}
}

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
