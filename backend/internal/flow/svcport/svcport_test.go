package svcport

import "testing"

func TestNamesExact(t *testing.T) {
	if len(Names) != 41 {
		t.Fatalf("len(Names) = %d, want 41", len(Names))
	}
	for port, want := range map[uint16]string{20: "FTP-data", 68: "DHCP", 162: "SNMP trap", 587: "SMTP submission",
		853: "DNS over TLS", 5247: "CAPWAP data", 8729: "RouterOS API-SSL", 51820: "WireGuard"} {
		if Names[port] != want {
			t.Errorf("Names[%d] = %q, want %q", port, Names[port], want)
		}
	}
}

func TestSelect(t *testing.T) {
	cases := []struct {
		proto        uint8
		sport, dport uint16
		want         uint16
	}{
		{6, 50514, 443, 443},      // only dport well-known
		{6, 443, 50514, 443},      // only sport well-known (return traffic)
		{17, 53, 123, 53},         // both well-known: lower
		{17, 5353, 5353, 5353},    // both, equal
		{6, 40000, 50000, 40000},  // neither: lower
		{6, 0, 50000, 50000},      // lower non-zero
		{17, 61000, 0, 61000},     // lower non-zero
		{6, 0, 0, 0},              // both zero
		{132, 38412, 2905, 2905},  // SCTP, neither known: lower
		{1, 8, 0, 0},              // ICMP: never a port
		{58, 128, 0, 0},           // ICMPv6
		{47, 1, 2, 0},             // GRE
		{17, 51820, 51821, 51820}, // WireGuard
	}
	for _, c := range cases {
		if got := Select(c.proto, c.sport, c.dport); got != c.want {
			t.Errorf("Select(%d, %d, %d) = %d, want %d", c.proto, c.sport, c.dport, got, c.want)
		}
	}
}

func TestLabel(t *testing.T) {
	cases := []struct {
		proto     uint8
		port      uint16
		label, pn string
	}{
		{6, 443, "HTTPS", "tcp"},
		{17, 443, "QUIC", "udp"},
		{17, 853, "DNS over QUIC", "udp"},
		{6, 853, "DNS over TLS", "tcp"},
		{17, 53, "DNS", "udp"},
		{6, 0, "", "tcp"},
		{6, 50514, "", "tcp"},
		{132, 22, "SSH", "sctp"},
		{1, 0, "", "icmp"},
		{58, 0, "", "icmpv6"},
		{47, 0, "", "gre"},
		{50, 0, "", "esp"},
		{51, 0, "", "ah"},
		{89, 0, "", "ospf"},
		{112, 0, "", "vrrp"},
		{41, 0, "", "ip-41"},
		{0, 0, "", "ip-0"},
	}
	for _, c := range cases {
		label, pn := Label(c.proto, c.port)
		if label != c.label || pn != c.pn {
			t.Errorf("Label(%d, %d) = %q, %q; want %q, %q", c.proto, c.port, label, pn, c.label, c.pn)
		}
	}
}
