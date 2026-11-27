package poller

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// The Settings page offered topology_interval and network_health_interval,
// saved them, and nothing read them: the env vars won. They are the fleet
// floors of their tiers now, clamped and picked up without a restart — and a
// database is first brought to what was in force, so a value typed into a
// dead field long ago never springs to life.
func TestTopologyAndNetworkHealthSettingsAreTheFloors(t *testing.T) {
	db := pollerTestDB(t)
	// Defaults of the env vars (nothing set in the environment), and a
	// stale value someone once typed into the dead field.
	cfg := &config.Config{TopologyInterval: 60 * time.Second, NetworkHealthInterval: 60 * time.Second, RetentionDays: 7}
	mustSet(t, db, "topology_interval", "5")
	AdoptIntervalSettings(db, cfg)

	f := newFloors(db, cfg)
	now := time.Now()
	at := func(tier devclass.Tier) time.Duration {
		now = now.Add(floorTTL) // past the cache
		return f.of(tier, now)
	}
	if got := at(devclass.TierNeighbors); got != time.Minute {
		t.Errorf("after adoption: neighbors floor = %v, want what was in force (1m), not the stale 5 s", got)
	}
	if got := at(devclass.TierBridgePorts); got != time.Minute {
		t.Errorf("after adoption: bridge ports floor = %v, want 1m", got)
	}

	mustSet(t, db, "topology_interval", "300")
	mustSet(t, db, "network_health_interval", "600")
	AdoptIntervalSettings(db, cfg) // a later start must not undo an edit
	for _, tier := range []devclass.Tier{devclass.TierNeighbors, devclass.TierRoutes} {
		if got := at(tier); got != 5*time.Minute {
			t.Errorf("%s floor = %v, want the setting 5m", tier, got)
		}
	}
	for _, tier := range []devclass.Tier{devclass.TierBridgePorts, devclass.TierBridgeVLAN, devclass.TierLogs} {
		if got := at(tier); got != 10*time.Minute {
			t.Errorf("%s floor = %v, want the setting 10m", tier, got)
		}
	}
	if got := TopologyFloor(db, cfg); got != 5*time.Minute {
		t.Errorf("TopologyFloor = %v, want 5m", got)
	}
	if got := NetworkHealthFloor(db, cfg); got != 10*time.Minute {
		t.Errorf("NetworkHealthFloor = %v, want 10m", got)
	}

	// Clamped to 30 s .. 1 h; garbage falls back to the default.
	mustSet(t, db, "topology_interval", "1")
	mustSet(t, db, "network_health_interval", "999999999999")
	if got := at(devclass.TierNeighbors); got != 30*time.Second {
		t.Errorf("tiny topology_interval = %v, want 30s", got)
	}
	if got := at(devclass.TierLogs); got != time.Hour {
		t.Errorf("huge network_health_interval = %v, want 1h", got)
	}
	mustSet(t, db, "topology_interval", "soon")
	if got := at(devclass.TierNeighbors); got != time.Minute {
		t.Errorf("non-numeric topology_interval = %v, want the default", got)
	}

	// A deployment configured by env stays configured by env.
	locked := &config.Config{TopologyInterval: 90 * time.Second, NetworkHealthInterval: 2 * time.Minute,
		EnvLocked: map[string]bool{"topology_interval": true, "network_health_interval": true}}
	mustSet(t, db, "topology_interval", "300")
	if got := TopologyFloor(db, locked); got != 90*time.Second {
		t.Errorf("env set: TopologyFloor = %v, want the env value 90s", got)
	}
	if got := newFloors(db, locked).of(devclass.TierBridgeVLAN, time.Now()); got != 2*time.Minute {
		t.Errorf("env set: bridge vlan floor = %v, want the env value 2m", got)
	}
}

// wifi_interval and client_discovery_interval accepted any positive number:
// 1 made the wifi loop spin and a pasted huge value overflowed. They are
// clamped to 15 s .. 1 h and 5 min .. 24 h.
func TestWifiAndClientDiscoveryIntervalsAreClamped(t *testing.T) {
	db := pollerTestDB(t)
	f := newFloors(db, nil)
	now := time.Now()
	at := func(tier devclass.Tier) time.Duration {
		now = now.Add(floorTTL)
		return f.of(tier, now)
	}
	cases := []struct {
		key, value string
		tier       devclass.Tier
		want       time.Duration
	}{
		{"wifi_interval", "1", devclass.TierWifiRegistrations, 15 * time.Second},
		{"wifi_interval", "45", devclass.TierWifiRegistrations, 45 * time.Second},
		{"wifi_interval", "99999999", devclass.TierWifiRegistrations, time.Hour},
		{"client_discovery_interval", "60", devclass.TierARP, 5 * time.Minute},
		{"client_discovery_interval", "1800", devclass.TierDHCPLeases, 30 * time.Minute},
		{"client_discovery_interval", "99999999", devclass.TierARP, 24 * time.Hour},
	}
	for _, c := range cases {
		mustSet(t, db, c.key, c.value)
		if got := at(c.tier); got != c.want {
			t.Errorf("%s=%s: %s floor = %v, want %v", c.key, c.value, c.tier, got, c.want)
		}
	}
}

// retention_days was saved by the Settings page and never read. The retention
// job reads it each cycle — after the database was brought to what was in
// force, so a stale "1" in the dead field cannot delete history.
func TestRetentionDaysSettingIsRead(t *testing.T) {
	db := pollerTestDB(t)
	cfg := &config.Config{RetentionDays: 7}
	mustSet(t, db, "retention_days", "1") // typed long ago, never effective
	AdoptIntervalSettings(db, cfg)
	if got := RetentionDays(db, cfg); got != 7 {
		t.Errorf("after adoption = %d, want what was in force (7)", got)
	}
	for _, c := range []struct {
		value string
		want  int
	}{{"30", 30}, {"0", 7}, {"-3", 7}, {"x", 7}, {"1", 1}, {"99999", 3650}} {
		mustSet(t, db, "retention_days", c.value)
		AdoptIntervalSettings(db, cfg) // once per database: an edit sticks
		if got := RetentionDays(db, cfg); got != c.want {
			t.Errorf("retention_days=%q = %d, want %d", c.value, got, c.want)
		}
	}
	env := &config.Config{RetentionDays: 14, EnvLocked: map[string]bool{"retention_days": true}}
	mustSet(t, db, "retention_days", "30")
	if got := RetentionDays(db, env); got != 14 {
		t.Errorf("env set = %d, want the env value 14", got)
	}

	// A database first started with the variable set adopts the env value.
	db2 := pollerTestDB(t)
	AdoptIntervalSettings(db2, env)
	if v, _ := queries.GetSetting(db2, "retention_days"); v != "14" {
		t.Errorf("adopted retention_days = %q, want 14", v)
	}
}

func mustSet(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	if err := queries.SetSetting(db, key, value); err != nil {
		t.Fatalf("set %s: %v", key, err)
	}
}
