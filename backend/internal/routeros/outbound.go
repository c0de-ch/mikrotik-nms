package routeros

import (
	"net"
	"net/netip"
)

// OutboundAddress returns the local address this host uses to reach
// host:port, without sending anything: "connecting" a UDP socket only
// consults the routing table. It is how the NMS learns which source address
// a device sees its API sessions arrive from (barring NAT in between), so the
// login watch can tell the NMS's own sessions from someone else's.
func OutboundAddress(host string, port int) (netip.Addr, bool) {
	conn, err := net.Dial("udp", JoinHostPort(host, port))
	if err != nil {
		return netip.Addr{}, false
	}
	defer conn.Close()
	ap, err := netip.ParseAddrPort(conn.LocalAddr().String())
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}
