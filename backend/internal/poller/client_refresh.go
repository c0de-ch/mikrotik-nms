package poller

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/leasesource"
	"github.com/mikrotik-nms/backend/internal/resolver"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// ClientRefreshMinInterval is the least time between two on-demand client
// refreshes, fleet-wide: the Clients page shows the cache the scheduled
// pollers keep, and a user may pull it forward at most this often.
const ClientRefreshMinInterval = 5 * time.Minute

// clientRefreshTimeout bounds one refresh: devices not reached by then keep
// their cached rows.
const clientRefreshTimeout = 90 * time.Second

var (
	// ErrClientRefreshRunning: a refresh is under way; its result is the one
	// to wait for.
	ErrClientRefreshRunning = errors.New("a client refresh is already running")
	// ErrClientRefreshTooSoon: the last refresh started less than
	// ClientRefreshMinInterval ago.
	ErrClientRefreshTooSoon = errors.New("the client list was refreshed less than 5 minutes ago")
)

// ClientRefreshStatus is the state of the on-demand client refresh.
type ClientRefreshStatus struct {
	Running bool `json:"running"`
	// StartedAt / FinishedAt are the last refresh's (FinishedAt is absent
	// while it runs, both before the first one).
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// NextAllowedAt is set while a new refresh would be refused as too soon.
	NextAllowedAt      *time.Time `json:"next_allowed_at,omitempty"`
	MinIntervalSeconds int        `json:"min_interval_seconds"`
	// What the last finished refresh did.
	Clients        int  `json:"clients"`
	DevicesRead    int  `json:"devices_read"`
	DevicesSkipped int  `json:"devices_skipped"`
	TimedOut       bool `json:"timed_out,omitempty"`
}

// clientRefreshResult is what one pass did.
type clientRefreshResult struct {
	clients, read, skipped int
	timedOut               bool
}

// ClientRefresher runs the on-demand refresh of the client cache
// (mac_lookup): the reads the scheduled pollers make anyway — ARP, DHCP
// leases, wifi registration tables, the external lease sources — pulled
// forward once, for every device at the same time. It reads a device only
// as its plan allows: nothing while it is paused or offline, DHCP leases
// only where the class plan has them (C3), and registration tables only
// from the menus the capability probe found, so a switch is never asked for
// a table it does not have. One refresh runs at a time, and at most one
// starts per ClientRefreshMinInterval.
type ClientRefresher struct {
	db       *sql.DB
	resolver *resolver.Resolver
	caps     *CapabilityCache
	cadence  CadenceSource

	// Test seams: runner is the device link (nil = no pooled connection),
	// now the clock, external the external lease sources.
	runner   func(deviceID string) routeros.Runner
	now      func() time.Time
	external func() []leasesource.Lease

	minInterval time.Duration
	timeout     time.Duration

	mu        sync.Mutex
	running   bool
	lastStart time.Time
	lastEnd   time.Time
	last      clientRefreshResult
	jobs      sync.WaitGroup
}

// NewClientRefresher returns a refresher reading through pool. caps is the
// pollers' shared capability cache; cadence their schedule (nil = the
// fleet-wide one built from the database).
func NewClientRefresher(db *sql.DB, pool *routeros.Pool, res *resolver.Resolver, caps *CapabilityCache, cadence CadenceSource) *ClientRefresher {
	if caps == nil {
		caps = NewCapabilityCache()
	}
	if cadence == nil {
		cadence = newFleetCadence(db)
	}
	cr := &ClientRefresher{
		db: db, resolver: res, caps: caps, cadence: cadence,
		now:         time.Now,
		minInterval: ClientRefreshMinInterval,
		timeout:     clientRefreshTimeout,
	}
	cr.runner = func(deviceID string) routeros.Runner { return poolRunner(pool, deviceID) }
	cr.external = func() []leasesource.Lease { return leasesource.FromSettings(db) }
	return cr
}

// Status reports the refresh state as of now.
func (cr *ClientRefresher) Status() ClientRefreshStatus {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.statusLocked(cr.now())
}

func (cr *ClientRefresher) statusLocked(now time.Time) ClientRefreshStatus {
	st := ClientRefreshStatus{
		Running:            cr.running,
		MinIntervalSeconds: int(cr.minInterval / time.Second),
		Clients:            cr.last.clients,
		DevicesRead:        cr.last.read,
		DevicesSkipped:     cr.last.skipped,
		TimedOut:           cr.last.timedOut,
	}
	if !cr.lastStart.IsZero() {
		t := cr.lastStart.UTC()
		st.StartedAt = &t
		if next := cr.lastStart.Add(cr.minInterval); now.Before(next) {
			n := next.UTC()
			st.NextAllowedAt = &n
		}
	}
	if !cr.running && !cr.lastEnd.IsZero() {
		t := cr.lastEnd.UTC()
		st.FinishedAt = &t
	}
	return st
}

// Start begins a refresh in the background and returns at once. It is
// refused with routeros.ErrPollersOff on an instance that polls nothing,
// ErrClientRefreshRunning while one runs and ErrClientRefreshTooSoon inside
// the minimum interval; the status says when the next one may start.
func (cr *ClientRefresher) Start() (ClientRefreshStatus, error) {
	if err := routeros.PollingAllowed(); err != nil {
		return cr.Status(), err
	}
	cr.mu.Lock()
	now := cr.now()
	switch {
	case cr.running:
		st := cr.statusLocked(now)
		cr.mu.Unlock()
		return st, ErrClientRefreshRunning
	case !cr.lastStart.IsZero() && now.Before(cr.lastStart.Add(cr.minInterval)):
		st := cr.statusLocked(now)
		cr.mu.Unlock()
		return st, ErrClientRefreshTooSoon
	}
	cr.running = true
	cr.lastStart = now
	st := cr.statusLocked(now)
	cr.jobs.Add(1)
	cr.mu.Unlock()

	go func() {
		defer cr.jobs.Done()
		ctx, cancel := context.WithTimeout(context.Background(), cr.timeout)
		defer cancel()
		res := cr.safePass(ctx)
		cr.mu.Lock()
		cr.running = false
		cr.lastEnd = cr.now()
		cr.last = res
		cr.mu.Unlock()
		log.Printf("client refresh: %d clients from %d devices (%d skipped)%s",
			res.clients, res.read, res.skipped, map[bool]string{true: ", timed out"}[res.timedOut])
	}()
	return st, nil
}

func (cr *ClientRefresher) safePass(ctx context.Context) (res clientRefreshResult) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("client refresh: panic: %v", r)
		}
	}()
	return cr.pass(ctx)
}

// pass reads every device the plan allows, merges what they report by MAC
// and writes it to the cache.
func (cr *ClientRefresher) pass(ctx context.Context) clientRefreshResult {
	var res clientRefreshResult
	devices, err := queries.ListDevices(cr.db)
	if err != nil {
		log.Printf("client refresh: list devices: %v", err)
		return res
	}
	found := make(map[string]*queries.MACLookup) // by upper-case MAC
	for _, dev := range devices {
		if ctx.Err() != nil {
			res.timedOut = true
			res.skipped++
			continue
		}
		now := cr.now()
		// A paused device gets nothing but liveness pings — this included.
		if dev.Status != "online" || cr.cadence.Paused(dev, now) {
			res.skipped++
			continue
		}
		run := cr.runner(dev.ID)
		if run == nil {
			res.skipped++
			continue
		}
		res.read++
		devName := nonempty(dev.Identity, dev.Address)
		if cr.cadence.Interval(dev, devclass.TierARP, now) > 0 {
			cr.readARP(run, dev.ID, devName, found)
		}
		if cr.cadence.Interval(dev, devclass.TierDHCPLeases, now) > 0 {
			cr.readDHCP(run, dev.ID, devName, found)
		}
		if cr.cadence.Interval(dev, devclass.TierWifiRegistrations, now) > 0 {
			cr.readRegistrations(run, dev, devName, now, found)
		}
	}

	// External DHCP (Kea / OPNsense, per app_settings): clients — and DHCP
	// host names — on subnets no MikroTik device sees.
	for _, l := range cr.external() {
		mac := strings.ToUpper(l.MAC)
		if mac == "" {
			continue
		}
		if c, ok := found[mac]; ok {
			c.IPAddress = queries.PreferIP(c.IPAddress, l.IP)
			if c.HostName == "" {
				c.HostName = l.Hostname
			}
			continue
		}
		found[mac] = &queries.MACLookup{
			MACAddress: mac, IPAddress: l.IP, HostName: l.Hostname, Source: "dhcp", DeviceName: l.Origin,
		}
	}

	cr.store(found)
	res.clients = len(found)
	return res
}

// readARP adds one device's ARP table.
func (cr *ClientRefresher) readARP(run routeros.Runner, devID, devName string, found map[string]*queries.MACLookup) {
	defer func() { recover() }()
	arps, err := routeros.GetARPTable(run)
	if err != nil {
		log.Printf("client refresh: arp %s: %v", devName, err)
		return
	}
	for _, a := range arps {
		mac := strings.ToUpper(a.MAC)
		if mac == "" {
			continue
		}
		if c, ok := found[mac]; ok {
			c.IPAddress = queries.PreferIP(c.IPAddress, a.Address)
			continue
		}
		found[mac] = &queries.MACLookup{
			MACAddress: mac, IPAddress: a.Address, InterfaceName: a.Interface,
			Source: "arp", DeviceID: devID, DeviceName: devName,
		}
	}
}

// readDHCP adds one device's DHCP leases.
func (cr *ClientRefresher) readDHCP(run routeros.Runner, devID, devName string, found map[string]*queries.MACLookup) {
	defer func() { recover() }()
	leases, err := routeros.GetDHCPLeases(run)
	if err != nil {
		log.Printf("client refresh: dhcp %s: %v", devName, err)
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
		if c, ok := found[mac]; ok {
			c.IPAddress = queries.PreferIP(c.IPAddress, addr)
			if l.HostName != "" {
				if c.HostName == "" {
					c.HostName = l.HostName
				}
				c.Source = "dhcp"
			}
			continue
		}
		found[mac] = &queries.MACLookup{
			MACAddress: mac, IPAddress: addr, HostName: l.HostName, InterfaceName: l.Server,
			Source: "dhcp", DeviceID: devID, DeviceName: devName,
		}
	}
}

// readRegistrations adds one device's wireless clients, from the
// registration menus its capability probe found. A device without a
// wireless package is asked for nothing.
func (cr *ClientRefresher) readRegistrations(run routeros.Runner, dev queries.Device, devName string, now time.Time, found map[string]*queries.MACLookup) {
	defer func() { recover() }()
	caps := cr.caps.Ensure(dev.ID, dev.ROSVersion, run, now)
	for _, menu := range caps.RegistrationMenus() {
		regs, err := routeros.GetRegistrations(run, menu)
		if err != nil {
			// The package or the policy changed since the probe: re-probe
			// soon instead of trapping again.
			cr.caps.NoteReadTrap(dev.ID, menu, err, now)
			continue
		}
		for _, reg := range regs {
			mac := strings.ToUpper(reg.MAC)
			if mac == "" || mac == "00:00:00:00:00:00" || !wirelessRegistration(reg) {
				continue
			}
			// The AP is the registration's own field, else the CAP part of
			// an interface named "cap/radio".
			ap := reg.AP
			if ap == "" {
				ap, _, _ = strings.Cut(reg.Interface, "/")
			}
			c, ok := found[mac]
			if !ok {
				c = &queries.MACLookup{
					MACAddress: mac, InterfaceName: reg.Interface, DeviceID: dev.ID, DeviceName: devName,
				}
				found[mac] = c
			}
			c.IPAddress = queries.PreferIP(c.IPAddress, reg.LastIP)
			c.Source = "wifi"
			c.AP, c.SSID, c.Band, c.Channel, c.Frequency = ap, reg.SSID, reg.Band, reg.Channel, reg.Frequency
			c.Signal, c.TxRate, c.RxRate, c.Uptime = reg.Signal, reg.TxRate, reg.RxRate, reg.Uptime
		}
	}
}

// wirelessRegistration reports whether a registration row is a wireless
// client: a real one carries an SSID, a band or a signal; a row with none
// of them on a wired-looking interface is not one.
func wirelessRegistration(reg routeros.WifiRegistration) bool {
	if reg.SSID != "" || reg.Band != "" || reg.Signal != "" {
		return true
	}
	iface := strings.ToLower(reg.Interface)
	for _, wired := range []string{"ether", "bridge", "vlan", "pppoe"} {
		if strings.Contains(iface, wired) {
			return false
		}
	}
	return true
}

// store resolves the clients' DNS names and writes them to the cache.
func (cr *ClientRefresher) store(found map[string]*queries.MACLookup) {
	if len(found) == 0 {
		return
	}
	var dnsNames map[string]string
	if cr.resolver != nil {
		ips := make([]string, 0, len(found))
		for _, c := range found {
			if c.IPAddress != "" {
				ips = append(ips, c.IPAddress)
			}
		}
		dnsNames = cr.resolver.ResolveMany(ips)
	}
	for _, c := range found {
		if name := dnsNames[c.IPAddress]; name != "" {
			c.DNSName = name
			if c.HostName == "" {
				c.HostName = name
			}
		}
		if err := queries.UpsertMACLookup(cr.db, c); err != nil {
			log.Printf("client refresh: store %s: %v", c.MACAddress, err)
		}
	}
}
