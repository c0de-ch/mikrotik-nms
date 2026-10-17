package poller

import (
	"database/sql"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Per-device cadences (design §2.2 P0-4, Appendix B). Every loop that reads
// devices wakes every pollerTick, re-reads the device list and runs, per
// device, only the tiers that are due. A tier's interval is the device's class
// cadence (devclass.EffectiveInterval: the effective class, or C1 on the light
// profile) floored by the fleet-wide setting or env var that used to drive the
// whole loop — an admin can still slow everything down, the class can only
// make a device gentler. The next run is the end of the last one plus that
// interval, ±10 % by the device's phase (devclass.NextRun); a slow run pushes
// the next one out, nothing catches up. A paused device runs nothing but the
// liveness ping, and nothing derived from its reads ages while it is paused.

const (
	// pollerTick is how often the per-device loops look for due tiers: short
	// against every cadence, so a read starts within a tick of being due and
	// a pause, profile or override change applies within one. A tick with
	// nothing due costs one device-list query.
	pollerTick = 5 * time.Second

	// noClientRetry is how soon a due tier is tried again on a device the
	// pool holds no connection for yet (cold start, redial cooldown) instead
	// of a whole interval later.
	noClientRetry = 30 * time.Second

	// floorTTL bounds how long the settings-backed floors are cached. Loops
	// ask per device and tier every tick; a Settings change applies within it.
	floorTTL = 5 * time.Second

	// maxFloor caps a settings-backed floor: a pasted huge number would
	// otherwise overflow the Duration multiply into a negative interval.
	maxFloor = 7 * 24 * time.Hour
)

// Fleet-wide defaults of the env-driven floors (config's defaults).
const (
	defaultTopologyFloor      = 60 * time.Second
	defaultNetworkHealthFloor = 60 * time.Second
	defaultWifiFloor          = 30 * time.Second
	defaultClientDiscovery    = 15 * time.Minute
)

// CadenceSource is how a loop learns, per device, how often a tier runs and
// whether an admin pause is in force. The manager builds one (NewCadence) and
// hands it to every loop; tests substitute a stub.
type CadenceSource interface {
	// Interval is the interval tier runs at on dev: the class cadence
	// floored by the fleet setting; 0 when the device's plan has no such
	// read. It ignores the pause on purpose — staleness rules need the
	// cadence the rows were written at, paused or not.
	Interval(dev queries.Device, tier devclass.Tier, now time.Time) time.Duration
	// Paused reports an admin pause in force at now.
	Paused(dev queries.Device, now time.Time) bool
}

// runInterval is the interval a loop schedules tier at: Interval, or 0 (the
// tier does not run) while the device is paused.
func runInterval(cs CadenceSource, dev queries.Device, tier devclass.Tier, now time.Time) time.Duration {
	if cs.Paused(dev, now) {
		return 0
	}
	return cs.Interval(dev, tier, now)
}

// floors resolves the fleet-wide interval that floors each tier: the env vars
// for the loops that are configured by env (topology, network health), the
// app settings for the others, re-read at most every floorTTL.
type floors struct {
	db            *sql.DB
	topology      time.Duration // MIKROTIK_NMS_TOPOLOGY_INTERVAL
	networkHealth time.Duration // MIKROTIK_NMS_NETWORK_HEALTH_INTERVAL

	mu     sync.Mutex
	readAt time.Time
	byTier map[devclass.Tier]time.Duration
}

func newFloors(db *sql.DB, cfg *config.Config) *floors {
	f := &floors{db: db, topology: defaultTopologyFloor, networkHealth: defaultNetworkHealthFloor}
	if cfg != nil {
		if cfg.TopologyInterval > 0 {
			f.topology = cfg.TopologyInterval
		}
		if cfg.NetworkHealthInterval > 0 {
			f.networkHealth = cfg.NetworkHealthInterval
		}
	}
	return f
}

// of returns tier's floor at now (0 = none).
func (f *floors) of(tier devclass.Tier, now time.Time) time.Duration {
	switch tier {
	case devclass.TierNeighbors, devclass.TierRoutes:
		return f.topology
	case devclass.TierBridgePorts, devclass.TierBridgeVLAN, devclass.TierLogs:
		return f.networkHealth
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byTier == nil || now.Sub(f.readAt) >= floorTTL || now.Before(f.readAt) {
		f.byTier = settingFloors(f.db)
		f.readAt = now
	}
	return f.byTier[tier]
}

// settingFloors reads the runtime-tunable intervals that floor the tiers of
// the settings-driven loops. Tier 1 (cpu) has no loop yet and no floor.
func settingFloors(db *sql.DB) map[devclass.Tier]time.Duration {
	counters, hosts := PortStatsInterval(db), PortHostsInterval(db)
	clients := settingSeconds(db, "client_discovery_interval", defaultClientDiscovery)
	return map[devclass.Tier]time.Duration{
		devclass.TierCounters:          counters,
		devclass.TierEthernet:          counters,
		devclass.TierBridgeHosts:       hosts,
		devclass.TierRelations:         hosts,
		devclass.TierWifiRegistrations: settingSeconds(db, "wifi_interval", defaultWifiFloor),
		devclass.TierARP:               clients,
		devclass.TierDHCPLeases:        clients,
		devclass.TierInventory:         settingSeconds(db, "info_interval", defaultInfoInterval),
	}
}

// settingSeconds reads a positive app_setting in seconds, capped at maxFloor;
// a missing, non-numeric or non-positive value yields def — the parsing the
// loops' own interval readers always used.
func settingSeconds(db *sql.DB, key string, def time.Duration) time.Duration {
	if db == nil {
		return def
	}
	v, err := queries.GetSetting(db, key)
	if err != nil {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	if d := time.Duration(n) * time.Second; d < maxFloor {
		return d
	}
	return maxFloor
}

// classCadence is the production CadenceSource: the per-class plan, floored.
type classCadence struct{ floors *floors }

// NewCadence returns the CadenceSource every loop shares: per device and tier
// max(devclass.EffectiveInterval(effective class, profile, tier), the fleet
// floor). cfg supplies the env floors (nil = their defaults).
func NewCadence(db *sql.DB, cfg *config.Config) CadenceSource {
	return classCadence{floors: newFloors(db, cfg)}
}

func (c classCadence) Interval(dev queries.Device, tier devclass.Tier, now time.Time) time.Duration {
	iv := devclass.EffectiveInterval(dev.EffectiveClass(), dev.Profile(), tier)
	if iv <= 0 {
		return 0
	}
	return max(iv, c.floors.of(tier, now))
}

func (c classCadence) Paused(dev queries.Device, now time.Time) bool {
	return dev.PollingPaused(now)
}

// fleetCadence is what the pollers do when nobody hands them a CadenceSource
// (their constructors, tests): every device on the fleet-wide interval, as
// before the per-class plan; the pause still holds. The manager replaces it
// with NewCadence.
type fleetCadence struct{ floors *floors }

func newFleetCadence(db *sql.DB) CadenceSource {
	return fleetCadence{floors: newFloors(db, nil)}
}

func (c fleetCadence) Interval(_ queries.Device, tier devclass.Tier, now time.Time) time.Duration {
	if tier == devclass.TierEthernet {
		return ethernetInterval
	}
	return c.floors.of(tier, now)
}

func (c fleetCadence) Paused(dev queries.Device, now time.Time) bool {
	return dev.PollingPaused(now)
}

// poolRunner is a loop's production device link: deviceID's pooled client,
// or nil while the pool holds no connection for it.
func poolRunner(pool *routeros.Pool, deviceID string) routeros.Runner {
	if pool == nil {
		return nil
	}
	if client := pool.GetLive(deviceID); client != nil {
		return routeros.ClientRunner{Client: client}
	}
	return nil
}

// DevicePaused reports whether an admin pause of deviceID's polling is in
// force at now — for code outside the scheduled loops that must honour the
// pause too (the on-demand live stream, run-now actions). A device that cannot
// be read counts as not paused.
func DevicePaused(db *sql.DB, deviceID string, now time.Time) bool {
	dev, err := queries.GetDevice(db, deviceID)
	if err != nil {
		return false
	}
	return dev.PollingPaused(now)
}

// pauseNotices logs, once per pause, that a consumer skips a paused device.
// The scheduled loops just do not run its tiers; the firmware check,
// connectivity probes and speed tests would otherwise have acted on it, and an
// operator looking for a missing sample should find the pause in the log.
type pauseNotices struct {
	mu   sync.Mutex
	seen map[string]time.Time // consumer|device → the pause end it was logged for
}

var pauseLog = &pauseNotices{seen: make(map[string]time.Time)}

// skip records that consumer skipped dev because of its pause.
func (p *pauseNotices) skip(consumer string, dev queries.Device) {
	if dev.PollingPausedUntil == nil {
		return
	}
	until := dev.PollingPausedUntil.UTC()
	key := consumer + "|" + dev.ID
	p.mu.Lock()
	if prev, ok := p.seen[key]; ok && prev.Equal(until) {
		p.mu.Unlock()
		return
	}
	p.seen[key] = until
	p.mu.Unlock()
	log.Printf("poller: %s skips %s while its polling is paused (until %s)",
		consumer, nonempty(dev.Identity, dev.Address), until.Format(time.RFC3339))
}
