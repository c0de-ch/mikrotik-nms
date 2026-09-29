// Package svcport holds the well-known service ports of the flow collector:
// which of a flow's two ports is its "service" (the aggregation key keeps
// that one, so ephemeral client ports never explode the row count) and the
// app label shown for a (protocol, port). It is a leaf package shared by the
// collector (ingest) and the API (labels).
package svcport

import "strconv"

// Names maps the well-known service ports to their app label.
var Names = map[uint16]string{
	20:    "FTP-data",
	21:    "FTP",
	22:    "SSH",
	23:    "Telnet",
	25:    "SMTP",
	53:    "DNS",
	67:    "DHCP",
	68:    "DHCP",
	80:    "HTTP",
	110:   "POP3",
	123:   "NTP",
	143:   "IMAP",
	161:   "SNMP",
	162:   "SNMP trap",
	389:   "LDAP",
	443:   "HTTPS",
	445:   "SMB",
	465:   "SMTPS",
	514:   "Syslog",
	587:   "SMTP submission",
	636:   "LDAPS",
	853:   "DNS over TLS",
	993:   "IMAPS",
	995:   "POP3S",
	1194:  "OpenVPN",
	1883:  "MQTT",
	2055:  "NetFlow",
	3389:  "RDP",
	4739:  "IPFIX",
	5060:  "SIP",
	5061:  "SIP-TLS",
	5246:  "CAPWAP",
	5247:  "CAPWAP data",
	5353:  "mDNS",
	6343:  "sFlow",
	8080:  "HTTP-alt",
	8291:  "WinBox",
	8443:  "HTTPS-alt",
	8728:  "RouterOS API",
	8729:  "RouterOS API-SSL",
	51820: "WireGuard",
}

// hasPorts reports whether the protocol carries L4 ports (TCP, UDP, SCTP).
func hasPorts(proto uint8) bool {
	return proto == 6 || proto == 17 || proto == 132
}

// Select returns the service port of a flow: for TCP(6)/UDP(17)/SCTP(132), the
// port in Names if exactly one of sport/dport is; the lower of the two if both
// are; otherwise the lower non-zero port (0 when both are 0). Other protocols: 0.
func Select(proto uint8, sport, dport uint16) uint16 {
	if !hasPorts(proto) {
		return 0
	}
	_, sk := Names[sport]
	_, dk := Names[dport]
	switch {
	case sk && !dk:
		return sport
	case dk && !sk:
		return dport
	case sk && dk:
		return min(sport, dport)
	case sport == 0:
		return dport
	case dport == 0:
		return sport
	}
	return min(sport, dport)
}

// protoNames are the protocol names Label knows; the rest render "ip-<n>".
var protoNames = map[uint8]string{
	1:   "icmp",
	6:   "tcp",
	17:  "udp",
	47:  "gre",
	50:  "esp",
	51:  "ah",
	58:  "icmpv6",
	89:  "ospf",
	112: "vrrp",
	132: "sctp",
}

// Label returns the app label ("" when unknown) and the protocol name for
// (proto, port): tcp, udp, sctp, icmp (1), icmpv6 (58), gre (47), esp (50),
// ah (51), ospf (89), vrrp (112), else "ip-<n>". Special cases: 443/udp ->
// "QUIC", 853/udp -> "DNS over QUIC", port 0 -> label "".
func Label(proto uint8, port uint16) (label, protoName string) {
	protoName, ok := protoNames[proto]
	if !ok {
		protoName = "ip-" + strconv.Itoa(int(proto))
	}
	if port == 0 || !hasPorts(proto) {
		return "", protoName
	}
	if proto == 17 {
		switch port {
		case 443:
			return "QUIC", protoName
		case 853:
			return "DNS over QUIC", protoName
		}
	}
	return Names[port], protoName
}
