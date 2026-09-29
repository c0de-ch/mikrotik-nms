package decode

import "encoding/binary"

// parseEthernet extracts VLAN, L3 addresses, protocol and L4 ports from an
// sFlow sampled frame header (typically the first 64..128 bytes). It never
// reads past len(b) and returns false when no IP header was found. This is
// the only piece of goflow2's producer/proto we would otherwise need
// (ParseSampledHeader), and it drags in protobuf + reflection-based mapping.
func parseEthernet(b []byte, f *Flow) bool {
	if len(b) < 14 {
		return false
	}
	et := binary.BigEndian.Uint16(b[12:14])
	off := 14
	for i := 0; i < 2 && (et == 0x8100 || et == 0x88a8); i++ { // 802.1Q / QinQ
		if len(b) < off+4 {
			return false
		}
		if f.VLAN == 0 {
			f.VLAN = binary.BigEndian.Uint16(b[off:]) & 0x0fff
		}
		et = binary.BigEndian.Uint16(b[off+2:])
		off += 4
	}
	switch et {
	case 0x0800:
		return parseIPv4(b[off:], f)
	case 0x86dd:
		return parseIPv6(b[off:], f)
	}
	return false
}

func parseIPv4(b []byte, f *Flow) bool {
	if len(b) < 20 || b[0]>>4 != 4 {
		return false
	}
	ihl := int(b[0]&0x0f) * 4
	if ihl < 20 || len(b) < ihl {
		return false
	}
	f.Proto = b[9]
	f.SrcAddr = addrOf(b[12:16])
	f.DstAddr = addrOf(b[16:20])
	if binary.BigEndian.Uint16(b[6:8])&0x1fff != 0 { // non-first fragment: no L4 header
		return true
	}
	parseL4(b[ihl:], f)
	return true
}

func parseIPv6(b []byte, f *Flow) bool {
	if len(b) < 40 || b[0]>>4 != 6 {
		return false
	}
	nh := b[6]
	f.SrcAddr = addrOf(b[8:24])
	f.DstAddr = addrOf(b[24:40])
	p := b[40:]
	for i := 0; i < 4; i++ { // skip a bounded number of extension headers
		switch nh {
		case 0, 43, 60: // hop-by-hop, routing, destination options
			if len(p) < 8 {
				f.Proto = nh
				return true
			}
			l := (int(p[1]) + 1) * 8
			if len(p) < l {
				f.Proto = nh
				return true
			}
			nh, p = p[0], p[l:]
			continue
		case 44: // fragment
			if len(p) < 8 {
				f.Proto = nh
				return true
			}
			frag := binary.BigEndian.Uint16(p[2:4]) &^ 0x7
			nh, p = p[0], p[8:]
			if frag != 0 {
				f.Proto = nh
				return true
			}
			continue
		}
		break
	}
	f.Proto = nh
	parseL4(p, f)
	return true
}

func parseL4(b []byte, f *Flow) {
	switch f.Proto {
	case 6, 17, 132, 136: // TCP, UDP, SCTP, UDP-Lite
		if len(b) >= 4 {
			f.SrcPort = binary.BigEndian.Uint16(b[0:2])
			f.DstPort = binary.BigEndian.Uint16(b[2:4])
		}
		if f.Proto == 6 && len(b) >= 14 {
			f.TCPFlags = b[13]
		}
	}
}
