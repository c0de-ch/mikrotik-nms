package main

import (
	"math/rand/v2"
	"time"

	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
)

// The -mix traffic: lab hosts (the UI names and attaches them when the dev
// database knows them in mac_lookup / port_hosts) talking to common internet
// services at a few Mbit/s, on top of the contract records R1–R6 / O1–O3.

// mixService is one internet service with typical per-host rates.
type mixService struct {
	remote         string
	proto          uint8
	port           uint16
	downBps, upBps float64 // bytes per second
}

var mixServices = []mixService{
	{"203.0.113.110", 6, 443, 350_000, 18_000},  // Google HTTPS
	{"203.0.113.110", 17, 443, 550_000, 14_000}, // YouTube QUIC
	{"198.51.100.229", 6, 443, 60_000, 9_000},   // Cloudflare-fronted site
	{"198.51.100.207", 6, 443, 120_000, 8_000},  // Apple
	{"198.51.100.130", 6, 443, 650_000, 11_000}, // Netflix
	{"198.51.100.4", 6, 22, 4_000, 3_000},       // GitHub SSH
	{"1.1.1.1", 17, 53, 400, 250},               // DNS
	{"198.51.100.1", 17, 123, 90, 90},           // NTP
}

// mixHost is a lab host and the services it uses (indexes into mixServices).
type mixHost struct {
	ip       string
	services []int
}

// RouterOS side: net28 hosts, masqueraded to the Starlink CGNAT address
// (download: dst = CGNAT, IE 226 = host; upload: IE 225 = CGNAT). 192.168.40.25
// (ccr2004-hv05) is attached at RB5009 eth3, so the trunk views exclude it.
var mixRouterOSHosts = []mixHost{
	{"192.168.40.20", []int{1, 3, 6}},    // iPhone
	{"192.168.40.28", []int{1, 4, 6}},    // iPad
	{"192.168.40.29", []int{0, 2, 3, 5}}, // Mac
	{"192.168.40.21", []int{0, 6, 7}},    // laptop
	{"192.168.40.52", []int{2, 6, 7}},    // netalertx
	{"192.168.40.24", []int{0, 7}},       // ha
	{"192.168.40.25", []int{0, 5, 7}},    // ccr2004-hv05 (eth3)
}

// OPNsense side: hosts on the 192.168.60.0/23 LAN (LAN ifIndex 1, WAN 7).
var mixOPNsenseHosts = []mixHost{
	{"192.168.60.7", []int{0, 1, 6}},     // desktop
	{"192.168.60.46", []int{0, 2, 3, 6}}, // office
	{"192.168.60.97", []int{4, 6, 7}},    // nas01
	{"192.168.60.99", []int{0, 7}},       // nas02
	{"192.168.61.162", []int{3, 6}},      // homebridge
	{"192.168.60.82", []int{1, 4}},       // media
	{"192.168.60.75", []int{6, 7}},       // pihole01
}

const (
	cgnatAddr = "100.101.102.103"
	ifWAN     = 1  // RB5009 eth1-wan
	ifBridge  = 10 // RB5009 bridge (VLAN 1 L3, 192.168.60.0/23)
	ifNet28   = 13 // RB5009 net28 (VLAN 28 L3)
	opnLAN    = 1
	opnWAN    = 7
)

// mixScale returns bytes/packets for rate bytesPerSec over window, ±20 %.
func mixScale(rng *rand.Rand, bytesPerSec float64, window time.Duration, pktSize float64) (uint64, uint64) {
	b := jitter(rng, uint64(bytesPerSec*window.Seconds()))
	return b, max(uint64(float64(b)/pktSize), 1)
}

// mixRouterOS returns the RouterOS mix records for the window ending at now.
func mixRouterOS(rng *rand.Rand, now time.Time, window time.Duration, sysInitMs uint64) []pktgen.IPFIXFlow {
	up := func(d time.Duration) uint32 { return uint32(uint64(now.Add(d).UnixMilli()) - sysInitMs) }
	start, end := up(-window), up(-time.Second)
	var out []pktgen.IPFIXFlow
	for hi, h := range mixRouterOSHosts {
		for _, si := range h.services {
			s := mixServices[si]
			eph := uint16(49152 + hi*512 + si*7)
			natEph := uint16(20000 + hi*512 + si*7)
			db, dp := mixScale(rng, s.downBps, window, 1200)
			ub, upk := mixScale(rng, s.upBps, window, 90)
			out = append(out,
				pktgen.IPFIXFlow{Src: s.remote, Dst: cgnatAddr, SrcPort: s.port, DstPort: natEph, Proto: s.proto,
					Bytes: db, Packets: dp, InIf: ifWAN, OutIf: ifNet28, NextHop: h.ip, StartUp: start, EndUp: end,
					SysInitMs: sysInitMs, NatSrc: s.remote, NatDst: h.ip, NatSrcPort: s.port, NatDstPort: eph},
				pktgen.IPFIXFlow{Src: h.ip, Dst: s.remote, SrcPort: eph, DstPort: s.port, Proto: s.proto,
					Bytes: ub, Packets: upk, InIf: ifNet28, OutIf: ifWAN, NextHop: "100.64.0.1", StartUp: start, EndUp: end,
					SysInitMs: sysInitMs, NatSrc: cgnatAddr, NatDst: s.remote, NatSrcPort: natEph, NatDstPort: s.port})
		}
	}
	// Inter-VLAN routed by the RB5009 (crosses the sfp-sfpplus1 trunk twice):
	// Mac (net28) <-> nas01 (VLAN 1) SMB, and ha polling desktop.
	for _, c := range []struct {
		a, b           string
		port           uint16
		downBps, upBps float64
	}{
		{"192.168.40.29", "192.168.60.97", 445, 900_000, 60_000},
		{"192.168.40.24", "192.168.60.7", 8123, 20_000, 4_000},
	} {
		db, dp := mixScale(rng, c.downBps, window, 1400)
		ub, upk := mixScale(rng, c.upBps, window, 120)
		out = append(out,
			pktgen.IPFIXFlow{Src: c.b, Dst: c.a, SrcPort: c.port, DstPort: 51515, Proto: 6, Bytes: db, Packets: dp,
				InIf: ifBridge, OutIf: ifNet28, StartUp: start, EndUp: end, SysInitMs: sysInitMs},
			pktgen.IPFIXFlow{Src: c.a, Dst: c.b, SrcPort: 51515, DstPort: c.port, Proto: 6, Bytes: ub, Packets: upk,
				InIf: ifNet28, OutIf: ifBridge, StartUp: start, EndUp: end, SysInitMs: sysInitMs})
	}
	return out
}

// mixOPNsense returns the OPNsense (ng_netflow, LAN-only capture) mix records
// for a header SysUptime of uptime: LAN ingress = upload (in LAN, out WAN),
// LAN egress = download (in WAN, out LAN).
func mixOPNsense(rng *rand.Rand, uptime uint32, window time.Duration) []pktgen.V9Flow {
	first, last := uptime-uint32(window.Milliseconds()), uptime-1_000
	var out []pktgen.V9Flow
	for hi, h := range mixOPNsenseHosts {
		for _, si := range h.services {
			s := mixServices[si]
			eph := uint16(49152 + hi*512 + si*7)
			db, dp := mixScale(rng, s.downBps, window, 1200)
			ub, upk := mixScale(rng, s.upBps, window, 90)
			out = append(out,
				pktgen.V9Flow{Src: h.ip, Dst: s.remote, NextHop: "192.168.150.1", InIf: opnLAN, OutIf: opnWAN,
					Packets: uint32(upk), Bytes: uint32(ub), First: first, Last: last, SrcPort: eph, DstPort: s.port,
					Proto: s.proto, TCPFlags: 0x1b, SrcMask: 23},
				pktgen.V9Flow{Src: s.remote, Dst: h.ip, InIf: opnWAN, OutIf: opnLAN,
					Packets: uint32(dp), Bytes: uint32(db), First: first, Last: last, SrcPort: s.port, DstPort: eph,
					Proto: s.proto, TCPFlags: 0x1b, DstMask: 23})
		}
	}
	return out
}

// chunk splits flows into groups of at most n (one datagram each).
func chunk[T any](flows []T, n int) [][]T {
	var out [][]T
	for len(flows) > n {
		out = append(out, flows[:n])
		flows = flows[n:]
	}
	if len(flows) > 0 {
		out = append(out, flows)
	}
	return out
}
