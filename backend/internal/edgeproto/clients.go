package edgeproto

// KindClientSeen is a client the scheduled client discovery found in a pass:
// the devices' ARP tables and DHCP leases and the external lease servers,
// merged by MAC. Subject is the MAC address in upper case; Body is a
// ClientSeen. Applied by ingest.ApplyClientSeen (the MAC lookup cache and
// client_history).
const KindClientSeen Kind = "client.seen"

// ClientSeen is the body of KindClientSeen.
type ClientSeen struct {
	// IP is the address the pass prefers for the client: IPv4 over IPv6, the
	// first one seen within a family.
	IP string
	// HostName is the name a DHCP lease gives it, "" when none does; DNSName
	// the reverse-DNS name of IP, "" when there is none.
	HostName string
	DNSName  string
	// Source is the read that found it first: "arp" or "dhcp" (a device's
	// lease table or an external lease server).
	Source string
	// DeviceName is where it was found: the device's identity (else its
	// address), or the external lease source ("kea", "opnsense2", …).
	DeviceName string
}
