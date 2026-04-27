package poller

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/kea"
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

func (cdp *ClientDiscoveryPoller) Run(ctx context.Context) {
	ticker := time.NewTicker(cdp.interval)
	defer ticker.Stop()

	// Run immediately on startup (short delay for device connections to establish)
	time.Sleep(10 * time.Second)
	cdp.safePoll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
