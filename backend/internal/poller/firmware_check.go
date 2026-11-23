package poller

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/firmware"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// Firmware availability without asking the devices (P0-14).
//
// Each device's update channel and installed version come from its Tier 6
// inventory read (two read-only proplisted prints, ReadFirmware). The newest
// version of every channel comes from MikroTik's release feed, fetched by
// this process (FirmwareChecker.Refresh). "Update available" is the
// comparison of the two — no device ever runs check-for-updates with the
// poll credential; only an admin's upgrade job does, on its own one-shot
// session with the credentials typed for it.

// FirmwareFetchMinInterval is the shortest gap between two fetches of the
// release feed, scheduled or on demand ("Check All").
const FirmwareFetchMinInterval = time.Hour

// firmwareFetchMu makes feed fetches one at a time process-wide: the
// scheduled loop and the API each build their own checker.
var firmwareFetchMu sync.Mutex

// FirmwareChecker fetches the release feed and keeps firmware_status's
// newest-version comparison current.
type FirmwareChecker struct {
	db   *sql.DB
	hub  *ws.Hub
	Feed firmware.Feed
	now  func() time.Time
}

// NewFirmwareChecker reads MikroTik's public feed.
func NewFirmwareChecker(db *sql.DB, hub *ws.Hub) *FirmwareChecker {
	return &FirmwareChecker{db: db, hub: hub, now: time.Now}
}

// LastFetched is the time of the newest successful feed fetch.
func (c *FirmwareChecker) LastFetched() (time.Time, bool) {
	rels, err := queries.ListFirmwareReleases(c.db)
	if err != nil || len(rels) == 0 {
		return time.Time{}, false
	}
	var last time.Time
	for _, r := range rels {
		if r.FetchedAt.After(last) {
			last = r.FetchedAt
		}
	}
	return last, true
}

// Refresh fetches the feed when the last successful fetch is older than
// minAge (never more often than FirmwareFetchMinInterval), then recomputes
// every device's comparison. It reports whether it fetched. A channel whose
// fetch fails keeps its stored version; only a fetch that failed for every
// channel is an error.
func (c *FirmwareChecker) Refresh(ctx context.Context, minAge time.Duration) (bool, error) {
	firmwareFetchMu.Lock()
	defer firmwareFetchMu.Unlock()

	minAge = max(minAge, FirmwareFetchMinInterval)
	if last, ok := c.LastFetched(); ok && c.now().Sub(last) < minAge {
		return false, c.Recompute()
	}
	var ok int
	var lastErr error
	for _, ch := range firmware.Channels {
		rel, err := c.Feed.Fetch(ctx, ch)
		if err != nil {
			log.Printf("firmware: release feed: %v", err)
			lastErr = err
			continue
		}
		r := queries.FirmwareRelease{Channel: ch, Version: rel.Version, FetchedAt: c.now()}
		if !rel.BuildTime.IsZero() {
			bt := rel.BuildTime
			r.BuildTime = &bt
		}
		if err := queries.UpsertFirmwareRelease(c.db, r); err != nil {
			log.Printf("firmware: store %s release: %v", ch, err)
			lastErr = err
			continue
		}
		ok++
	}
	if err := c.Recompute(); err != nil {
		return ok > 0, err
	}
	if ok == 0 {
		if lastErr == nil {
			lastErr = errors.New("no channel fetched")
		}
		return false, lastErr
	}
	return true, nil
}

// releaseMap is the stored newest version per channel.
func (c *FirmwareChecker) releaseMap() (map[string]string, error) {
	rels, err := queries.ListFirmwareReleases(c.db)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rels))
	for _, r := range rels {
		out[r.Channel] = r.Version
	}
	return out, nil
}

// compare is the one availability rule: the newest of the device's channel
// against its installed version. latest is nil while the channel's newest is
// unknown.
func compareFirmware(releases map[string]string, channel, installed string) (*string, bool) {
	v, ok := releases[channel]
	if !ok || v == "" {
		return nil, false
	}
	return &v, firmware.UpdateAvailable(installed, v)
}

// Recompute re-evaluates every stored device against the stored feed —
// database only, no device I/O.
func (c *FirmwareChecker) Recompute() error {
	releases, err := c.releaseMap()
	if err != nil {
		return err
	}
	statuses, err := queries.ListFirmwareStatus(c.db)
	if err != nil {
		return err
	}
	for _, st := range statuses {
		latest, avail := compareFirmware(releases, st.Channel, st.InstalledVersion)
		changed, err := queries.SetFirmwareLatest(c.db, st.DeviceID, latest, avail)
		if err != nil {
			log.Printf("firmware: update %s: %v", st.DeviceID, err)
			continue
		}
		if changed && avail && !st.UpdateAvailable {
			c.publishAvailable(st.DeviceID, st.InstalledVersion, *latest)
		}
	}
	return nil
}

// RecordDevice stores a device's inventory read and its comparison.
func (c *FirmwareChecker) RecordDevice(deviceID string, pkg *routeros.FirmwareInfo, rb *routeros.RouterboardInfo) {
	if pkg == nil || pkg.InstalledVersion == "" {
		return
	}
	releases, err := c.releaseMap()
	if err != nil {
		log.Printf("firmware: releases: %v", err)
		return
	}
	prev, _ := queries.GetFirmwareStatusByDevice(c.db, deviceID)
	latest, avail := compareFirmware(releases, pkg.Channel, pkg.InstalledVersion)
	st := &queries.FirmwareStatus{
		DeviceID:         deviceID,
		Channel:          pkg.Channel,
		InstalledVersion: pkg.InstalledVersion,
		LatestVersion:    latest,
		UpdateAvailable:  avail,
	}
	if rb != nil {
		st.RouterboardCurrent = &rb.CurrentFirmware
		st.RouterboardUpgrade = &rb.UpgradeFirmware
	}
	if err := queries.UpsertDeviceFirmware(c.db, st); err != nil {
		log.Printf("firmware: store %s: %v", deviceID, err)
		return
	}
	if avail && (prev == nil || !prev.UpdateAvailable) {
		c.publishAvailable(deviceID, pkg.InstalledVersion, *latest)
	}
}

func (c *FirmwareChecker) publishAvailable(deviceID, installed, latest string) {
	if c.hub == nil {
		return
	}
	c.hub.Publish("firmware.update", map[string]interface{}{
		"device_id":        deviceID,
		"installed":        installed,
		"latest":           latest,
		"update_available": true,
	})
}

// ReadFirmware is the firmware half of the Tier 6 inventory read: the update
// channel + installed version and the RouterBOARD firmware, both proplisted
// and read-only. A device without RouterBOARD firmware (CHR, x86) and a
// failed RouterBOARD read are not errors of the info pass.
func ReadFirmware(run routeros.Runner) (*routeros.FirmwareInfo, *routeros.RouterboardInfo, error) {
	pkg, err := routeros.ReadPackageUpdate(run)
	if err != nil {
		return nil, nil, err
	}
	rb, err := routeros.ReadRouterboard(run)
	if err != nil {
		if !errors.Is(err, routeros.ErrNoRouterboard) {
			log.Printf("firmware: routerboard read: %v", err)
		}
		rb = nil
	}
	return pkg, rb, nil
}
