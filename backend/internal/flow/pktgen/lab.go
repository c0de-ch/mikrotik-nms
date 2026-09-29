package pktgen

import "time"

// LabRouterOS returns the contract's RouterOS records R1–R6 for an export at
// export, with the router's sysInit at sysInitMs (epoch ms). withSysInit
// false omits IE 160 (the records then match templates sent without it).
// R5 is the router's own export: rosSrc -> collector:collectorPort (UDP).
//
//	R1 download 142.250.203.100:443 -> 100.114.241.213:50514, in 1 out 13, 226 = 192.168.28.33
//	R2 upload   192.168.28.33:50514 -> 142.250.203.100:443,   in 13 out 1, 225 = 100.114.241.213
//	R3 firewall003 NAT 192.168.28.81:40000 -> 1.1.1.1:443,     in 13 out 1
//	R4 NMS polling 192.168.79.216:51000 -> 192.168.78.202:8728, in 10 out 0
//	R5 self-export, in 0 out 10
//	R6 IPv6 2606:4700::1111:443 -> 2a01:4f8:202:13d1:28::33:50600, in 1 out 13
func LabRouterOS(export time.Time, sysInitMs uint64, withSysInit bool, rosSrc, collector string, collectorPort uint16) []IPFIXFlow {
	up := func(d time.Duration) uint32 {
		return uint32(uint64(export.Add(d).UnixMilli()) - sysInitMs)
	}
	si := sysInitMs
	if !withSysInit {
		si = 0
	}
	return []IPFIXFlow{
		{Src: "142.250.203.100", Dst: "100.114.241.213", SrcPort: 443, DstPort: 50514, Proto: 6, TCPFlags: 0x18,
			Bytes: 5_000_000_000, Packets: 3_500_000, InIf: 1, OutIf: 13, NextHop: "192.168.28.33",
			StartUp: up(-45 * time.Second), EndUp: up(-2 * time.Second), SysInitMs: si,
			NatSrc: "142.250.203.100", NatDst: "192.168.28.33", NatSrcPort: 443, NatDstPort: 50514},
		{Src: "192.168.28.33", Dst: "142.250.203.100", SrcPort: 50514, DstPort: 443, Proto: 6, TCPFlags: 0x18,
			Bytes: 80_000, Packets: 100, InIf: 13, OutIf: 1, NextHop: "100.64.0.1",
			StartUp: up(-45 * time.Second), EndUp: up(-2 * time.Second), SysInitMs: si,
			NatSrc: "100.114.241.213", NatDst: "142.250.203.100", NatSrcPort: 61000, NatDstPort: 443},
		{Src: "192.168.28.81", Dst: "1.1.1.1", SrcPort: 40000, DstPort: 443, Proto: 6,
			Bytes: 10_000, Packets: 20, InIf: 13, OutIf: 1, NextHop: "100.64.0.1",
			StartUp: up(-30 * time.Second), EndUp: up(-time.Second), SysInitMs: si,
			NatSrc: "100.114.241.213", NatDst: "1.1.1.1", NatSrcPort: 40001, NatDstPort: 443},
		{Src: "192.168.79.216", Dst: "192.168.78.202", SrcPort: 51000, DstPort: 8728, Proto: 6,
			Bytes: 5_000, Packets: 40, InIf: 10, OutIf: 0,
			StartUp: up(-59 * time.Second), EndUp: up(-500 * time.Millisecond), SysInitMs: si},
		{Src: rosSrc, Dst: collector, SrcPort: 40001, DstPort: collectorPort, Proto: 17,
			Bytes: 3_000, Packets: 3, InIf: 0, OutIf: 10,
			StartUp: up(-60 * time.Second), EndUp: up(0), SysInitMs: si},
		{Src: "2606:4700::1111", Dst: "2a01:4f8:202:13d1:28::33", SrcPort: 443, DstPort: 50600, Proto: 6,
			Bytes: 64_000, Packets: 50, InIf: 1, OutIf: 13,
			StartUp: up(-20 * time.Second), EndUp: up(-3 * time.Second), SysInitMs: si},
	}
}

// LabOPNsense returns the contract's ng_netflow records O1–O3 for a header
// SysUptime of uptime (ms), plus the samplicate self-export record
// opnSrc -> collector:collectorPort (UDP, in 0 out 1).
//
//	O1 upload   192.168.78.60:53124 -> 9.9.9.9:853, in 1 out 7, 1480 B / 12 pkts
//	O2 download 9.9.9.9:853 -> 192.168.78.60:53124, in 7 out 1, 64000 B / 50 pkts
//	O3 IPv6     2a01:4f8:202:13d1:78::60:40000 -> 2620:fe::fe:853, in 1 out 7
func LabOPNsense(uptime uint32, opnSrc, collector string, collectorPort uint16) []V9Flow {
	return []V9Flow{
		{Src: "192.168.78.60", Dst: "9.9.9.9", NextHop: "192.168.111.1", InIf: 1, OutIf: 7, Packets: 12, Bytes: 1480,
			First: uptime - 40_000, Last: uptime - 1_000, SrcPort: 53124, DstPort: 853, TCPFlags: 0x1b, Proto: 6,
			SrcMask: 23},
		{Src: "9.9.9.9", Dst: "192.168.78.60", InIf: 7, OutIf: 1, Packets: 50, Bytes: 64_000,
			First: uptime - 40_000, Last: uptime - 1_000, SrcPort: 853, DstPort: 53124, TCPFlags: 0x1b, Proto: 6,
			DstMask: 23},
		{Src: "2a01:4f8:202:13d1:78::60", Dst: "2620:fe::fe", InIf: 1, OutIf: 7, Packets: 10, Bytes: 2000,
			First: uptime - 30_000, Last: uptime - 2_000, SrcPort: 40000, DstPort: 853, Proto: 6, SrcMask: 64},
		{Src: opnSrc, Dst: collector, InIf: 0, OutIf: 1, Packets: 2, Bytes: 1400,
			First: uptime - 10_000, Last: uptime - 500, SrcPort: 49152, DstPort: collectorPort, Proto: 17},
	}
}
