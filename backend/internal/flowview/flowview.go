// Package flowview is the pure query-side logic of the measured flow views:
// which stored flow rows belong to an observation point and in which
// direction, how endpoints are classified against the address plan, top-N
// grouping, the measured Sankey, the flow vs port-counter mapping and the
// derived-view suggestions. It does no DB access and no I/O: the API layer
// loads the inputs (queries types) and renders the outputs.
//
// A point is always one exporter plus a set S of its ifIndexes. Rows of two
// exporters are never added together anywhere in this package.
package flowview

import (
	"cmp"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Dir is a traffic direction relative to a point: download = toward the
// point's local side, upload = away from it.
type Dir string

// Directions.
const (
	Download Dir = "download"
	Upload   Dir = "upload"
)

// Point facings.
const (
	FacingUp   = "up"   // the point's interfaces face the Internet
	FacingDown = "down" // they face internal hosts
)

// Point is an observation point plus the resolved exclude_attach filter:
// ExcludeIPs holds the IPs whose MAC is learned on one of the point's
// exclude_attach ports (filled by the API from port_hosts ⋈ mac_lookup).
type Point struct {
	queries.FlowPoint
	ExcludeIPs map[netip.Addr]bool
}

// has reports whether ifIndex idx is in the point's set S. ifIndex 0 (the
// exporter itself) is never in S.
func (p Point) has(idx uint32) bool {
	return idx != 0 && slices.Contains(p.IfIndexes, idx)
}

// Directions classifies a stored row with (inIf, outIf) for this point
// (contract §6.2): facing up → download ⇔ inIf ∈ S, upload ⇔ outIf ∈ S;
// facing down → download ⇔ outIf ∈ S, upload ⇔ inIf ∈ S. A row may count in
// both directions (it crossed S twice, e.g. a hairpin).
func (p Point) Directions(inIf, outIf uint32) (down, up bool) {
	inS, outS := p.has(inIf), p.has(outIf)
	if p.Facing == FacingUp {
		return inS, outS
	}
	return outS, inS
}

// DirRow is one stored row seen in one direction of a point. Download: Local
// = Dst, Remote = Src; upload: Local = Src, Remote = Dst. Other marks the
// exporter's folded "below top-N" row (no endpoints, never filtered).
type DirRow struct {
	Bucket                time.Time
	Src, Dst              netip.Addr
	Local, Remote         netip.Addr
	Proto                 uint8
	Port                  uint16
	Bytes, Packets, Flows int64
	Other                 bool
}

// isOther reports the folded other row (src = dst = empty blob).
func isOther(r queries.FlowAgg) bool {
	return !r.Src.IsValid() || !r.Dst.IsValid()
}

// Select keeps p's rows in direction d (§6.2) and applies p.LocalInternal
// (the local endpoint's class must be internal or vpn) and p.ExcludeIPs
// (§6.3); folded other rows are kept with Other = true. pl may be nil when
// the point has no local_internal filter.
func Select(rows []queries.FlowAgg, p Point, d Dir, pl *Plan) []DirRow {
	out := make([]DirRow, 0, len(rows))
	for _, r := range rows {
		down, up := p.Directions(r.InIf, r.OutIf)
		if (d == Download && !down) || (d == Upload && !up) {
			continue
		}
		dr := DirRow{Bucket: r.Bucket, Src: r.Src, Dst: r.Dst, Proto: r.Proto, Port: r.Port,
			Bytes: r.Bytes, Packets: r.Packets, Flows: r.Flows}
		if isOther(r) {
			dr.Src, dr.Dst, dr.Other = netip.Addr{}, netip.Addr{}, true
			out = append(out, dr)
			continue
		}
		if d == Download {
			dr.Local, dr.Remote = r.Dst, r.Src
		} else {
			dr.Local, dr.Remote = r.Src, r.Dst
		}
		if p.LocalInternal {
			switch pl.Class(dr.Local) {
			case ClassInternal, ClassVPN:
			default:
				continue
			}
		}
		if p.ExcludeIPs[dr.Local.Unmap()] {
			continue
		}
		out = append(out, dr)
	}
	return out
}

// SumByBucket is Totals(Select(rows, p, d, pl)) per bucket for both
// directions at once, without building DirRows: rows may come from
// queries.AggregateFlowBytes (no endpoints: every row counts like the folded
// other row, which is only right for a point without row filters) or from
// AggregateFlows. The local-side filters are applied as in Select.
func SumByBucket(rows []queries.FlowAgg, p Point, pl *Plan) (down, up map[int64]int64) {
	down, up = map[int64]int64{}, map[int64]int64{}
	keep := func(local netip.Addr) bool {
		if p.LocalInternal {
			switch pl.Class(local) {
			case ClassInternal, ClassVPN:
			default:
				return false
			}
		}
		return !p.ExcludeIPs[local.Unmap()]
	}
	for _, r := range rows {
		d, u := p.Directions(r.InIf, r.OutIf)
		if !d && !u {
			continue
		}
		b := r.Bucket.Unix()
		other := isOther(r)
		if d && (other || keep(r.Dst)) {
			down[b] += r.Bytes
		}
		if u && (other || keep(r.Src)) {
			up[b] += r.Bytes
		}
	}
	return down, up
}

// Groupings of Group.
const (
	GroupSrc  = "src"
	GroupDst  = "dst"
	GroupPair = "pair"
	GroupConv = "conv"
	GroupApp  = "app"
)

// ValidGroup reports whether by is one of the five groupings.
func ValidGroup(by string) bool {
	switch by {
	case GroupSrc, GroupDst, GroupPair, GroupConv, GroupApp:
		return true
	}
	return false
}

// GroupRow is one group of Group. Only the fields of the grouping are set:
// src → Src; dst → Dst; pair → Src, Dst; conv → Src, Dst, Proto, Port;
// app → Proto, Port.
type GroupRow struct {
	Key                   string
	Src, Dst              netip.Addr
	Proto                 uint8
	Port                  uint16
	Bytes, Packets, Flows int64
}

// AppKey is the "<proto>/<port>" key of an app.
func AppKey(proto uint8, port uint16) string {
	return strconv.Itoa(int(proto)) + "/" + strconv.Itoa(int(port))
}

// groupKey builds the group key and the identifying fields of r under by.
func groupKey(r DirRow, by string) GroupRow {
	switch by {
	case GroupSrc:
		return GroupRow{Key: r.Src.String(), Src: r.Src}
	case GroupDst:
		return GroupRow{Key: r.Dst.String(), Dst: r.Dst}
	case GroupConv:
		return GroupRow{Key: r.Src.String() + "|" + r.Dst.String() + "|" + AppKey(r.Proto, r.Port),
			Src: r.Src, Dst: r.Dst, Proto: r.Proto, Port: r.Port}
	case GroupApp:
		return GroupRow{Key: AppKey(r.Proto, r.Port), Proto: r.Proto, Port: r.Port}
	}
	return GroupRow{Key: r.Src.String() + "|" + r.Dst.String(), Src: r.Src, Dst: r.Dst}
}

// Group groups rows by "src" | "dst" | "pair" | "conv" (src, dst, proto,
// port) | "app" (proto, port); an unknown by groups as "pair". It returns the
// top limit groups by bytes (ties: key ascending; limit <= 0 = no limit), the
// remainder plus every folded other row summed into other (Key ""), and the
// total bytes of all rows.
func Group(rows []DirRow, by string, limit int) (top []GroupRow, other GroupRow, total int64) {
	idx := make(map[string]int)
	var groups []GroupRow
	for _, r := range rows {
		total += r.Bytes
		if r.Other {
			other.Bytes += r.Bytes
			other.Packets += r.Packets
			other.Flows += r.Flows
			continue
		}
		g := groupKey(r, by)
		i, ok := idx[g.Key]
		if !ok {
			i = len(groups)
			idx[g.Key] = i
			groups = append(groups, g)
		}
		groups[i].Bytes += r.Bytes
		groups[i].Packets += r.Packets
		groups[i].Flows += r.Flows
	}
	slices.SortFunc(groups, func(a, b GroupRow) int {
		if c := cmp.Compare(b.Bytes, a.Bytes); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	if limit > 0 && len(groups) > limit {
		for _, g := range groups[limit:] {
			other.Bytes += g.Bytes
			other.Packets += g.Packets
			other.Flows += g.Flows
		}
		groups = groups[:limit]
	}
	if groups == nil {
		groups = []GroupRow{}
	}
	return groups, other, total
}

// Totals sums the bytes of rows (other rows included).
func Totals(rows []DirRow) int64 {
	var n int64
	for _, r := range rows {
		n += r.Bytes
	}
	return n
}

// CounterDownIsTx reports which Phase 1 port counter carries a point's
// download direction (contract §6.6): tx when (facing = down) XOR
// (port_side = peer), else rx. Checks: (up, same) → rx; (down, peer) → rx;
// (down, same) → tx; (up, peer) → tx.
func CounterDownIsTx(facing, portSide string) bool {
	return (facing == FacingDown) != (portSide == "peer")
}

// CounterBytes maps a port's rx/tx byte counters to the point's download /
// upload (see CounterDownIsTx).
func CounterBytes(facing, portSide string, rx, tx int64) (down, up int64) {
	if CounterDownIsTx(facing, portSide) {
		return tx, rx
	}
	return rx, tx
}
