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

func UpsertFirmwareStatus(db *sql.DB, f *FirmwareStatus) error {
	_, err := db.Exec(
		`INSERT INTO firmware_status (id, device_id, channel, installed_version, latest_version,
		    update_available, routerboard_current, routerboard_upgrade, last_checked, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id) DO UPDATE SET
		    channel=excluded.channel, installed_version=excluded.installed_version,
		    latest_version=excluded.latest_version, update_available=excluded.update_available,
		    routerboard_current=excluded.routerboard_current, routerboard_upgrade=excluded.routerboard_upgrade,
		    last_checked=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP`,
		f.ID, f.DeviceID, f.Channel, f.InstalledVersion, f.LatestVersion,
		f.UpdateAvailable, f.RouterboardCurrent, f.RouterboardUpgrade,
	)
	return err
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
