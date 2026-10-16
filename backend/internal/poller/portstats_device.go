package poller

import (
	"database/sql"
	"log"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// inventoryRefresh bounds how long an unchanged interfaces row goes without
// a rewrite. Rows are written when their inventory changes, and all of them
// at least this often, so last_updated keeps the meaning it had when the
// hourly info pass rewrote the table.
const inventoryRefresh = time.Hour

// DeviceIfaces is one device's interface state as the port-stats collector
// holds it.
type DeviceIfaces struct {
	// At is when the Tier 0 /interface/print behind Rows was read (UTC).
	At time.Time
	// EthernetAt is when the merged Tier 4a ethernet rows were read; zero
	// when none is merged (not read yet, or stale).
	EthernetAt time.Time
	// Rows is the Tier 0 read in the device's print order, ethernet ports
	// with their Tier 4a row merged in by name. A copy: callers may keep it.
	Rows []routeros.InterfaceSnapshot
	// CommandsLastMinute counts the RouterOS commands this collector issued
	// to the device during the last complete minute (Tier 0, Tier 4a, FDB
	// and stacking reads) — the seed of the per-device NMS self-metrics.
	CommandsLastMinute int
}

// deviceState is the collector's per-device memory.
type deviceState struct {
	// Tier 0: the latest rows (the shared interface snapshot), when they
	// were read, and at which interval (every age rule of the device
	// follows it).
	at       time.Time
	interval time.Duration
	rows     []routeros.InterfaceStats
	// Tier 4a: the latest ethernet rows by name.
	ethAt       time.Time
	ethInterval time.Duration
	eth         map[string]routeros.EthernetStats
	// When each tier is due next; zero = at the next wake-up.
	nextStats, nextEthernet time.Time
	// What the interfaces table last received, by name; invAt is when every
	// row was last written. nil = nothing written since startup.
	inv   map[string]inventoryRow
	invAt time.Time
	// Commands issued to the device: cmdCount in unix minute cmdMinute,
	// cmdPrev in the minute before it.
	cmdMinute         int64
	cmdCount, cmdPrev int
}

// fresh reports whether the Tier 0 rows may still be served at now: read
// within 3× the device's interval — the same rule that expires its rates.
func (st *deviceState) fresh(now time.Time) bool {
	return !st.at.IsZero() && now.Sub(st.at) <= 3*st.interval
}

// ethernetFresh is fresh for the Tier 4a rows.
func (st *deviceState) ethernetFresh(now time.Time) bool {
	return !st.ethAt.IsZero() && now.Sub(st.ethAt) <= 3*st.ethInterval
}

// commandsLastMinute is the command count of the last complete minute
// before now.
func (st *deviceState) commandsLastMinute(now time.Time) int {
	switch now.Unix() / 60 {
	case st.cmdMinute:
		return st.cmdPrev
	case st.cmdMinute + 1:
		return st.cmdCount
	}
	return 0
}

// deviceLocked returns (creating) deviceID's state. c.mu held.
func (c *PortStatsCollector) deviceLocked(deviceID string) *deviceState {
	st := c.devs[deviceID]
	if st == nil {
		st = &deviceState{}
		c.devs[deviceID] = st
	}
	return st
}

// forgetDevicesNotIn drops the state of devices that no longer exist.
func (c *PortStatsCollector) forgetDevicesNotIn(devices []queries.Device) {
	keep := make(map[string]bool, len(devices))
	for _, d := range devices {
		keep[d.ID] = true
	}
	c.mu.Lock()
	for id := range c.devs {
		if !keep[id] {
			delete(c.devs, id)
		}
	}
	c.mu.Unlock()
}

// retryEthernet makes deviceID's Tier 4a read due again at the next wake-up.
func (c *PortStatsCollector) retryEthernet(deviceID string) {
	c.mu.Lock()
	c.deviceLocked(deviceID).nextEthernet = time.Time{}
	c.mu.Unlock()
}

// ingestEthernet stores one device's Tier 4a rows read at t.
func (c *PortStatsCollector) ingestEthernet(deviceID string, rows []routeros.EthernetStats, t time.Time, interval time.Duration) {
	byName := make(map[string]routeros.EthernetStats, len(rows))
	for _, e := range rows {
		byName[e.Name] = e
	}
	c.mu.Lock()
	st := c.deviceLocked(deviceID)
	st.eth, st.ethAt, st.ethInterval = byName, t, interval
	c.mu.Unlock()
}

// DeviceSnapshot returns deviceID's latest Tier 0 interface read with the
// Tier 4a ethernet rows merged in by name, and when it was read. ok is false
// before the first read and once the read is older than 3× the device's
// counter interval (offline or failing device); consumers then fall back to
// what they cached — none of them prints the interface table on a schedule
// of its own. Safe for concurrent use; the rows are a copy. A nil collector
// has no snapshots.
func (c *PortStatsCollector) DeviceSnapshot(deviceID string) (rows []routeros.InterfaceSnapshot, at time.Time, ok bool) {
	d, ok := c.DeviceIfaces(deviceID)
	return d.Rows, d.At, ok
}

// DeviceIfaces is DeviceSnapshot with the rest of the device's state. ok
// reports a servable snapshot as DeviceSnapshot does; CommandsLastMinute is
// filled for every device the collector knows, ok or not (a device that
// stopped answering still received the commands).
func (c *PortStatsCollector) DeviceIfaces(deviceID string) (DeviceIfaces, bool) {
	if c == nil {
		return DeviceIfaces{}, false
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.devs[deviceID]
	if st == nil {
		return DeviceIfaces{}, false
	}
	d := DeviceIfaces{CommandsLastMinute: st.commandsLastMinute(now)}
	if !st.fresh(now) {
		return d, false
	}
	var eth map[string]routeros.EthernetStats
	if st.ethernetFresh(now) {
		eth, d.EthernetAt = st.eth, st.ethAt
	}
	d.At = st.at
	d.Rows = routeros.MergeEthernet(st.rows, eth)
	return d, true
}

// inventoryRow is the part of an interface the interfaces table stores.
type inventoryRow struct {
	typ, mac, comment string
	mtu               int
	running, disabled bool
}

func inventoryOf(s routeros.InterfaceStats) inventoryRow {
	return inventoryRow{typ: s.Type, mac: s.MAC, comment: s.Comment, mtu: s.MTU, running: s.Running, disabled: s.Disabled}
}

func interfaceRow(deviceID string, s routeros.InterfaceStats) queries.Interface {
	mtu := s.MTU
	return queries.Interface{
		ID: deviceID + ":" + s.Name, DeviceID: deviceID, Name: s.Name, Type: s.Type,
		MACAddress: s.MAC, MTU: &mtu, Running: s.Running, Disabled: s.Disabled, Comment: s.Comment,
	}
}

// syncInventory keeps the interfaces table in step with a Tier 0 read taken
// at t — the job the hourly info pass's own /interface/print used to do. A
// row is upserted when one of its inventory fields changed since the last
// write, and every row at least each inventoryRefresh (and on the first read
// after startup). The device's rows the print no longer lists — interfaces
// removed or renamed, dynamic CAPsMAN radios that went away — are pruned in
// the same transaction, with the same guard as before: an empty print never
// wipes a device.
func (c *PortStatsCollector) syncInventory(dev queries.Device, stats []routeros.InterfaceStats, t time.Time) {
	if c.db == nil || len(stats) == 0 {
		return
	}
	current := make(map[string]inventoryRow, len(stats))
	names := make([]string, 0, len(stats))
	for _, s := range stats {
		if _, dup := current[s.Name]; !dup {
			names = append(names, s.Name)
		}
		current[s.Name] = inventoryOf(s)
	}

	c.mu.Lock()
	st := c.deviceLocked(dev.ID)
	full := st.inv == nil || t.Sub(st.invAt) >= inventoryRefresh
	var upserts []queries.Interface
	for _, s := range stats {
		if prev, ok := st.inv[s.Name]; full || !ok || prev != inventoryOf(s) {
			upserts = append(upserts, interfaceRow(dev.ID, s))
		}
	}
	gone := false
	for name := range st.inv {
		if _, ok := current[name]; !ok {
			gone = true
			break
		}
	}
	c.mu.Unlock()
	if len(upserts) == 0 && !gone {
		return
	}

	var keep []string
	if full || gone {
		keep = names
	}
	pruned, err := queries.SyncInterfaces(c.db, dev.ID, upserts, keep)
	if err != nil {
		c.logThrottled("inventory:"+dev.ID, "poller portstats: store interfaces of %s: %v", dev.Identity, err)
		return // retried with the next read: inv still holds the last write
	}
	if pruned > 0 {
		log.Printf("poller portstats: pruned %d stale interfaces of %s", pruned, dev.Identity)
	}
	c.mu.Lock()
	if st := c.devs[dev.ID]; st != nil {
		st.inv = current
		if full {
			st.invAt = t
		}
	}
	c.mu.Unlock()
}

// countingRunner forwards to a device's pooled client and counts every
// command against the device (DeviceIfaces.CommandsLastMinute).
type countingRunner struct {
	c        *PortStatsCollector
	deviceID string
	inner    routeros.Runner
}

func (r countingRunner) Run(command string, args ...string) (*ros.Reply, error) {
	r.c.countCommand(r.deviceID, r.c.now())
	return r.inner.Run(command, args...)
}

// deviceRunner returns deviceID's counted device link, or errNoClient when
// the pool holds no connection for it.
func (c *PortStatsCollector) deviceRunner(deviceID string) (routeros.Runner, error) {
	run := c.runner(deviceID)
	if run == nil {
		return nil, errNoClient
	}
	return countingRunner{c: c, deviceID: deviceID, inner: run}, nil
}

// countCommand records one RouterOS command issued to deviceID at t.
func (c *PortStatsCollector) countCommand(deviceID string, t time.Time) {
	m := t.Unix() / 60
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.deviceLocked(deviceID)
	switch m {
	case st.cmdMinute:
	case st.cmdMinute + 1:
		st.cmdPrev, st.cmdCount, st.cmdMinute = st.cmdCount, 0, m
	default: // first command, a quiet minute in between, or a clock step
		st.cmdPrev, st.cmdCount, st.cmdMinute = 0, 0, m
	}
	st.cmdCount++
}

// PortHostsFreshness is how recent a port_hosts row must be to count as
// current: three dump intervals, at least 15 min.
func PortHostsFreshness(db *sql.DB) time.Duration {
	return max(3*PortHostsInterval(db), 15*time.Minute)
}
