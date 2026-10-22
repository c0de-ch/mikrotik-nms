package poller

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/leasesource"
	"github.com/mikrotik-nms/backend/internal/resolver"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// ClientDiscoveryPoller scans the devices' ARP tables (Tier 5a) and DHCP
// leases (Tier 5b, only where the class plan runs it — C3), each on the
// device's own cadence with client_discovery_interval as the floor, plus the
// external DHCP sources (Kea / OPNsense) once per client_discovery_interval.
// What a pass finds updates the MAC lookup cache and is saved to
// client_history.
type ClientDiscoveryPoller struct {
	db       *sql.DB
	pool     *routeros.Pool
	resolver *resolver.Resolver
	interval time.Duration

	// cadence and clock schedule the device reads (UseCadence); runner is
	// the device link (nil = no pooled connection) and now the clock (test
	// seams). The external lease sources run on lastExternal + the floor;
	// their latest leases enrich the device reads in between.
	cadence      CadenceSource
	clock        *tierClock
	runner       func(deviceID string) routeros.Runner
	now          func() time.Time
	lastExternal time.Time
	external     []leasesource.Lease
}

func NewClientDiscoveryPoller(db *sql.DB, pool *routeros.Pool, res *resolver.Resolver, interval time.Duration) *ClientDiscoveryPoller {
	if interval <= 0 {
		interval = defaultClientDiscovery
	}
	cdp := &ClientDiscoveryPoller{
		db: db, pool: pool, resolver: res, interval: interval,
		cadence: newFleetCadence(db),
		clock:   newTierClock(Schedule()),
		now:     time.Now,
	}
	cdp.runner = func(deviceID string) routeros.Runner { return poolRunner(cdp.pool, deviceID) }
	return cdp
}

// UseCadence schedules the device reads by cs. Call before Run.
func (cdp *ClientDiscoveryPoller) UseCadence(cs CadenceSource) {
	if cs != nil {
		cdp.cadence = cs
	}
}

// externalInterval is the external lease sources' period: the runtime-tunable
// client_discovery_interval (seconds), falling back to the constructed
// interval, re-read each tick so a Settings change applies without a restart.
func (cdp *ClientDiscoveryPoller) externalInterval() time.Duration {
	return settingSeconds(cdp.db, "client_discovery_interval", cdp.interval)
}

func (cdp *ClientDiscoveryPoller) Run(ctx context.Context) {
	// Run shortly after startup (short delay for device connections to establish)
	if !sleepCtx(ctx, 10*time.Second) {
		return
	}
	for {
		cdp.safePoll(ctx)
		if !sleepCtx(ctx, pollerTick) {
			return
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

// clientEntry is one client a pass found.
type clientEntry struct {
	mac, ip, hostname, source, deviceName string
}

// poll is one tick: the ARP and DHCP reads that are due, and the external
// lease sources when their interval is up; then the clients found are
// resolved and stored. A tick with nothing due does nothing.
func (cdp *ClientDiscoveryPoller) poll(ctx context.Context) {
	devices, err := queries.ListDevices(cdp.db)
	if err != nil {
		return
	}
	now := cdp.now()
	cdp.clock.retain(deviceIDs(devices))

	clientMap := make(map[string]*clientEntry)
	readAny := false
	for _, dev := range devices {
		if ctx.Err() != nil {
			return
		}
		var arpIv, dhcpIv time.Duration
		if dev.Status == "online" {
			arpIv = runInterval(cdp.cadence, dev, devclass.TierARP, now)
			dhcpIv = runInterval(cdp.cadence, dev, devclass.TierDHCPLeases, now)
		}
		arpDue := cdp.clock.due(dev.ID, devclass.TierARP, arpIv, now)
		dhcpDue := cdp.clock.due(dev.ID, devclass.TierDHCPLeases, dhcpIv, now)
		if !arpDue && !dhcpDue {
			continue
		}
		run := cdp.runner(dev.ID)
		if run == nil {
			if arpDue {
				cdp.clock.retryAt(dev.ID, devclass.TierARP, now.Add(noClientRetry))
			}
			if dhcpDue {
				cdp.clock.retryAt(dev.ID, devclass.TierDHCPLeases, now.Add(noClientRetry))
			}
			continue
		}
		devName := nonempty(dev.Identity, dev.Address)
		readAny = true
		if arpDue {
			cdp.readARP(run, devName, clientMap)
			cdp.clock.ran(dev.ID, devclass.TierARP, arpIv, cdp.now())
		}
		if dhcpDue {
			cdp.readDHCP(run, devName, clientMap)
			cdp.clock.ran(dev.ID, devclass.TierDHCPLeases, dhcpIv, cdp.now())
		}
	}

	// External DHCP (Kea Control Agent and/or OPNsense's Kea wrapper, per
	// app_settings). Picks up leases — and their DHCP hostnames — for subnets
	// the MikroTik routers don't see in their own ARP/DHCP tables. Fetched
	// once per interval; in between, the latest leases only enrich the
	// clients a device read found.
	externalDue := cdp.lastExternal.IsZero() || now.Sub(cdp.lastExternal) >= cdp.externalInterval()
	if externalDue {
		cdp.external = leasesource.FromSettings(cdp.db)
		cdp.lastExternal = now
		if len(cdp.external) > 0 {
			log.Printf("client discovery: external dhcp: %d leases", len(cdp.external))
		}
	}
	if !readAny && !externalDue {
		return
	}
	for _, l := range cdp.external {
		if existing, ok := clientMap[l.MAC]; ok {
			existing.ip = queries.PreferIP(existing.ip, l.IP)
			if existing.hostname == "" && l.Hostname != "" {
				existing.hostname = l.Hostname
			}
		} else if externalDue {
			clientMap[l.MAC] = &clientEntry{mac: l.MAC, ip: l.IP, hostname: l.Hostname, source: "dhcp", deviceName: l.Origin}
		}
	}
	if len(clientMap) == 0 {
		return
	}
	cdp.store(clientMap)
}

// readARP adds one device's ARP table to clientMap.
func (cdp *ClientDiscoveryPoller) readARP(run routeros.Runner, devName string, clientMap map[string]*clientEntry) {
	defer func() { recover() }()
	arps, err := routeros.GetARPTable(run)
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
		} else {
			clientMap[mac].ip = queries.PreferIP(clientMap[mac].ip, a.Address)
		}
	}
}

// readDHCP adds one device's DHCP leases to clientMap.
func (cdp *ClientDiscoveryPoller) readDHCP(run routeros.Runner, devName string, clientMap map[string]*clientEntry) {
	defer func() { recover() }()
	leases, err := routeros.GetDHCPLeases(run)
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
			existing.ip = queries.PreferIP(existing.ip, addr)
		} else {
			clientMap[mac] = &clientEntry{mac: mac, ip: addr, hostname: l.HostName, source: "dhcp", deviceName: devName}
		}
	}
}

// store resolves the clients' DNS names and writes them to the MAC lookup
// cache and client_history.
func (cdp *ClientDiscoveryPoller) store(clientMap map[string]*clientEntry) {
	ips := make([]string, 0, len(clientMap))
	for _, c := range clientMap {
		if c.ip != "" {
			ips = append(ips, c.ip)
		}
	}
	var dnsNames map[string]string
	if cdp.resolver != nil {
		dnsNames = cdp.resolver.ResolveMany(ips)
	}

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
