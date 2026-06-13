// Package leasesource fetches DHCP leases from the external DHCP servers the NMS
// can be pointed at (one or more Kea Control Agents and/or OPNsense Kea
// wrappers), keyed by app_settings. It is the single place both the background
// client-discovery poller and the on-demand client scan use to enrich MAC ->
// IP/hostname with leases the MikroTik routers themselves don't serve, so the
// two stay in sync.
package leasesource

import (
	"database/sql"
	"log"
	"strings"
	"sync"

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

// opnsenseSuffixes are the app_settings key suffixes for OPNsense instances:
// "" is the primary (opnsense_url, …), "2" the secondary (opnsense2_url, …) for
// a remote site whose subnet the primary doesn't serve. Add more here (and to
// the settings whitelist + UI) if further sites are needed.
var opnsenseSuffixes = []string{"", "2"}

// FromSettings returns the union of active IPv4/IPv6 leases from whichever
// external DHCP sources are configured in app_settings (kea_url, opnsense*_*).
// All sources are queried concurrently and the call is best-effort: an
// unconfigured source is skipped, and a source that errors (or is slow — each
// client carries its own HTTP timeout) is logged and skipped without affecting
// the others, so one bad URL can't stall the whole scan. Leases with an empty
// MAC are dropped (they can't be matched to a client).
func FromSettings(db *sql.DB) []Lease {
	// Build the list of source fetchers first (cheap, DB-only), then run them
	// all concurrently so total wall time is bounded by the slowest single
	// source, not the sum.
	type source struct {
		name string
		fn   func() ([]Lease, error)
	}
	var fetchers []source

	// Kea Control Agent(s). The kea_url setting may list more than one agent —
	// one per line, or comma-separated.
	if keaURLs, err := queries.GetSetting(db, "kea_url"); err == nil {
		for _, keaURL := range splitURLs(keaURLs) {
			keaURL := keaURL
			fetchers = append(fetchers, source{"kea " + keaURL, func() ([]Lease, error) {
				leases, err := kea.New(keaURL).GetLeases4()
				if err != nil {
					return nil, err
				}
				out := make([]Lease, 0, len(leases))
				for _, l := range leases {
					if mac := strings.ToUpper(l.HWAddress); mac != "" {
						out = append(out, Lease{MAC: mac, IP: l.IPAddress, Hostname: l.Hostname, Origin: "kea"})
					}
				}
				return out, nil
			}})
		}
	}

	// OPNsense Kea wrapper(s). Primary + numbered extras (e.g. a remote site).
	for _, suffix := range opnsenseSuffixes {
		opURL, _ := queries.GetSetting(db, "opnsense"+suffix+"_url")
		opKey, _ := queries.GetSetting(db, "opnsense"+suffix+"_api_key")
		opSecret, _ := queries.GetSetting(db, "opnsense"+suffix+"_api_secret")
		if opURL == "" || opKey == "" || opSecret == "" {
			continue
		}
		opVerify, _ := queries.GetSetting(db, "opnsense"+suffix+"_verify_tls")
		cfg := opnsense.Config{
			URL: opURL, APIKey: opKey, APISecret: opSecret,
			VerifyTLS: opVerify == "true" || opVerify == "1",
		}
		origin := "opnsense" + suffix // "opnsense", "opnsense2", …
		fetchers = append(fetchers, source{origin, func() ([]Lease, error) {
			client := opnsense.New(cfg)
			if client == nil {
				return nil, nil
			}
			leases, err := client.GetLeases()
			if err != nil {
				return nil, err
			}
			out := make([]Lease, 0, len(leases))
			for _, l := range leases {
				if mac := strings.ToUpper(l.HWAddress); mac != "" {
					out = append(out, Lease{MAC: mac, IP: l.IPAddress, Hostname: l.Hostname, Origin: origin})
				}
			}
			return out, nil
		}})
	}

	var (
		mu  sync.Mutex
		out []Lease
		wg  sync.WaitGroup
	)
	for _, f := range fetchers {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { recover() }()
			leases, err := f.fn()
			if err != nil {
				log.Printf("leasesource: %s: %v", f.name, err)
				return
			}
			if len(leases) == 0 {
				return
			}
			mu.Lock()
			out = append(out, leases...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// splitURLs parses a setting that may hold several URLs separated by newlines
// or commas, trimming whitespace and dropping empties.
func splitURLs(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	}) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
