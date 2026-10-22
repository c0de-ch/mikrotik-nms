package routeros

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
)

// DialPolicy is the process-wide fence on device I/O. main derives it from
// MIKROTIK_NMS_POLLERS and MIKROTIK_NMS_ALLOWED_TARGET_CIDRS and installs it
// with SetDialPolicy before anything dials; every path that reaches a device
// — the pool, one-shot connections, the liveness ping, subnet sweeps and MNDP
// discovery — consults it. The point is that a checkout started against a
// copy of a production database can never poll the production fleet: the
// code default is off, and "local" only reaches the listed prefixes.
type DialPolicy struct {
	// Enabled is false while polling is off (MIKROTIK_NMS_POLLERS=off): no
	// device or UDP I/O at all, whatever Allowed says.
	Enabled bool
	// Allowed are the prefixes a target may resolve into. With Enabled set an
	// empty list refuses every target — config refuses that combination at
	// startup, and the check stays fail-closed should it ever get here.
	Allowed []netip.Prefix
}

var dialPolicy atomic.Pointer[DialPolicy]

// SetDialPolicy installs the policy every later dial consults. A process that
// never calls it (library tests, the flowreplay tool) is unrestricted.
func SetDialPolicy(p *DialPolicy) { dialPolicy.Store(p) }

// ErrPollersOff is returned by every device dial while polling is off.
var ErrPollersOff = errors.New("device polling is off on this instance (MIKROTIK_NMS_POLLERS=off)")

// ErrTargetNotAllowed matches (errors.Is) every *TargetNotAllowedError.
var ErrTargetNotAllowed = errors.New("target is outside MIKROTIK_NMS_ALLOWED_TARGET_CIDRS")

// TargetNotAllowedError names the target the allow-list refused.
type TargetNotAllowedError struct {
	Host    string     // the address as the caller gave it (or the swept prefix)
	Addr    netip.Addr // the resolved address outside the allow-list; zero when Host says it all
	Allowed []netip.Prefix
}

func (e *TargetNotAllowedError) Error() string {
	target := e.Host
	if e.Addr.IsValid() && e.Addr.String() != strings.Trim(e.Host, "[]") {
		target = fmt.Sprintf("%s (%s)", e.Host, e.Addr)
	}
	return fmt.Sprintf("target %s is outside MIKROTIK_NMS_ALLOWED_TARGET_CIDRS (%s)", target, describePrefixes(e.Allowed))
}

// Is lets errors.Is(err, ErrTargetNotAllowed) match without the caller
// knowing the concrete type.
func (e *TargetNotAllowedError) Is(target error) bool { return target == ErrTargetNotAllowed }

func describePrefixes(ps []netip.Prefix) string {
	if len(ps) == 0 {
		return "no prefixes are allowed"
	}
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = p.String()
	}
	return "allowed: " + strings.Join(s, ", ")
}

// PollingAllowed reports whether this process may do device I/O at all: nil,
// or ErrPollersOff.
func PollingAllowed() error {
	if p := dialPolicy.Load(); p != nil && !p.Enabled {
		return ErrPollersOff
	}
	return nil
}

// resolveTimeout bounds the lookup CheckTarget does for a hostname.
const resolveTimeout = 5 * time.Second

// CheckTarget decides whether a device address — an IP literal, optionally
// bracketed, or a hostname — may be dialled: nil, ErrPollersOff, a
// *TargetNotAllowedError, or a resolver error. A hostname is checked on every
// address it resolves to and all of them must be allowed, so a name that
// resolves both inside and outside the fence is refused. The dial that
// follows resolves the name again; the window between the two lookups is
// accepted — the fence is against mistakes, not a hostile resolver.
func CheckTarget(host string) error {
	p := dialPolicy.Load()
	if p == nil {
		return nil
	}
	if !p.Enabled {
		return ErrPollersOff
	}
	h := strings.TrimSpace(host)
	if len(h) >= 2 && h[0] == '[' && h[len(h)-1] == ']' {
		h = h[1 : len(h)-1]
	}
	if h == "" {
		return &TargetNotAllowedError{Host: host, Allowed: p.Allowed}
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return checkAddr(host, addr, p.Allowed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", h)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if err := checkAddr(host, a, p.Allowed); err != nil {
			return err
		}
	}
	return nil
}

// CheckPrefix decides whether a whole prefix (a subnet sweep) may be probed:
// it must lie inside one allowed prefix.
func CheckPrefix(cidr netip.Prefix) error {
	p := dialPolicy.Load()
	if p == nil {
		return nil
	}
	if !p.Enabled {
		return ErrPollersOff
	}
	c := cidr.Masked()
	if a := c.Addr(); a.Is4In6() && c.Bits() >= 96 {
		c = netip.PrefixFrom(a.Unmap(), c.Bits()-96)
	}
	for _, a := range p.Allowed {
		if a.Bits() <= c.Bits() && a.Contains(c.Addr()) {
			return nil
		}
	}
	return &TargetNotAllowedError{Host: cidr.String(), Allowed: p.Allowed}
}

func checkAddr(host string, addr netip.Addr, allowed []netip.Prefix) error {
	a := addr.Unmap().WithZone("")
	for _, p := range allowed {
		if p.Contains(a) {
			return nil
		}
	}
	return &TargetNotAllowedError{Host: host, Addr: a, Allowed: allowed}
}
