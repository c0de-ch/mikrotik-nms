package routeros

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func withPolicy(t *testing.T, p *DialPolicy) {
	t.Helper()
	SetDialPolicy(p)
	t.Cleanup(func() { SetDialPolicy(nil) })
}

func prefixes(t *testing.T, s ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(s))
	for _, v := range s {
		out = append(out, netip.MustParsePrefix(v))
	}
	return out
}

// No policy installed (library tests, tools): nothing is fenced.
func TestCheckTargetUnrestrictedWithoutPolicy(t *testing.T) {
	withPolicy(t, nil)
	for _, h := range []string{"203.0.113.9", "[2001:db8::1]", "router.example"} {
		if err := CheckTarget(h); err != nil {
			t.Errorf("CheckTarget(%q) without a policy = %v, want nil", h, err)
		}
	}
	if err := PollingAllowed(); err != nil {
		t.Errorf("PollingAllowed without a policy = %v", err)
	}
}

// Polling off refuses everything before any address is looked at.
func TestCheckTargetPollersOff(t *testing.T) {
	withPolicy(t, &DialPolicy{Enabled: false, Allowed: prefixes(t, "0.0.0.0/0", "::/0")})
	if err := CheckTarget("127.0.0.1"); !errors.Is(err, ErrPollersOff) {
		t.Errorf("CheckTarget = %v, want ErrPollersOff", err)
	}
	if err := PollingAllowed(); !errors.Is(err, ErrPollersOff) {
		t.Errorf("PollingAllowed = %v, want ErrPollersOff", err)
	}
	if err := CheckPrefix(netip.MustParsePrefix("127.0.0.0/24")); !errors.Is(err, ErrPollersOff) {
		t.Errorf("CheckPrefix = %v, want ErrPollersOff", err)
	}
	if err := Ping("127.0.0.1", 1, time.Second); !errors.Is(err, ErrPollersOff) {
		t.Errorf("Ping = %v, want ErrPollersOff", err)
	}
	if _, err := DialOnce("127.0.0.1", 1, "u", "p", false, false); !errors.Is(err, ErrPollersOff) {
		t.Errorf("DialOnce = %v, want ErrPollersOff", err)
	}
	if _, err := NewPool(false).Dial("dev", "127.0.0.1", 1, "u", "p", false); !errors.Is(err, ErrPollersOff) {
		t.Errorf("Pool.Dial = %v, want ErrPollersOff", err)
	}
	if _, err := ScanMNDP(time.Millisecond); !errors.Is(err, ErrPollersOff) {
		t.Errorf("ScanMNDP = %v, want ErrPollersOff", err)
	}
}

func TestCheckTargetAllowList(t *testing.T) {
	withPolicy(t, &DialPolicy{Enabled: true, Allowed: prefixes(t, "192.0.2.0/24", "2001:db8::/32")})
	for _, h := range []string{"192.0.2.1", " 192.0.2.254 ", "2001:db8::1", "[2001:db8:1::1]", "::ffff:192.0.2.7", "2001:db8::1%eth0"} {
		if err := CheckTarget(h); err != nil {
			t.Errorf("CheckTarget(%q) = %v, want nil", h, err)
		}
	}
	for _, h := range []string{"192.0.3.1", "198.51.100.1", "2001:db9::1", "[::1]", ""} {
		err := CheckTarget(h)
		var tna *TargetNotAllowedError
		if !errors.As(err, &tna) || !errors.Is(err, ErrTargetNotAllowed) {
			t.Errorf("CheckTarget(%q) = %v, want *TargetNotAllowedError", h, err)
		}
	}
	// The message names the target and the allow-list so the operator can
	// fix either side.
	err := CheckTarget("198.51.100.1")
	if err == nil || !contains(err.Error(), "198.51.100.1") || !contains(err.Error(), "192.0.2.0/24") {
		t.Errorf("error message %q should name the target and the allowed prefixes", err)
	}
}

// An enabled policy with an empty allow-list is fail-closed.
func TestCheckTargetEmptyAllowListRefuses(t *testing.T) {
	withPolicy(t, &DialPolicy{Enabled: true})
	if err := CheckTarget("192.0.2.1"); !errors.Is(err, ErrTargetNotAllowed) {
		t.Errorf("CheckTarget = %v, want ErrTargetNotAllowed", err)
	}
}

// A hostname is judged by every address it resolves to.
func TestCheckTargetHostname(t *testing.T) {
	addrs, err := net.DefaultResolver.LookupNetIP(t.Context(), "ip", "localhost")
	if err != nil || len(addrs) == 0 {
		t.Skipf("localhost does not resolve here: %v", err)
	}
	withPolicy(t, &DialPolicy{Enabled: true, Allowed: prefixes(t, "127.0.0.0/8", "::1/128")})
	if err := CheckTarget("localhost"); err != nil {
		t.Errorf("CheckTarget(localhost) = %v, want nil", err)
	}
	withPolicy(t, &DialPolicy{Enabled: true, Allowed: prefixes(t, "192.0.2.0/24")})
	if err := CheckTarget("localhost"); !errors.Is(err, ErrTargetNotAllowed) {
		t.Errorf("CheckTarget(localhost) outside the fence = %v, want ErrTargetNotAllowed", err)
	}
	// A name that does not resolve is a resolver error, not a policy verdict.
	if err := CheckTarget("does-not-exist.invalid"); err == nil || errors.Is(err, ErrTargetNotAllowed) || errors.Is(err, ErrPollersOff) {
		t.Errorf("CheckTarget(unresolvable) = %v, want a resolver error", err)
	}
}

func TestCheckPrefix(t *testing.T) {
	withPolicy(t, &DialPolicy{Enabled: true, Allowed: prefixes(t, "10.0.0.0/8", "2001:db8::/32")})
	for _, c := range []string{"10.1.2.0/24", "10.0.0.0/8", "10.9.9.9/32", "2001:db8:1::/48"} {
		if err := CheckPrefix(netip.MustParsePrefix(c)); err != nil {
			t.Errorf("CheckPrefix(%s) = %v, want nil", c, err)
		}
	}
	for _, c := range []string{"0.0.0.0/0", "10.0.0.0/7", "192.0.2.0/24", "2001:db9::/48"} {
		if err := CheckPrefix(netip.MustParsePrefix(c)); !errors.Is(err, ErrTargetNotAllowed) {
			t.Errorf("CheckPrefix(%s) = %v, want ErrTargetNotAllowed", c, err)
		}
	}
	if _, err := ScanSubnet(t.Context(), "192.0.2.0/30", []int{1}, time.Millisecond, 1); !errors.Is(err, ErrTargetNotAllowed) {
		t.Errorf("ScanSubnet outside the fence = %v, want ErrTargetNotAllowed", err)
	}
}

// The liveness ping honours the fence but still reaches an allowed target.
func TestPingHonoursPolicy(t *testing.T) {
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
			c.Close()
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	withPolicy(t, &DialPolicy{Enabled: true, Allowed: prefixes(t, "127.0.0.0/8")})
	if err := Ping(host, port, time.Second); err != nil {
		t.Errorf("Ping inside the fence = %v, want nil", err)
	}
	withPolicy(t, &DialPolicy{Enabled: true, Allowed: prefixes(t, "192.0.2.0/24")})
	if err := Ping(host, port, time.Second); !errors.Is(err, ErrTargetNotAllowed) {
		t.Errorf("Ping outside the fence = %v, want ErrTargetNotAllowed", err)
	}
}

// The login watch's outbound-address lookup is fenced like a dial: with
// polling off, or a target outside the allow-list, it opens no socket and
// reports nothing; inside the fence (or without a policy) it answers.
func TestOutboundAddressFenced(t *testing.T) {
	withPolicy(t, nil)
	if a, ok := OutboundAddress("127.0.0.1", 8728); !ok || a != netip.MustParseAddr("127.0.0.1") {
		t.Fatalf("without a policy = %v %v, want 127.0.0.1", a, ok)
	}
	withPolicy(t, &DialPolicy{Enabled: false, Allowed: prefixes(t, "0.0.0.0/0", "::/0")})
	if a, ok := OutboundAddress("127.0.0.1", 8728); ok {
		t.Errorf("with polling off = %v, want nothing", a)
	}
	withPolicy(t, &DialPolicy{Enabled: true, Allowed: prefixes(t, "192.0.2.0/24")})
	if a, ok := OutboundAddress("127.0.0.1", 8728); ok {
		t.Errorf("outside the allow-list = %v, want nothing", a)
	}
	withPolicy(t, &DialPolicy{Enabled: true, Allowed: prefixes(t, "127.0.0.0/8")})
	if _, ok := OutboundAddress("127.0.0.1", 8728); !ok {
		t.Error("inside the allow-list: no address")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
