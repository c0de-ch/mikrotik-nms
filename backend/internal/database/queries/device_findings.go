package queries

import (
	"database/sql"
	"time"
)

// DeviceFinding is one device_findings row: something a poller noticed about
// a device, deduplicated by (device, kind, detail).
type DeviceFinding struct {
	ID        int64     `json:"id"`
	DeviceID  string    `json:"device_id"`
	Kind      string    `json:"kind"`
	Severity  string    `json:"severity"` // info | warn | critical
	Detail    string    `json:"detail"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Count     int       `json:"count"`
}

// UpsertDeviceFinding records a finding: a new (device, kind, detail) inserts
// a row, a repeat bumps count and last_seen and takes the latest severity.
func UpsertDeviceFinding(db *sql.DB, f *DeviceFinding) error {
	_, err := db.Exec(
		`INSERT INTO device_findings (device_id, kind, severity, detail)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(device_id, kind, detail) DO UPDATE SET
		   severity  = excluded.severity,
		   last_seen = CURRENT_TIMESTAMP,
		   count     = device_findings.count + 1`,
		f.DeviceID, f.Kind, f.Severity, f.Detail,
	)
	return err
}

// ListDeviceFindings returns a device's findings, most recent first.
func ListDeviceFindings(db *sql.DB, deviceID string, limit int) ([]DeviceFinding, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := db.Query(
		`SELECT id, device_id, kind, severity, detail, first_seen, last_seen, count
		 FROM device_findings WHERE device_id = ?
		 ORDER BY last_seen DESC, id DESC LIMIT ?`, deviceID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DeviceFinding, 0, 16)
	for rows.Next() {
		var f DeviceFinding
		if err := rows.Scan(&f.ID, &f.DeviceID, &f.Kind, &f.Severity, &f.Detail, &f.FirstSeen, &f.LastSeen, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteOldDeviceFindings drops findings not seen since cutoff (retention).
func DeleteOldDeviceFindings(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM device_findings WHERE last_seen < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
