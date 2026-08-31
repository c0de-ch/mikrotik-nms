package routeros

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestJoinHostPort(t *testing.T) {
	cases := map[string]string{
		"192.168.1.1":   "192.168.1.1:8728",   // IPv4 — identical to "%s:%d"
		"10.0.0.5":      "10.0.0.5:8728",      // IPv4
		"host.example":  "host.example:8728",  // hostname
		"2001:db8::1":   "[2001:db8::1]:8728", // bare IPv6 — bracketed (was "too many colons")
		"[2001:db8::1]": "[2001:db8::1]:8728", // already-bracketed IPv6 — not double-wrapped
		" 2001:db8::1 ": "[2001:db8::1]:8728", // trimmed then bracketed
		"":              ":8728",              // empty host — unchanged behaviour
	}
	for host, want := range cases {
		if got := JoinHostPort(host, 8728); got != want {
			t.Errorf("JoinHostPort(%q, 8728) = %q, want %q", host, got, want)
		}
	}
}

func TestIsConnError(t *testing.T) {
	broken := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		net.ErrClosed,
		syscall.EPIPE,
		syscall.ECONNRESET,
		fmt.Errorf("write tcp 192.168.79.216:40746->192.168.78.202:8728: write: broken pipe"),
		fmt.Errorf("read tcp 10.0.0.1:1->10.0.0.2:8728: read: connection reset by peer"),
		fmt.Errorf("wrapped: %w", io.EOF),
	}
	for _, err := range broken {
		if !isConnError(err) {
			t.Errorf("isConnError(%v) = false, want true", err)
		}
	}

	// A command timeout is not listed here on purpose: RunCommandWithTimeout
	// closes and evicts that client on the timeout branch directly, so the
	// error text never has to be classified.

	// RouterOS rejecting a command must not cost us a working connection.
	fine := []error{
		nil,
		fmt.Errorf("no such command prefix"),
		fmt.Errorf("from RouterOS device: unknown parameter"),
		fmt.Errorf("input does not match any value of interface"),
	}
	for _, err := range fine {
		if isConnError(err) {
			t.Errorf("isConnError(%v) = true, want false", err)
		}
	}
}

// TestGetLiveRedialThrottle pins the behaviour that keeps a down device from
// turning a 1s poll loop into a dial storm: GetLive redials at most once per
// ReconnectCooldown, and returns nil in between.
func TestGetLiveRedialThrottle(t *testing.T) {
	var dials int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&dials, 1)
			c.Close() // never completes the RouterOS login, so Dial fails
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	old := ReconnectCooldown
	ReconnectCooldown = 60 * time.Second
	defer func() { ReconnectCooldown = old }()

	p := NewPool(false)
	// Seed the dial spec the way a successful Dial would have.
	p.specs["dev1"] = dialSpec{address: host, port: port, username: "u", password: "p"}

	for range 5 {
		if c := p.GetLive("dev1"); c != nil {
			t.Fatal("GetLive returned a client from a server that never logs in")
		}
	}
	if got := atomic.LoadInt32(&dials); got != 1 {
		t.Errorf("dial attempts = %d, want 1 (throttled by ReconnectCooldown)", got)
	}

	// Once the cooldown lapses a new attempt is allowed.
	p.mu.Lock()
	p.lastDial["dev1"] = time.Now().Add(-2 * time.Minute)
	p.mu.Unlock()
	_ = p.GetLive("dev1")
	if got := atomic.LoadInt32(&dials); got != 2 {
		t.Errorf("dial attempts after cooldown = %d, want 2", got)
	}
}

// TestGetLiveNoSpec: a device that was never dialled has no credentials to
// redial with, so GetLive must report "no connection" rather than guess.
func TestGetLiveNoSpec(t *testing.T) {
	p := NewPool(false)
	if c := p.GetLive("never-dialled"); c != nil {
		t.Error("GetLive invented a connection for an unknown device")
	}
}

// TestEvictFailedClientUnregistered: DialOnce clients aren't pooled, so evicting
// one must be a no-op rather than a panic or a cross-device close.
func TestEvictFailedClientUnregistered(t *testing.T) {
	evictFailedClient(nil)
}
