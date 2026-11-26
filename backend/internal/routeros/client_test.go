package routeros

import (
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
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
		fmt.Errorf("write tcp 192.0.2.10:40746->192.0.2.20:8728: write: broken pipe"),
		fmt.Errorf("read tcp 192.0.2.10:1->192.0.2.20:8728: read: connection reset by peer"),
		fmt.Errorf("wrapped: %w", io.EOF),
	}
	for _, err := range broken {
		if !isConnError(err) {
			t.Errorf("isConnError(%v) = false, want true", err)
		}
	}

	// A command timeout is classified by its type (TimeoutError), see
	// IsSessionFatal; the pool tests are in pool_test.go.

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
