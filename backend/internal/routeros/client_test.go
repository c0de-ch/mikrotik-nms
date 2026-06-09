package routeros

import "testing"

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
