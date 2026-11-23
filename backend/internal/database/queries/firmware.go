package queries

import (
	"database/sql"
	"time"
)

type FirmwareStatus struct {
	ID                 string     `json:"id"`
	DeviceID           string     `json:"device_id"`
	Channel            string     `json:"channel"`
	InstalledVersion   string     `json:"installed_version"`
	LatestVersion      *string    `json:"latest_version"`
	UpdateAvailable    bool       `json:"update_available"`
	RouterboardCurrent *string    `json:"routerboard_current"`
	RouterboardUpgrade *string    `json:"routerboard_upgrade"`
	LastChecked        *time.Time `json:"last_checked"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// UpsertDeviceFirmware records what a device's inventory read found (channel,
// installed version, RouterBOARD firmware — nil RouterBOARD values keep the
// stored ones, so a failed read never blanks them) together with the newest
// version of its channel and whether that is an update. LatestVersion nil =
// the channel's newest is not known yet. last_checked is the device read.
func UpsertDeviceFirmware(db *sql.DB, f *FirmwareStatus) error {
	_, err := db.Exec(
		`INSERT INTO firmware_status (id, device_id, channel, installed_version, latest_version,
		    update_available, routerboard_current, routerboard_upgrade, last_checked, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id) DO UPDATE SET
		    channel=excluded.channel,
		    installed_version=COALESCE(NULLIF(excluded.installed_version, ''), firmware_status.installed_version),
		    latest_version=excluded.latest_version,
		    update_available=excluded.update_available,
		    routerboard_current=COALESCE(excluded.routerboard_current, firmware_status.routerboard_current),
		    routerboard_upgrade=COALESCE(excluded.routerboard_upgrade, firmware_status.routerboard_upgrade),
		    last_checked=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP`,
		f.DeviceID+":fw", f.DeviceID, f.Channel, f.InstalledVersion, f.LatestVersion,
		f.UpdateAvailable, f.RouterboardCurrent, f.RouterboardUpgrade,
	)
	return err
}

// SetFirmwareLatest updates a device's newest-version comparison without a
// device read (the release feed changed). It reports whether the row changed.
func SetFirmwareLatest(db *sql.DB, deviceID string, latest *string, available bool) (bool, error) {
	res, err := db.Exec(
		`UPDATE firmware_status SET latest_version = ?, update_available = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE device_id = ? AND (latest_version IS NOT ? OR update_available <> ?)`,
		latest, available, deviceID, latest, available,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// FirmwareRelease is the newest version of one update channel, read from
// MikroTik's release feed.
type FirmwareRelease struct {
	Channel   string     `json:"channel"`
	Version   string     `json:"version"`
	BuildTime *time.Time `json:"build_time"`
	FetchedAt time.Time  `json:"fetched_at"`
}

// UpsertFirmwareRelease stores a successful feed fetch.
func UpsertFirmwareRelease(db *sql.DB, r FirmwareRelease) error {
	var built *int64
	if r.BuildTime != nil && !r.BuildTime.IsZero() {
		v := r.BuildTime.Unix()
		built = &v
	}
	_, err := db.Exec(
		`INSERT INTO firmware_releases (channel, version, build_time, fetched_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(channel) DO UPDATE SET version=excluded.version,
		    build_time=excluded.build_time, fetched_at=excluded.fetched_at`,
		r.Channel, r.Version, built, r.FetchedAt.Unix(),
	)
	return err
}

// ListFirmwareReleases returns every channel fetched so far.
func ListFirmwareReleases(db *sql.DB) ([]FirmwareRelease, error) {
	rows, err := db.Query(`SELECT channel, version, build_time, fetched_at FROM firmware_releases ORDER BY channel`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FirmwareRelease{}
	for rows.Next() {
		var r FirmwareRelease
		var built sql.NullInt64
		var fetched int64
		if err := rows.Scan(&r.Channel, &r.Version, &built, &fetched); err != nil {
			return nil, err
		}
		if built.Valid {
			t := time.Unix(built.Int64, 0).UTC()
			r.BuildTime = &t
		}
		r.FetchedAt = time.Unix(fetched, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func ListFirmwareStatus(db *sql.DB) ([]FirmwareStatus, error) {
	rows, err := db.Query(
		`SELECT fs.id, fs.device_id, fs.channel, fs.installed_version, fs.latest_version,
		        fs.update_available, fs.routerboard_current, fs.routerboard_upgrade,
		        fs.last_checked, fs.updated_at
		 FROM firmware_status fs
		 JOIN devices d ON fs.device_id = d.id
		 ORDER BY d.identity, d.address`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []FirmwareStatus
	for rows.Next() {
		var f FirmwareStatus
		if err := rows.Scan(&f.ID, &f.DeviceID, &f.Channel, &f.InstalledVersion, &f.LatestVersion,
			&f.UpdateAvailable, &f.RouterboardCurrent, &f.RouterboardUpgrade,
			&f.LastChecked, &f.UpdatedAt); err != nil {
			return nil, err
		}
		statuses = append(statuses, f)
	}
	return statuses, rows.Err()
}

func GetFirmwareStatusByDevice(db *sql.DB, deviceID string) (*FirmwareStatus, error) {
	f := &FirmwareStatus{}
	err := db.QueryRow(
		`SELECT id, device_id, channel, installed_version, latest_version,
		        update_available, routerboard_current, routerboard_upgrade,
		        last_checked, updated_at
		 FROM firmware_status WHERE device_id = ?`, deviceID,
	).Scan(&f.ID, &f.DeviceID, &f.Channel, &f.InstalledVersion, &f.LatestVersion,
		&f.UpdateAvailable, &f.RouterboardCurrent, &f.RouterboardUpgrade,
		&f.LastChecked, &f.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return f, nil
}
