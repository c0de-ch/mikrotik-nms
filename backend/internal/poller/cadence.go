package poller

import (
	"context"
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

// floors resolves the fleet-wide interval that floors each tier from the app
// settings, re-read at most every floorTTL. For topology and network health
// an environment variable set for this process wins over the setting
// (config.Locked); unset, its default is only the setting's default.
type floors struct {
	db  *sql.DB
	cfg *config.Config

	mu     sync.Mutex
	readAt time.Time
	byTier map[devclass.Tier]time.Duration
}

func newFloors(db *sql.DB, cfg *config.Config) *floors {
	return &floors{db: db, cfg: cfg}
}

// of returns tier's floor at now (0 = none).
func (f *floors) of(tier devclass.Tier, now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byTier == nil || now.Sub(f.readAt) >= floorTTL || now.Before(f.readAt) {
		f.byTier = settingFloors(f.db, f.cfg)
		f.readAt = now
	}
	return f.byTier[tier]
}

// Bounds of the settings-backed floors. A floor below the lower bound would
// only make a loop spin (the class cadences are never faster), and the upper
// bound keeps a typo from switching a read off.
const (
	minLoopFloor      = 30 * time.Second // topology, network health
	maxLoopFloor      = time.Hour
	minWifiFloor      = 15 * time.Second
	maxWifiFloor      = time.Hour
	minClientsFloor   = 5 * time.Minute
	maxClientsFloor   = 24 * time.Hour
	minRetentionDays  = 1
	maxRetentionDays  = 3650
	defaultRetainDays = 7
)

// settingFloors reads the runtime-tunable intervals that floor the tiers.
// Tier 1 and 1c (the CPU samples) have no floor: they run at their class
// cadence — 60 s and 10 min on every class.
func settingFloors(db *sql.DB, cfg *config.Config) map[devclass.Tier]time.Duration {
	counters, hosts := PortStatsInterval(db), PortHostsInterval(db)
	clients := clampDuration(settingSeconds(db, "client_discovery_interval", defaultClientDiscovery), minClientsFloor, maxClientsFloor)
	topo, health := TopologyFloor(db, cfg), NetworkHealthFloor(db, cfg)
	return map[devclass.Tier]time.Duration{
		devclass.TierCounters:          counters,
		devclass.TierEthernet:          counters,
		devclass.TierBridgeHosts:       hosts,
		devclass.TierRelations:         hosts,
		devclass.TierWifiRegistrations: clampDuration(settingSeconds(db, "wifi_interval", defaultWifiFloor), minWifiFloor, maxWifiFloor),
		devclass.TierARP:               clients,
		devclass.TierDHCPLeases:        clients,
		devclass.TierInventory:         settingSeconds(db, "info_interval", defaultInfoInterval),
		devclass.TierNeighbors:         topo,
		devclass.TierRoutes:            topo,
		devclass.TierBridgePorts:       health,
		devclass.TierBridgeVLAN:        health,
		devclass.TierLogs:              health,
	}
}

func clampDuration(d, lo, hi time.Duration) time.Duration {
	return min(max(d, lo), hi)
}

// TopologyFloor is how often the topology loop rebuilds the graph and the
// fleet floor of the neighbor and route reads:
// MIKROTIK_NMS_TOPOLOGY_INTERVAL when it is set in the environment, else the
// topology_interval setting (seconds, 30 s .. 1 h).
func TopologyFloor(db *sql.DB, cfg *config.Config) time.Duration {
	def := defaultTopologyFloor
	if cfg != nil && cfg.TopologyInterval > 0 {
		def = cfg.TopologyInterval
	}
	if cfg.Locked("topology_interval") {
		return def
	}
	return clampDuration(settingSeconds(db, "topology_interval", def), minLoopFloor, maxLoopFloor)
}

// NetworkHealthFloor is the same for the bridge / STP / log reads and the
// network-health summary: MIKROTIK_NMS_NETWORK_HEALTH_INTERVAL when set,
// else the network_health_interval setting.
func NetworkHealthFloor(db *sql.DB, cfg *config.Config) time.Duration {
	def := defaultNetworkHealthFloor
	if cfg != nil && cfg.NetworkHealthInterval > 0 {
		def = cfg.NetworkHealthInterval
	}
	if cfg.Locked("network_health_interval") {
		return def
	}
	return clampDuration(settingSeconds(db, "network_health_interval", def), minLoopFloor, maxLoopFloor)
}

// RetentionDays is how many days of history the retention job keeps:
// MIKROTIK_NMS_RETENTION_DAYS when it is set in the environment, else the
// retention_days setting (1 .. 3650). Read every retention cycle.
func RetentionDays(db *sql.DB, cfg *config.Config) int {
	def := defaultRetainDays
	if cfg != nil && cfg.RetentionDays > 0 {
		def = cfg.RetentionDays
	}
	if db == nil || cfg.Locked("retention_days") {
		return def
	}
	v, err := queries.GetSetting(db, "retention_days")
	if err != nil {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return min(max(n, minRetentionDays), maxRetentionDays)
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
	return secondsOr(v, def)
}

// secondsOr is a setting's value v in seconds, capped at maxFloor, or def
// when v is empty, not a number or not positive (settingSeconds' parsing,
// for a value a ConfigSource returned).
func secondsOr(v string, def time.Duration) time.Duration {
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
	switch tier {
	case devclass.TierEthernet:
		return ethernetInterval
	case devclass.TierCPU:
		return defaultCPUInterval
	case devclass.TierCPUPost:
		return defaultCPUPostInterval
	case devclass.TierAudit:
		return devclass.Cadence(devclass.C1, tier) // 6 h on every class; no fleet floor
	}
	return c.floors.of(tier, now)
}

func (c fleetCadence) Paused(dev queries.Device, now time.Time) bool {
	return dev.PollingPaused(now)
}

// poolRunner is the production device link of the loops not yet built on the
// seams: deviceID's pooled client, or nil while the pool holds no connection
// for it. It is routeros.PoolExec, the Exec the converted loops get.
func poolRunner(pool *routeros.Pool, deviceID string) routeros.Runner {
	return routeros.PoolExec{Pool: pool}.Device(context.Background(), deviceID)
}

// DevicePaused reports whether an admin pause of deviceID's polling is in
// force at now — for code outside the scheduled loops that must honour the
// pause too (the on-demand live stream, run-now actions). A device that cannot
// be read counts as not paused.
func DevicePaused(db *sql.DB, deviceID string, now time.Time) bool {
	_, paused := PausedUntil(db, deviceID, now)
	return paused
}

// PausedUntil is DevicePaused with the end of the pause (UTC), for the
// refusal that names it.
func PausedUntil(db *sql.DB, deviceID string, now time.Time) (time.Time, bool) {
	dev, err := queries.GetDevice(db, deviceID)
	if err != nil || !dev.PollingPaused(now) {
		return time.Time{}, false
	}
	return dev.PollingPausedUntil.UTC(), true
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

// adoptedKey marks a database whose once-dead settings were adopted.
const adoptedKey = "interval_settings_adopted"

// AdoptIntervalSettings runs once per database, on the first start of a
// version in which topology_interval, network_health_interval and
// retention_days take effect. Until then the Settings page saved them and
// nothing read them, so a database can hold anything there: the seeded
// defaults, or a value typed long ago that never did anything — and a stale
// retention_days=1 springing to life would delete history. Each key is
// therefore set to what has actually been in force (the env value or its
// default); from then on the settings are the truth and edits apply.
func AdoptIntervalSettings(db *sql.DB, cfg *config.Config) {
	if db == nil {
		return
	}
	if v, err := queries.GetSetting(db, adoptedKey); err == nil && v == "1" {
		return
	}
	topology, health, days := defaultTopologyFloor, defaultNetworkHealthFloor, defaultRetainDays
	if cfg != nil {
		if cfg.TopologyInterval > 0 {
			topology = cfg.TopologyInterval
		}
		if cfg.NetworkHealthInterval > 0 {
			health = cfg.NetworkHealthInterval
		}
		if cfg.RetentionDays > 0 {
			days = cfg.RetentionDays
		}
	}
	values := map[string]string{
		"topology_interval":       strconv.Itoa(int(topology / time.Second)),
		"network_health_interval": strconv.Itoa(int(health / time.Second)),
		"retention_days":          strconv.Itoa(days),
	}
	for key, value := range values {
		if err := queries.SetSetting(db, key, value); err != nil {
			log.Printf("poller: adopt setting %s: %v", key, err)
			return // not marked: tried again at the next start
		}
	}
	if err := queries.SetSetting(db, adoptedKey, "1"); err != nil {
		log.Printf("poller: adopt settings: %v", err)
		return
	}
	log.Printf("poller: topology_interval=%s network_health_interval=%s retention_days=%s taken over from the running configuration — they are settings now",
		values["topology_interval"], values["network_health_interval"], values["retention_days"])
}
