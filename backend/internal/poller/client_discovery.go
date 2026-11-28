package poller

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
	"github.com/mikrotik-nms/backend/internal/leasesource"
	"github.com/mikrotik-nms/backend/internal/resolver"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// ClientDiscoveryPoller scans the devices' ARP tables (Tier 5a) and DHCP
// leases (Tier 5b, only where the class plan runs it — C3), each on the
// device's own cadence with client_discovery_interval as the floor, plus the
// external DHCP sources (Kea / OPNsense) once per client_discovery_interval.
// What a pass finds it reports to the sink, one KindClientSeen per client:
// ingest updates the MAC lookup cache and saves it to client_history.
//
// It is the collect half of the client discovery, built on the seams: the
// devices and the settings come from Config, every device read goes through
// Exec, and nothing is written but through Sink. One database read is left:
// the resolver reads the dns_servers table itself on an uncached lookup.
type ClientDiscoveryPoller struct {
	config   edgeproto.ConfigSource
	exec     routeros.DeviceExec
	sink     edgeproto.Sink
	leases   func() []leasesource.Lease
	resolver *resolver.Resolver
	interval time.Duration

	// clock schedules the device reads, now is the loop's time (both test
	// seams). The external lease sources run on lastExternal + the floor;
	// their latest leases enrich the device reads in between.
	clock        *tierClock
	now          func() time.Time
	lastExternal time.Time
	external     []leasesource.Lease
}

// NewClientDiscoveryPoller builds the client discovery on s. leases fetches
// the external lease servers' leases (nil: there are none); res resolves
// the clients' reverse-DNS names (nil: none); interval is the floor and the
// external sources' period while client_discovery_interval is not set.
func NewClientDiscoveryPoller(s Seams, leases func() []leasesource.Lease, res *resolver.Resolver, interval time.Duration) *ClientDiscoveryPoller {
	if interval <= 0 {
		interval = defaultClientDiscovery
	}
	if leases == nil {
		leases = func() []leasesource.Lease { return nil }
	}
	return &ClientDiscoveryPoller{
		config: s.Config, exec: s.Exec, sink: s.Sink, leases: leases,
		resolver: res, interval: interval,
		clock: newTierClock(Schedule()),
		now:   time.Now,
	}
}

// externalInterval is the external lease sources' period: the runtime-tunable
// client_discovery_interval (seconds), falling back to the constructed
// interval, re-read each tick so a Settings change applies without a restart.
func (cdp *ClientDiscoveryPoller) externalInterval() time.Duration {
	return secondsOr(cdp.config.Setting("client_discovery_interval"), cdp.interval)
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
// resolved and reported. A tick with nothing due does nothing.
func (cdp *ClientDiscoveryPoller) poll(ctx context.Context) {
	devices, err := cdp.config.Devices()
	if err != nil {
		return
	}
	now := cdp.now()
	cdp.clock.retain(specIDs(devices))

	clientMap := make(map[string]*clientEntry)
	readAny := false
	for _, dev := range devices {
		if ctx.Err() != nil {
			return
		}
		var arpIv, dhcpIv time.Duration
		if dev.Online {
			arpIv = dev.RunInterval(devclass.TierARP)
			dhcpIv = dev.RunInterval(devclass.TierDHCPLeases)
		}
		arpDue := cdp.clock.due(dev.ID, devclass.TierARP, arpIv, now)
		dhcpDue := cdp.clock.due(dev.ID, devclass.TierDHCPLeases, dhcpIv, now)
		if !arpDue && !dhcpDue {
			continue
		}
		run := cdp.exec.Device(ctx, dev.ID)
		if run == nil {
			if arpDue {
				cdp.clock.retryAt(dev.ID, devclass.TierARP, now.Add(noClientRetry))
			}
			if dhcpDue {
				cdp.clock.retryAt(dev.ID, devclass.TierDHCPLeases, now.Add(noClientRetry))
			}
			continue
		}
		readAny = true
		if arpDue {
			cdp.readARP(run, dev.Name, clientMap)
			cdp.clock.ran(dev.ID, devclass.TierARP, arpIv, cdp.now())
		}
		if dhcpDue {
			cdp.readDHCP(run, dev.Name, clientMap)
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
		cdp.external = cdp.leases()
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
	cdp.store(ctx, now, clientMap)
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

// store resolves the clients' DNS names and reports every client the pass
// found, seen at now, in one Emit: the sink stores the pass whole or not at
// all.
func (cdp *ClientDiscoveryPoller) store(ctx context.Context, now time.Time, clientMap map[string]*clientEntry) {
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

	obs := make([]edgeproto.Observation, 0, len(clientMap))
	for _, c := range clientMap {
		obs = append(obs, edgeproto.Observation{
			Kind:       edgeproto.KindClientSeen,
			Subject:    c.mac,
			ObservedAt: now,
			Body: edgeproto.ClientSeen{
				IP:         c.ip,
				HostName:   c.hostname,
				DNSName:    dnsNames[c.ip],
				Source:     c.source,
				DeviceName: c.deviceName,
			},
		})
	}
	if err := cdp.sink.Emit(ctx, nil, obs...); err != nil {
		log.Printf("client discovery: store %d MAC entries: %v", len(obs), err)
		return
	}
	log.Printf("client discovery: updated %d MAC entries", len(clientMap))
}
