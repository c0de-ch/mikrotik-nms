package routeros_test

import (
	"errors"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

// pooled dials fake under id and returns the pool and the session.
func pooled(t *testing.T, fake *routerostest.Device, id string) (*routeros.Pool, *ros.Client) {
	t.Helper()
	pool := routeros.NewPool()
	fake.Dial(t, pool, id)
	return pool, pool.Get(id)
}

// wantEvicted asserts the session is gone for good: out of the pool, closed,
// and answering nothing more.
func wantEvicted(t *testing.T, pool *routeros.Pool, id string, client *ros.Client) {
	t.Helper()
	if pool.Get(id) != nil {
		t.Error("the session is still pooled")
	}
	if _, err := routeros.RunCommand(client, "/system/identity/print"); !errors.Is(err, routeros.ErrSessionClosed) {
		t.Errorf("a command on the ended session = %v, want ErrSessionClosed", err)
	}
}

// A command that passes its deadline on the wire ends the session: the
// reply may still arrive, and would be read as the next command's.
func TestCommandDeadlineEndsTheSession(t *testing.T) {
	fake := routerostest.NewDevice(t)
	release := make(chan struct{})
	fake.OnCommand("/interface/print", func() { <-release })
	t.Cleanup(func() { close(release) })
	pool, client := pooled(t, fake, "d1")

	start := time.Now()
	_, err := routeros.RunCommandWithTimeout(client, 50*time.Millisecond, "/interface/print")
	var te *routeros.TimeoutError
	if !errors.As(err, &te) || te.Queued || te.Command != "/interface/print" {
		t.Fatalf("err = %v, want a TimeoutError on the wire", err)
	}
	if !routeros.IsSessionFatal(err) || routeros.IsSessionBusy(err) {
		t.Errorf("a deadline on the wire must be session-fatal and not 'busy': %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the command returned after %s, want it at its 50ms deadline", took)
	}
	wantEvicted(t, pool, "d1", client)
}

// The wait for the session counts against the deadline. A command that
// spends its whole budget queued behind another fails without being sent —
// and without touching the session, which the command holding it still uses.
func TestQueueWaitCountsAgainstTheDeadline(t *testing.T) {
	fake := routerostest.NewDevice(t)
	entered, release := make(chan struct{}), make(chan struct{})
	fake.OnCommand("/log/print", func() { close(entered); <-release })
	fake.SetReply("/log/print", map[string]string{"message": "one line"})
	pool, client := pooled(t, fake, "d1")

	slow := make(chan error, 1)
	go func() {
		reply, err := routeros.RunCommandWithTimeout(client, time.Minute, "/log/print")
		if err == nil && len(reply.Re) != 1 {
			err = errors.New("the slow command lost its reply")
		}
		slow <- err
	}()
	<-entered // the slow command holds the session

	_, err := routeros.RunCommandWithTimeout(client, 50*time.Millisecond, "/interface/print")
	var te *routeros.TimeoutError
	if !errors.As(err, &te) || !te.Queued {
		t.Fatalf("queued command = %v, want a TimeoutError{Queued}", err)
	}
	if !routeros.IsSessionBusy(err) || routeros.IsSessionFatal(err) {
		t.Errorf("a deadline in the queue is 'busy', not session-fatal: %v", err)
	}
	if pool.Get("d1") != client {
		t.Fatal("a command that timed out in the queue cost the session")
	}

	close(release)
	if err := <-slow; err != nil {
		t.Fatalf("the command that held the session: %v", err)
	}
	for _, c := range fake.Commands() {
		if c == "/interface/print" {
			t.Fatal("the queued command was sent although its deadline had passed")
		}
	}
	// The session is as good as before.
	if _, err := routeros.RunCommand(client, "/system/identity/print"); err != nil {
		t.Fatalf("command after the queue timeout: %v", err)
	}
}

// What a device may answer without ending the session — and what ends it.
func TestSessionFatalReplies(t *testing.T) {
	t.Run("a trap leaves the session alone", func(t *testing.T) {
		fake := routerostest.NewDevice(t)
		fake.SetTrap("/interface/wifi/print", "no such command prefix")
		pool, client := pooled(t, fake, "d1")
		_, err := routeros.RunCommand(client, "/interface/wifi/print")
		if !routeros.IsNoSuchCommand(err) || routeros.IsSessionFatal(err) {
			t.Fatalf("err = %v, want the trap, not session-fatal", err)
		}
		if pool.Get("d1") != client {
			t.Fatal("a trap cost the session")
		}
		if _, err := routeros.RunCommand(client, "/system/identity/print"); err != nil {
			t.Fatalf("command after the trap: %v", err)
		}
	})

	t.Run("!fatal", func(t *testing.T) {
		fake := routerostest.NewDevice(t)
		fake.SetFatal("/system/resource/print", "session terminated on request")
		pool, client := pooled(t, fake, "d1")
		_, err := routeros.RunCommand(client, "/system/resource/print")
		var de *ros.DeviceError
		if !errors.As(err, &de) || !routeros.IsSessionFatal(err) {
			t.Fatalf("err = %v, want the device's !fatal, session-fatal", err)
		}
		wantEvicted(t, pool, "d1", client)
	})

	t.Run("a word over 1 MiB", func(t *testing.T) {
		fake := routerostest.NewDevice(t)
		fake.SetRaw("/log/print", routerostest.OversizedWord(64<<20))
		pool, client := pooled(t, fake, "d1")
		_, err := routeros.RunCommand(client, "/log/print")
		var le *proto.LimitError
		if !errors.As(err, &le) || le.What != proto.LimitWord || !routeros.IsSessionFatal(err) {
			t.Fatalf("err = %v, want a word limit error, session-fatal", err)
		}
		wantEvicted(t, pool, "d1", client)
	})

	t.Run("an invalid sentence word", func(t *testing.T) {
		fake := routerostest.NewDevice(t)
		fake.SetRaw("/log/print", routerostest.Sentences([]string{"!re", "not an attribute"}, []string{"!done"}))
		pool, client := pooled(t, fake, "d1")
		_, err := routeros.RunCommand(client, "/log/print")
		var pe *proto.ProtocolError
		if !errors.As(err, &pe) || !routeros.IsSessionFatal(err) {
			t.Fatalf("err = %v, want a protocol error, session-fatal", err)
		}
		wantEvicted(t, pool, "d1", client)
	})

	t.Run("more than 50,000 rows", func(t *testing.T) {
		fake := routerostest.NewDevice(t)
		fake.SetRaw("/ip/arp/print", routerostest.Rows(ros.MaxReplyRows+1))
		fake.SetRaw("/ip/dhcp-server/lease/print", routerostest.Rows(ros.MaxReplyRows))
		pool, client := pooled(t, fake, "d1")

		reply, err := routeros.RunCommand(client, "/ip/dhcp-server/lease/print")
		if err != nil || len(reply.Re) != ros.MaxReplyRows {
			t.Fatalf("a reply of exactly the cap: %d rows, %v", len(reply.Re), err)
		}
		_, err = routeros.RunCommand(client, "/ip/arp/print")
		var le *proto.LimitError
		if !errors.As(err, &le) || le.What != proto.LimitRows || !routeros.IsSessionFatal(err) {
			t.Fatalf("err = %v, want a rows limit error, session-fatal", err)
		}
		wantEvicted(t, pool, "d1", client)
	})

	t.Run("a reply over 32 MiB", func(t *testing.T) {
		fake := routerostest.NewDevice(t)
		big := "=k=" + string(make([]byte, proto.MaxWordLen-3))
		var rows [][]string
		for range proto.MaxReplyLen/proto.MaxWordLen + 1 {
			rows = append(rows, []string{"!re", big})
		}
		fake.SetRaw("/log/print", routerostest.Sentences(append(rows, []string{"!done"})...))
		pool, client := pooled(t, fake, "d1")
		_, err := routeros.RunCommand(client, "/log/print")
		var le *proto.LimitError
		if !errors.As(err, &le) || le.What != proto.LimitReply || !routeros.IsSessionFatal(err) {
			t.Fatalf("err = %v, want a reply limit error, session-fatal", err)
		}
		wantEvicted(t, pool, "d1", client)
	})
}

// A one-shot session is cleaned up by closing it, and refuses commands
// afterwards instead of writing to a dead socket.
func TestOneShotSessionAfterClose(t *testing.T) {
	fake := routerostest.NewDevice(t)
	once, err := routeros.DialOnce(fake.Target(""), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routeros.RunCommand(once, "/system/identity/print"); err != nil {
		t.Fatal(err)
	}
	_ = once.Close()
	_ = once.Close() // closing twice is harmless
	if _, err := routeros.RunCommand(once, "/system/identity/print"); !errors.Is(err, routeros.ErrSessionClosed) {
		t.Fatalf("command after Close = %v, want ErrSessionClosed", err)
	}
	if got := fake.Commands(); len(got) != 1 {
		t.Fatalf("commands = %v, want only the one before Close", got)
	}
}

// A one-shot session dialled for a stored device counts against it without
// the caller tagging it, and stops doing so when it is closed.
func TestOneShotSessionIsTaggedWithItsDevice(t *testing.T) {
	rec := &recorder{}
	routeros.SetObserver(rec)
	t.Cleanup(func() { routeros.SetObserver(nil) })

	fake := routerostest.NewDevice(t)
	once, err := routeros.DialOnce(fake.Target("dev-9"), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer once.Close()
	if _, err := routeros.RunCommandWithTimeout(once, time.Second, "/tool/fetch"); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.commands) != 1 || rec.commands[0].DeviceID != "dev-9" {
		t.Fatalf("commands = %+v, want one counted against dev-9", rec.commands)
	}
	if len(rec.sessions) != 0 {
		t.Errorf("sessions = %+v, want none: a one-shot dial is not a pooled session", rec.sessions)
	}
}

// A pooled session dialled under another key for a stored device (the
// address-move verification) is observed as that device.
func TestPooledSessionUnderAnotherKeyCountsAgainstItsDevice(t *testing.T) {
	rec := &recorder{}
	routeros.SetObserver(rec)
	t.Cleanup(func() { routeros.SetObserver(nil) })

	fake := routerostest.NewDevice(t)
	pool := routeros.NewPool()
	client, err := pool.Dial("dev-9:verify", fake.Target("dev-9"), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Forget("dev-9:verify")
	if _, err := routeros.RunCommand(client, "/system/resource/print"); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sessions) != 1 || rec.sessions[0].DeviceID != "dev-9" {
		t.Errorf("sessions = %+v, want one for dev-9", rec.sessions)
	}
	if len(rec.commands) != 1 || rec.commands[0].DeviceID != "dev-9" {
		t.Errorf("commands = %+v, want one counted against dev-9", rec.commands)
	}
}
