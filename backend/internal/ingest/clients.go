package ingest

import (
	"fmt"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
)

// ApplyClientSeen stores a client the client discovery found
// (edgeproto.KindClientSeen): its row in the MAC lookup cache
// (queries.UpsertMACLookup — a field left empty keeps the stored value, an
// IPv6 address never replaces an IPv4 one) and one client_history row, both
// stamped CURRENT_TIMESTAMP. The name both rows show is the DHCP host name,
// else the reverse-DNS name.
func ApplyClientSeen(tx queries.DBTX, o edgeproto.Observation) error {
	c, ok := o.Body.(edgeproto.ClientSeen)
	if !ok {
		return fmt.Errorf("ingest: %s %q: body is %T, want edgeproto.ClientSeen", o.Kind, o.Subject, o.Body)
	}
	if o.Subject == "" {
		return fmt.Errorf("ingest: %s without a MAC address", o.Kind)
	}
	hostName := c.HostName
	if hostName == "" {
		hostName = c.DNSName
	}
	if err := queries.UpsertMACLookup(tx, &queries.MACLookup{
		MACAddress: o.Subject,
		IPAddress:  c.IP,
		HostName:   hostName,
		DNSName:    c.DNSName,
		Source:     c.Source,
		DeviceName: c.DeviceName,
	}); err != nil {
		return fmt.Errorf("ingest: mac_lookup %s: %w", o.Subject, err)
	}
	if err := queries.InsertClientHistory(tx, &queries.ClientHistoryEntry{
		MACAddress: o.Subject,
		IPAddress:  c.IP,
		HostName:   hostName,
		Source:     c.Source,
		DeviceName: c.DeviceName,
	}); err != nil {
		return fmt.Errorf("ingest: client_history %s: %w", o.Subject, err)
	}
	return nil
}
