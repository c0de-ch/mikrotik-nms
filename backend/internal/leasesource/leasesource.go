// Package leasesource fetches DHCP leases from the external DHCP servers the NMS
// can be pointed at (a Kea Control Agent and/or an OPNsense Kea wrapper), keyed
// by app_settings. It is the single place both the background client-discovery
// poller and the on-demand client scan use to enrich MAC -> IP/hostname with
// leases the MikroTik routers themselves don't serve, so the two stay in sync.
package leasesource

import (
	"database/sql"
	"log"
	"strings"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/kea"
	"github.com/mikrotik-nms/backend/internal/opnsense"
)

// Lease is a normalized active DHCP lease from an external source. MAC is
// upper-cased; Origin is a short source tag ("kea" / "opnsense") used as the
// reported device name.
type Lease struct {
	MAC      string
	IP       string
	Hostname string
	Origin   string
}

// FromSettings returns the union of active IPv4/IPv6 leases from whichever
// external DHCP sources are configured in app_settings (kea_url, opnsense_*).
// It is best-effort: an unconfigured source is skipped, and a source that errors
// is logged and skipped without affecting the other. Leases with an empty MAC
// are dropped (they can't be matched to a client).
func FromSettings(db *sql.DB) []Lease {
	var out []Lease

	// Kea Control Agent (direct REST API).
	if keaURL, err := queries.GetSetting(db, "kea_url"); err == nil && keaURL != "" {
		if leases, err := kea.New(keaURL).GetLeases4(); err != nil {
			log.Printf("leasesource: kea: %v", err)
		} else {
			for _, l := range leases {
				if mac := strings.ToUpper(l.HWAddress); mac != "" {
					out = append(out, Lease{MAC: mac, IP: l.IPAddress, Hostname: l.Hostname, Origin: "kea"})
				}
			}
		}
	}

	// OPNsense Kea wrapper (OPNsense REST API). Picks up leases for subnets the
	// MikroTik routers don't see in their own ARP/DHCP tables.
	opURL, _ := queries.GetSetting(db, "opnsense_url")
	opKey, _ := queries.GetSetting(db, "opnsense_api_key")
	opSecret, _ := queries.GetSetting(db, "opnsense_api_secret")
	if opURL != "" && opKey != "" && opSecret != "" {
		opVerify, _ := queries.GetSetting(db, "opnsense_verify_tls")
		client := opnsense.New(opnsense.Config{
			URL: opURL, APIKey: opKey, APISecret: opSecret,
			VerifyTLS: opVerify == "true" || opVerify == "1",
		})
		if client != nil {
			if leases, err := client.GetLeases(); err != nil {
				log.Printf("leasesource: opnsense: %v", err)
			} else {
				for _, l := range leases {
					if mac := strings.ToUpper(l.HWAddress); mac != "" {
						out = append(out, Lease{MAC: mac, IP: l.IPAddress, Hostname: l.Hostname, Origin: "opnsense"})
					}
				}
			}
		}
	}

	return out
}
