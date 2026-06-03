package queries

import (
	"database/sql"
	"time"
)

type Device struct {
	ID              string     `json:"id"`
	Address         string     `json:"address"`
	Identity        string     `json:"identity"`
	Platform        string     `json:"platform"`
	Board           string     `json:"board"`
	ROSVersion      string     `json:"ros_version"`
	FirmwareVersion string     `json:"firmware_version"`
	Architecture    string     `json:"architecture"`
	Username        string     `json:"username"`
	PasswordEnc     string     `json:"-"`
	UseTLS          bool       `json:"use_tls"`
	APIPort         int        `json:"api_port"`
	Status          string     `json:"status"`
	CPULoad         *int       `json:"cpu_load"`
	MemoryUsed      *int64     `json:"memory_used"`
	MemoryTotal     *int64     `json:"memory_total"`
	Uptime          *string    `json:"uptime"`
	LastSeen        *time.Time `json:"last_seen"`
	LastError       *string    `json:"last_error"`
	Tags            string     `json:"tags"`
	Notes           string     `json:"notes"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func CreateDevice(db *sql.DB, d *Device) error {
	_, err := db.Exec(
		`INSERT INTO devices (id, address, identity, username, password_enc, use_tls, api_port,
		    platform, board, ros_version, architecture, status, tags, notes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Address, d.Identity, d.Username, encryptSecret(d.PasswordEnc), d.UseTLS, d.APIPort,
		d.Platform, d.Board, d.ROSVersion, d.Architecture, d.Status, d.Tags, d.Notes,
	)
	return err
}

func GetDevice(db *sql.DB, id string) (*Device, error) {
	d := &Device{}
	err := db.QueryRow(
		`SELECT id, address, identity, platform, board, ros_version, firmware_version, architecture,
		        username, password_enc, use_tls, api_port,
		        status, cpu_load, memory_used, memory_total, uptime, last_seen, last_error,
		        tags, notes, created_at, updated_at
		 FROM devices WHERE id = ?`, id,
	).Scan(
		&d.ID, &d.Address, &d.Identity, &d.Platform, &d.Board, &d.ROSVersion, &d.FirmwareVersion, &d.Architecture,
		&d.Username, &d.PasswordEnc, &d.UseTLS, &d.APIPort,
		&d.Status, &d.CPULoad, &d.MemoryUsed, &d.MemoryTotal, &d.Uptime, &d.LastSeen, &d.LastError,
		&d.Tags, &d.Notes, &d.CreatedAt, &d.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	d.PasswordEnc = decryptSecret(d.PasswordEnc)
	return d, nil
}

func ListDevices(db *sql.DB) ([]Device, error) {
	rows, err := db.Query(
		`SELECT id, address, identity, platform, board, ros_version, firmware_version, architecture,
		        username, password_enc, use_tls, api_port,
		        status, cpu_load, memory_used, memory_total, uptime, last_seen, last_error,
		        tags, notes, created_at, updated_at
		 FROM devices ORDER BY identity, address`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(
			&d.ID, &d.Address, &d.Identity, &d.Platform, &d.Board, &d.ROSVersion, &d.FirmwareVersion, &d.Architecture,
			&d.Username, &d.PasswordEnc, &d.UseTLS, &d.APIPort,
			&d.Status, &d.CPULoad, &d.MemoryUsed, &d.MemoryTotal, &d.Uptime, &d.LastSeen, &d.LastError,
			&d.Tags, &d.Notes, &d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			return nil, err
		}
		d.PasswordEnc = decryptSecret(d.PasswordEnc)
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func UpdateDevice(db *sql.DB, d *Device) error {
	_, err := db.Exec(
		`UPDATE devices SET address=?, identity=?, username=?, password_enc=?, use_tls=?, api_port=?,
		        tags=?, notes=?, updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		d.Address, d.Identity, d.Username, encryptSecret(d.PasswordEnc), d.UseTLS, d.APIPort,
		d.Tags, d.Notes, d.ID,
	)
	return err
}

// UpdateDeviceAddressIfUnchanged atomically re-points a device's address only if
// it still equals oldAddr at write time. Returns true when a row was updated and
// false when the address changed concurrently (a manual edit wins). Avoids a
// read-modify-write race and never rewrites the encrypted credential column.
func UpdateDeviceAddressIfUnchanged(db *sql.DB, id, oldAddr, newAddr string) (bool, error) {
	res, err := db.Exec(
		`UPDATE devices SET address=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND address=?`,
		newAddr, id, oldAddr,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func UpdateDeviceHealth(db *sql.DB, id string, status string, cpuLoad *int, memUsed, memTotal *int64, uptime *string, lastError *string) error {
	_, err := db.Exec(
		`UPDATE devices SET status=?, cpu_load=?, memory_used=?, memory_total=?, uptime=?,
		        last_seen=CURRENT_TIMESTAMP, last_error=?, updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		status, cpuLoad, memUsed, memTotal, uptime, lastError, id,
	)
	return err
}

// MarkDeviceOnline records a successful liveness check (a reachable "ping")
// without touching the cached cpu/memory/uptime/version fields. The frequent
// liveness poll only proves the device is up; the heavier info poll refreshes
// the stats on its own (longer) cycle, so this keeps the last-known values
// intact instead of blanking them every health tick.
func MarkDeviceOnline(db *sql.DB, id string) error {
	_, err := db.Exec(
		`UPDATE devices SET status='online', last_seen=CURRENT_TIMESTAMP,
		        last_error=NULL, updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		id,
	)
	return err
}

// MarkDeviceUnreachable records the outcome of a failed poll WITHOUT touching
// last_seen, so last_seen keeps meaning "last successful contact". The health
// poller decides whether status should be "offline" (grace period elapsed) or
// stay "online" (still within the grace window).
func MarkDeviceUnreachable(db *sql.DB, id, status string, lastError *string) error {
	_, err := db.Exec(
		`UPDATE devices SET status=?, last_error=?, updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		status, lastError, id,
	)
	return err
}

func UpdateDeviceInfo(db *sql.DB, id, platform, board, rosVersion, fwVersion, arch string) error {
	_, err := db.Exec(
		`UPDATE devices SET platform=?, board=?, ros_version=?, firmware_version=?, architecture=?,
		        updated_at=CURRENT_TIMESTAMP
		 WHERE id=?`,
		platform, board, rosVersion, fwVersion, arch, id,
	)
	return err
}

func DeleteDevice(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
