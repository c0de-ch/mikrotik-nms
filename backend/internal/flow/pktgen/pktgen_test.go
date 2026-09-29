package pktgen

import (
	"encoding/binary"
	"testing"
)

func TestRecordSizes(t *testing.T) {
	sum := func(fs []Field) int {
		n := 0
		for _, f := range fs {
			n += int(f.Len)
		}
		return n
	}
	if n := sum(OPNsenseV9FieldsV4); n != 57 {
		t.Errorf("ng_netflow IPv4 record = %d bytes, want 57", n)
	}
	if n := sum(OPNsenseV9FieldsV6); n != 93 {
		t.Errorf("ng_netflow IPv6 record = %d bytes, want 93", n)
	}
	// Data sets are padded to 4 bytes; one IPv4 record: 4 + 57 -> 64.
	pkt := OPNsenseV9Data(1, 2, 3, V9Flow{Src: "10.0.0.1", Dst: "10.0.0.2"})
	if l := binary.BigEndian.Uint16(pkt[22:24]); l != 64 || len(pkt) != 20+64 {
		t.Errorf("v9 data flowset length %d, packet %d", l, len(pkt))
	}
	msg := RouterOSIPFIXData(1, 2, 3, IPFIXFlow{Src: "10.0.0.1", Dst: "10.0.0.2", SysInitMs: 1})
	if l := binary.BigEndian.Uint16(msg[2:4]); int(l) != len(msg) {
		t.Errorf("IPFIX length %d != %d", l, len(msg))
	}
	if want := 16 + 4 + sum(RouterOSIPFIXFieldsV4(true)); len(msg) != want {
		t.Errorf("IPFIX v4 data message = %d bytes, want %d", len(msg), want)
	}
	msg = RouterOSIPFIXData(1, 2, 3, IPFIXFlow{Src: "2001:db8::1", Dst: "2001:db8::2"})
	if want := 16 + 4 + sum(RouterOSIPFIXFieldsV6(false)); len(msg) != want {
		t.Errorf("IPFIX v6 data message (no IE 160) = %d bytes, want %d", len(msg), want)
	}
}
