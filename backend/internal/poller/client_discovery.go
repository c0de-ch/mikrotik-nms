package poller

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/kea"
	"github.com/mikrotik-nms/backend/internal/opnsense"
	"github.com/mikrotik-nms/backend/internal/resolver"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// ClientDiscoveryPoller periodically scans all devices for ARP/DHCP data,
// updates the MAC lookup cache, and saves snapshots to client_history.
type ClientDiscoveryPoller struct {
	db       *sql.DB
	pool     *routeros.Pool
	resolver *resolver.Resolver
	interval time.Duration
}

func NewClientDiscoveryPoller(db *sql.DB, pool *routeros.Pool, res *resolver.Resolver, interval time.Duration) *ClientDiscoveryPoller {
	return &ClientDiscoveryPoller{db: db, pool: pool, resolver: res, interval: interval}
}

// currentInterval reads the runtime-tunable client_discovery_interval (seconds)
// from app_settings, falling back to the constructed interval. Re-read each
// cycle so a Settings change applies without a backend restart.
func (cdp *ClientDiscoveryPoller) currentInterval() time.Duration {
	if v, err := queries.GetSetting(cdp.db, "client_discovery_interval"); err == nil {
		if n, e := strconv.Atoi(strings.TrimSpace(v)); e == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return cdp.interval
}

func (cdp *ClientDiscoveryPoller) Run(ctx context.Context) {
	// Run shortly after startup (short delay for device connections to establish)
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}
	cdp.safePoll(ctx)

	for {
		timer := time.NewTimer(cdp.currentInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			cdp.safePoll(ctx)
		}
	}
}

func (cdp *ClientDiscoveryPoller) safePoll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("client discovery: panic: %v", r)
		}
	}()
	cdp.poll(ctx)
}

func (cdp *ClientDiscoveryPoller) poll(ctx context.Context) {
	devices, err := queries.ListDevices(cdp.db)
	if err != nil {
		return
	}

	type clientEntry struct {
		mac, ip, hostname, source, deviceName string
	}
	clientMap := make(map[string]*clientEntry)

	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if dev.Status != "online" {
			continue
		}
		client := cdp.pool.Get(dev.ID)
		if client == nil {
			continue
		}
		devName := dev.Identity
		if devName == "" {
			devName = dev.Address
		}

		// ARP
		func() {
			defer func() { recover() }()
			arps, err := routeros.GetARPTable(client)
			if err != nil {
				return
			}
			for _, a := range arps {
				if a.MAC == "" {
					continue
				}
				mac := strings.ToUpper(a.MAC)
				if _, ok := clientMap[mac]; !ok {
					clientMap[mac] = &clientEntry{mac: mac, ip: a.Address, source: "arp", deviceName: devName}
				} else if clientMap[mac].ip == "" {
					clientMap[mac].ip = a.Address
				}
			}
		}()

		// DHCP
		func() {
			defer func() { recover() }()
			leases, err := routeros.GetDHCPLeases(client)
			if err != nil {
				return
			}
			for _, l := range leases {
				mac := strings.ToUpper(l.MAC)
				if mac == "" {
					mac = strings.ToUpper(l.ActiveMAC)
				}
				if mac == "" {
					continue
				}
				addr := l.ActiveAddr
				if addr == "" {
					addr = l.Address
				}
				if existing, ok := clientMap[mac]; ok {
					if existing.hostname == "" && l.HostName != "" {
						existing.hostname = l.HostName
					}
					if existing.ip == "" && addr != "" {
						existing.ip = addr
					}
				} else {
					clientMap[mac] = &clientEntry{mac: mac, ip: addr, hostname: l.HostName, source: "dhcp", deviceName: devName}
				}
			}
		}()
	}

	// Kea DHCP: pull active leases if configured
	if keaURL, err := queries.GetSetting(cdp.db, "kea_url"); err == nil && keaURL != "" {
		func() {
			defer func() { recover() }()
			leases, err := kea.New(keaURL).GetLeases4()
			if err != nil {
				log.Printf("client discovery: kea: %v", err)
				return
			}
			for _, l := range leases {
				mac := strings.ToUpper(l.HWAddress)
				if mac == "" {
					continue
				}
				if existing, ok := clientMap[mac]; ok {
					if existing.ip == "" && l.IPAddress != "" {
						existing.ip = l.IPAddress
					}
					if existing.hostname == "" && l.Hostname != "" {
						existing.hostname = l.Hostname
					}
				} else {
					clientMap[mac] = &clientEntry{mac: mac, ip: l.IPAddress, hostname: l.Hostname, source: "dhcp", deviceName: "kea"}
				}
			}
			log.Printf("client discovery: kea: %d active leases", len(leases))
		}()
	}

	// OPNsense Kea via the OPNsense REST API. Lets us pick up DHCP leases
	// for subnets where the MikroTik routers don't see the clients in ARP.
	opURL, _ := queries.GetSetting(cdp.db, "opnsense_url")
	opKey, _ := queries.GetSetting(cdp.db, "opnsense_api_key")
	opSecret, _ := queries.GetSetting(cdp.db, "opnsense_api_secret")
	if opURL != "" && opKey != "" && opSecret != "" {
		opVerify, _ := queries.GetSetting(cdp.db, "opnsense_verify_tls")
		client := opnsense.New(opnsense.Config{
			URL: opURL, APIKey: opKey, APISecret: opSecret,
			VerifyTLS: opVerify == "true" || opVerify == "1",
		})
		func() {
			defer func() { recover() }()
			leases, err := client.GetLeases()
			if err != nil {
				log.Printf("client discovery: opnsense: %v", err)
			}
			for _, l := range leases {
				mac := strings.ToUpper(l.HWAddress)
				if mac == "" {
					continue
				}
				if existing, ok := clientMap[mac]; ok {
					if existing.ip == "" && l.IPAddress != "" {
						existing.ip = l.IPAddress
					}
					if existing.hostname == "" && l.Hostname != "" {
						existing.hostname = l.Hostname
					}
				} else {
					clientMap[mac] = &clientEntry{mac: mac, ip: l.IPAddress, hostname: l.Hostname, source: "dhcp", deviceName: "opnsense"}
				}
			}
			log.Printf("client discovery: opnsense: %d active leases", len(leases))
		}()
	}

	// Resolve DNS for IPs
	ips := make([]string, 0)
	for _, c := range clientMap {
		if c.ip != "" {
			ips = append(ips, c.ip)
		}
	}
	dnsNames := cdp.resolver.ResolveMany(ips)

	// Update MAC lookup cache and save history
	for _, c := range clientMap {
		dns := dnsNames[c.ip]
		hostname := c.hostname
		if hostname == "" {
			hostname = dns
		}

		_ = queries.UpsertMACLookup(cdp.db, &queries.MACLookup{
			MACAddress: c.mac,
			IPAddress:  c.ip,
			HostName:   hostname,
			DNSName:    dns,
			Source:     c.source,
			DeviceName: c.deviceName,
		})

		_ = queries.InsertClientHistory(cdp.db, &queries.ClientHistoryEntry{
			MACAddress: c.mac,
			IPAddress:  c.ip,
			HostName:   hostname,
			Source:     c.source,
			DeviceName: c.deviceName,
		})
	}

	log.Printf("client discovery: updated %d MAC entries", len(clientMap))
}
