package queries

import (
	"database/sql"
	"time"
)

// AuditEntry is one row of audit_log: an operator action on a device
// (polling pause, profile or class change, …) with who did it and when.
type AuditEntry struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	UserID   string    `json:"user_id"`
	Username string    `json:"username"`
	Action   string    `json:"action"`
	DeviceID string    `json:"device_id"`
	Detail   string    `json:"detail"`
}

// InsertAuditLog records an action. UserID/Username may be empty for actions
// the system takes on its own; DeviceID may be empty for global actions.
func InsertAuditLog(db *sql.DB, e *AuditEntry) (int64, error) {
	at := e.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := db.Exec(
		`INSERT INTO audit_log (at, user_id, username, action, device_id, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		at, e.UserID, e.Username, e.Action, e.DeviceID, e.Detail,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAuditLog returns a device's most recent entries, newest first (every
// device's when deviceID is empty). limit ≤ 0 means 50; the cap is 500.
func ListAuditLog(db *sql.DB, deviceID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q := `SELECT id, at, COALESCE(user_id, ''), COALESCE(username, ''), action,
	             COALESCE(device_id, ''), COALESCE(detail, '')
	      FROM audit_log`
	args := []any{}
	if deviceID != "" {
		q += ` WHERE device_id = ?`
		args = append(args, deviceID)
	}
	q += ` ORDER BY at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.UserID, &e.Username, &e.Action, &e.DeviceID, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
