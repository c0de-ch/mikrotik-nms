package poller

import (
	"database/sql"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
	"github.com/mikrotik-nms/backend/internal/ingest"
	"github.com/mikrotik-nms/backend/internal/leasesource"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Seams are what a poller split into its collect half is built on (design
// 3.5, the M1 series converts one poller at a time): where it learns what to
// read (Config), how it reaches a device (Exec), and where what it saw goes
// (Sink). Its collect half touches neither the database nor the pool
// (internal/fences). The manager builds the monolith's seams once.
type Seams struct {
	Config edgeproto.ConfigSource
	Exec   routeros.DeviceExec
	Sink   edgeproto.Sink
}

// monolithSeams are the seams over the paths the pollers always took: the
// device table, the settings and cadence for Config; the pooled sessions for
// Exec; ingest, applying every Emit in one transaction, for Sink. now is the
// loops' clock the cadence and the pause are judged at.
func monolithSeams(db *sql.DB, pool *routeros.Pool, cadence CadenceSource, now func() time.Time) Seams {
	return Seams{
		Config: &dbConfig{db: db, cadence: cadence, now: now},
		Exec:   routeros.PoolExec{Pool: pool},
		Sink:   ingest.NewSink(db),
	}
}

// dbConfig is the monolith's edgeproto.ConfigSource: the device table, the
// app settings and the cadence every loop shares, read at each call.
type dbConfig struct {
	db      *sql.DB
	cadence CadenceSource
	now     func() time.Time
}

// Devices lists the devices with each one's cadence and pause as of now.
func (c *dbConfig) Devices() ([]edgeproto.DeviceSpec, error) {
	devices, err := queries.ListDevices(c.db)
	if err != nil {
		return nil, err
	}
	now := c.now()
	specs := make([]edgeproto.DeviceSpec, 0, len(devices))
	for _, dev := range devices {
		specs = append(specs, deviceSpec(dev, c.cadence, now))
	}
	return specs, nil
}

// deviceSpec is dev as the seams describe it at now: its cadence per tier
// and its pause as cadence judges them (CadenceSource.Interval, .Paused).
func deviceSpec(dev queries.Device, cadence CadenceSource, now time.Time) edgeproto.DeviceSpec {
	spec := edgeproto.DeviceSpec{
		ID:      dev.ID,
		Name:    nonempty(dev.Identity, dev.Address),
		Online:  dev.Status == "online",
		Paused:  cadence.Paused(dev, now),
		Cadence: make(map[devclass.Tier]time.Duration),
	}
	for _, tier := range devclass.Tiers() {
		if iv := cadence.Interval(dev, tier, now); iv > 0 {
			spec.Cadence[tier] = iv
		}
	}
	return spec
}

// Setting is the stored value of key, "" when there is none or it cannot be
// read.
func (c *dbConfig) Setting(key string) string {
	if c.db == nil {
		return ""
	}
	v, err := queries.GetSetting(c.db, key)
	if err != nil {
		return ""
	}
	return v
}

// externalLeases is the client discovery's external lease source in the
// monolith: the Kea Control Agents and OPNsense instances app_settings name.
func (m *Manager) externalLeases() []leasesource.Lease {
	return leasesource.FromSettings(m.db)
}
