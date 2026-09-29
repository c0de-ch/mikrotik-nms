package flowview

import (
	"cmp"
	"net/netip"
	"slices"
	"strings"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Endpoint classes.
const (
	ClassInternal  = "internal"
	ClassExternal  = "external"
	ClassSelf      = "self"
	ClassMulticast = "multicast"
	ClassVPN       = "vpn"
)

// Plan classifies endpoint addresses (contract §7.2) from the address plan
// the collector derives from managed devices (flow_prefixes), the admin's
// flow_internal_prefixes / flow_external_prefixes, the known hosts
// (mac_lookup IPs) and the point's exporter's own addresses. Immutable once
// built; safe for concurrent use.
type Plan struct {
	rows     []queries.FlowPrefix // most specific first (host-length rows are in hosts)
	hosts    map[netip.Addr]string
	internal []netip.Prefix
	external []netip.Prefix
	known    map[netip.Addr]bool
	self     map[netip.Addr]bool
	bcast    map[netip.Addr]bool // broadcast addresses of internal IPv4 plan prefixes
}

// unmapPrefix returns p with a v4-mapped address unmapped, masked.
func unmapPrefix(p netip.Prefix) netip.Prefix {
	if a := p.Addr(); a.Is4In6() {
		bits := p.Bits() - 96
		if bits < 0 {
			bits = 0
		}
		p = netip.PrefixFrom(a.Unmap(), bits)
	}
	return p.Masked()
}

// unmapSet copies an address set with every key unmapped.
func unmapSet(in map[netip.Addr]bool) map[netip.Addr]bool {
	out := make(map[netip.Addr]bool, len(in))
	for a, ok := range in {
		if ok && a.IsValid() {
			out[a.Unmap()] = true
		}
	}
	return out
}

// broadcastOf returns the directed broadcast address of an IPv4 prefix of
// length <= 30.
func broadcastOf(p netip.Prefix) (netip.Addr, bool) {
	if !p.Addr().Is4() || p.Bits() > 30 {
		return netip.Addr{}, false
	}
	b := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= (1 << host) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

// NewPlan builds a classifier. rows is the collector's address plan;
// internal / external are the parsed flow_internal_prefixes /
// flow_external_prefixes; known is the set of IPs present in mac_lookup;
// self the point's exporter's own addresses (see WithSelf).
func NewPlan(rows []queries.FlowPrefix, internal, external []netip.Prefix, known map[netip.Addr]bool, self map[netip.Addr]bool) *Plan {
	pl := &Plan{known: unmapSet(known), self: unmapSet(self), bcast: map[netip.Addr]bool{}, hosts: map[netip.Addr]string{}}
	for _, r := range rows {
		if !r.Prefix.IsValid() {
			continue
		}
		r.Prefix = unmapPrefix(r.Prefix)
		if r.Prefix.IsSingleIP() { // the collector's per-address rows: a map lookup, not a scan
			pl.hosts[r.Prefix.Addr()] = r.Class
			continue
		}
		pl.rows = append(pl.rows, r)
		if r.Class == ClassInternal {
			if b, ok := broadcastOf(r.Prefix); ok {
				pl.bcast[b] = true
			}
		}
	}
	slices.SortStableFunc(pl.rows, func(a, b queries.FlowPrefix) int { return cmp.Compare(b.Prefix.Bits(), a.Prefix.Bits()) })
	for _, p := range internal {
		if p.IsValid() {
			pl.internal = append(pl.internal, unmapPrefix(p))
		}
	}
	for _, p := range external {
		if p.IsValid() {
			pl.external = append(pl.external, unmapPrefix(p))
		}
	}
	return pl
}

// WithSelf returns a copy of pl whose "self" set is self (the plan rows and
// settings are shared): one base plan per request, one per point's exporter.
func (pl *Plan) WithSelf(self map[netip.Addr]bool) *Plan {
	if pl == nil {
		return NewPlan(nil, nil, nil, nil, self)
	}
	cp := *pl
	cp.self = unmapSet(self)
	return &cp
}

// ParsePrefixList parses a CSV (also newline, space or semicolon separated)
// of CIDRs or bare IPs; a bare IP is a /32 or /128. Junk entries are
// skipped. Prefixes are masked and v4-mapped addresses unmapped.
func ParsePrefixList(csv string) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.FieldsFunc(csv, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' || r == ';'
	}) {
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, unmapPrefix(p))
			continue
		}
		if a, err := netip.ParseAddr(part); err == nil && a.Zone() == "" {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}

func containedIn(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// planClass maps a plan row's class to an endpoint class.
func planClass(c string) string {
	switch c {
	case "transit":
		return ClassExternal
	case ClassVPN:
		return ClassVPN
	}
	return ClassInternal
}

// Class classifies an address, first match wins (contract §7.2):
//  1. multicast / broadcast (224.0.0.0/4, ff00::/8, 255.255.255.255, the
//     broadcast address of an internal plan prefix) → multicast
//  2. the point's exporter's own address → self
//  3. inside flow_external_prefixes → external
//  4. inside flow_internal_prefixes → internal
//  5. most specific plan prefix → its class (transit → external)
//  6. present in mac_lookup (a known host) → internal
//  7. otherwise external
//
// RFC 1918 is deliberately not a blanket rule. A nil Plan classifies
// everything that is not multicast as external.
func (pl *Plan) Class(a netip.Addr) string {
	a = a.Unmap()
	if a.IsMulticast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return ClassMulticast
	}
	if pl == nil || !a.IsValid() {
		return ClassExternal
	}
	if pl.bcast[a] {
		return ClassMulticast
	}
	if pl.self[a] {
		return ClassSelf
	}
	if containedIn(pl.external, a) {
		return ClassExternal
	}
	if containedIn(pl.internal, a) {
		return ClassInternal
	}
	if c, ok := pl.hosts[a]; ok {
		return planClass(c)
	}
	for _, r := range pl.rows {
		if r.Prefix.Contains(a) {
			return planClass(r.Class)
		}
	}
	if pl.known[a] {
		return ClassInternal
	}
	return ClassExternal
}
