package queries

import (
	"database/sql"
	"errors"
	"time"
)

// Alert states.
const (
	AlertFiring   = "firing"
	AlertAcked    = "acked"
	AlertResolved = "resolved"
)

// Alert is one alerts row. Times are unix seconds in the table; zero time /
// nil pointer = not set.
type Alert struct {
	ID             int64      `json:"id"`
	Rule           string     `json:"rule"`
	Key            string     `json:"key"`
	DeviceID       string     `json:"device_id,omitempty"`
	Severity       string     `json:"severity"`
	State          string     `json:"state"`
	FirstAt        time.Time  `json:"first_at"`
	LastAt         time.Time  `json:"last_at"`
	LastNotifiedAt *time.Time `json:"last_notified_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	AckedBy        string     `json:"acked_by,omitempty"`
	AckedAt        *time.Time `json:"acked_at,omitempty"`
	Confirmed      bool       `json:"confirmed"`
	// Payload is the rule's JSON object ({"summary": …, …}).
	Payload string `json:"-"`
}

const alertColumns = `id, rule, key, COALESCE(device_id, ''), severity, state, first_at, last_at,
	last_notified_at, resolved_at, COALESCE(acked_by, ''), acked_at, confirmed, payload`

func unixPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

func scanAlert(row rowScanner) (Alert, error) {
	var a Alert
	var first, last int64
	var notified, resolved, acked sql.NullInt64
	if err := row.Scan(&a.ID, &a.Rule, &a.Key, &a.DeviceID, &a.Severity, &a.State, &first, &last,
		&notified, &resolved, &a.AckedBy, &acked, &a.Confirmed, &a.Payload); err != nil {
		return a, err
	}
	a.FirstAt, a.LastAt = time.Unix(first, 0).UTC(), time.Unix(last, 0).UTC()
	a.LastNotifiedAt, a.ResolvedAt, a.AckedAt = unixPtr(notified), unixPtr(resolved), unixPtr(acked)
	return a, nil
}

func listAlerts(db *sql.DB, query string, args ...any) ([]Alert, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Alert, 0, 16)
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListOpenAlerts returns every alert that is not resolved, oldest first.
func ListOpenAlerts(db *sql.DB) ([]Alert, error) {
	return listAlerts(db, `SELECT `+alertColumns+` FROM alerts WHERE state <> 'resolved' ORDER BY first_at, id`)
}

// ListRecentAlerts returns the open alerts plus the ones resolved since
// the cutoff, newest activity first.
func ListRecentAlerts(db *sql.DB, resolvedSince time.Time, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return listAlerts(db, `SELECT `+alertColumns+` FROM alerts
		WHERE state <> 'resolved' OR resolved_at >= ?
		ORDER BY (state = 'resolved'), last_at DESC, id DESC LIMIT ?`, resolvedSince.Unix(), limit)
}

// GetAlert returns one alert.
func GetAlert(db *sql.DB, id int64) (Alert, error) {
	return scanAlert(db.QueryRow(`SELECT `+alertColumns+` FROM alerts WHERE id = ?`, id))
}

// InsertAlert opens an alert in state firing and returns its id. The unique
// index on open (rule, key) makes a second open alert for the same key fail.
func InsertAlert(db *sql.DB, a *Alert) (int64, error) {
	res, err := db.Exec(`INSERT INTO alerts (rule, key, device_id, severity, state, first_at, last_at, confirmed, payload)
		VALUES (?, ?, NULLIF(?, ''), ?, 'firing', ?, ?, ?, ?)`,
		a.Rule, a.Key, a.DeviceID, a.Severity, a.FirstAt.Unix(), a.LastAt.Unix(), a.Confirmed, a.Payload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TouchAlert records that an open alert's condition still holds.
func TouchAlert(db *sql.DB, id int64, severity, payload string, at time.Time) error {
	_, err := db.Exec(`UPDATE alerts SET last_at = ?, severity = ?, payload = ? WHERE id = ? AND state <> 'resolved'`,
		at.Unix(), severity, payload, id)
	return err
}

// SetAlertConfirmed records whether an open alert rests on confirmed
// evidence (confirmed = 0: raised on something that can be forged).
func SetAlertConfirmed(db *sql.DB, id int64, confirmed bool) error {
	_, err := db.Exec(`UPDATE alerts SET confirmed = ? WHERE id = ? AND state <> 'resolved'`, confirmed, id)
	return err
}

// EscalateAlert makes an open alert fire again after its severity rose: an
// acknowledgement of the lower severity is dropped and the alert counts as
// not notified yet, so the next notification pass delivers it.
func EscalateAlert(db *sql.DB, id int64) error {
	_, err := db.Exec(`UPDATE alerts SET state = 'firing', acked_by = NULL, acked_at = NULL, last_notified_at = NULL
		WHERE id = ? AND state <> 'resolved'`, id)
	return err
}

// ResolveAlert closes an open alert.
func ResolveAlert(db *sql.DB, id int64, at time.Time) error {
	_, err := db.Exec(`UPDATE alerts SET state = 'resolved', resolved_at = ? WHERE id = ? AND state <> 'resolved'`, at.Unix(), id)
	return err
}

// MarkAlertNotified records a delivered notification.
func MarkAlertNotified(db *sql.DB, id int64, at time.Time) error {
	_, err := db.Exec(`UPDATE alerts SET last_notified_at = ? WHERE id = ?`, at.Unix(), id)
	return err
}

// ErrAlertNotOpen is returned by AckAlert for a missing or resolved alert.
var ErrAlertNotOpen = errors.New("alert is not open")

// AckAlert marks a firing alert as acknowledged by a user: it stays open
// (and resolves by itself) but is no longer re-notified.
func AckAlert(db *sql.DB, id int64, username string, at time.Time) error {
	res, err := db.Exec(`UPDATE alerts SET state = 'acked', acked_by = ?, acked_at = ? WHERE id = ? AND state = 'firing'`,
		username, at.Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var state string
		if err := db.QueryRow(`SELECT state FROM alerts WHERE id = ?`, id).Scan(&state); err != nil || state != AlertAcked {
			return ErrAlertNotOpen
		}
	}
	return nil
}

// DeleteResolvedAlertsBefore prunes resolved alerts older than the cutoff.
func DeleteResolvedAlertsBefore(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM alerts WHERE state = 'resolved' AND resolved_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AlertSilence stops notifications for a device, a rule or the whole site
// until it expires.
type AlertSilence struct {
	ID        int64     `json:"id"`
	Scope     string    `json:"scope"` // site | device | rule
	Target    string    `json:"target"`
	Until     time.Time `json:"until"`
	Reason    string    `json:"reason"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// ListAlertSilences returns the silences still in force at now.
func ListAlertSilences(db *sql.DB, now time.Time) ([]AlertSilence, error) {
	rows, err := db.Query(`SELECT id, scope, target, until, COALESCE(reason, ''), created_by, created_at
		FROM alert_silences WHERE until > ? ORDER BY until`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AlertSilence, 0, 4)
	for rows.Next() {
		var s AlertSilence
		var until, created int64
		if err := rows.Scan(&s.ID, &s.Scope, &s.Target, &until, &s.Reason, &s.CreatedBy, &created); err != nil {
			return nil, err
		}
		s.Until, s.CreatedAt = time.Unix(until, 0).UTC(), time.Unix(created, 0).UTC()
		out = append(out, s)
	}
	return out, rows.Err()
}

// InsertAlertSilence adds a silence and returns its id.
func InsertAlertSilence(db *sql.DB, s *AlertSilence) (int64, error) {
	res, err := db.Exec(`INSERT INTO alert_silences (scope, target, until, reason, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		s.Scope, s.Target, s.Until.Unix(), s.Reason, s.CreatedBy, s.CreatedAt.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeleteAlertSilence removes a silence (ending it early).
func DeleteAlertSilence(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM alert_silences WHERE id = ?`, id)
	return err
}

// DeleteExpiredAlertSilences prunes silences that ended before the cutoff.
func DeleteExpiredAlertSilences(db *sql.DB, cutoff time.Time) error {
	_, err := db.Exec(`DELETE FROM alert_silences WHERE until < ?`, cutoff.Unix())
	return err
}

// NotificationChannel is where alert notifications go. Target is the e-mail
// address or webhook URL in the clear — encrypted at rest, and crypto.Unreadable
// when it cannot be decrypted.
type NotificationChannel struct {
	ID          int64
	Name        string
	Kind        string // smtp | webhook
	Target      string
	MinSeverity string
	Enabled     bool
	CreatedAt   time.Time
}

// ListNotificationChannels returns every channel, oldest first.
func ListNotificationChannels(db *sql.DB) ([]NotificationChannel, error) {
	rows, err := db.Query(`SELECT id, name, kind, target_enc, min_severity, enabled, created_at FROM notification_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NotificationChannel, 0, 4)
	for rows.Next() {
		var c NotificationChannel
		var created int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Target, &c.MinSeverity, &c.Enabled, &created); err != nil {
			return nil, err
		}
		c.Target = decryptSecret(c.Target)
		c.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetNotificationChannel returns one channel.
func GetNotificationChannel(db *sql.DB, id int64) (NotificationChannel, error) {
	var c NotificationChannel
	var created int64
	err := db.QueryRow(`SELECT id, name, kind, target_enc, min_severity, enabled, created_at FROM notification_channels WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.Kind, &c.Target, &c.MinSeverity, &c.Enabled, &created)
	if err != nil {
		return c, err
	}
	c.Target = decryptSecret(c.Target)
	c.CreatedAt = time.Unix(created, 0).UTC()
	return c, nil
}

// InsertNotificationChannel stores a channel, its target encrypted (it fails
// like a device password when that is not possible), and returns its id.
func InsertNotificationChannel(db *sql.DB, c *NotificationChannel) (int64, error) {
	target, err := encryptSecret(c.Target)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO notification_channels (name, kind, target_enc, min_severity, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		c.Name, c.Kind, target, c.MinSeverity, c.Enabled, c.CreatedAt.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateNotificationChannel saves a channel's name, threshold and enabled
// flag, and its target when newTarget is not empty.
func UpdateNotificationChannel(db *sql.DB, c *NotificationChannel, newTarget string) error {
	if newTarget != "" {
		target, err := encryptSecret(newTarget)
		if err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE notification_channels SET target_enc = ? WHERE id = ?`, target, c.ID); err != nil {
			return err
		}
	}
	_, err := db.Exec(`UPDATE notification_channels SET name = ?, min_severity = ?, enabled = ? WHERE id = ?`,
		c.Name, c.MinSeverity, c.Enabled, c.ID)
	return err
}

// DeleteNotificationChannel removes a channel.
func DeleteNotificationChannel(db *sql.DB, id int64) error {
	_, err := db.Exec(`DELETE FROM notification_channels WHERE id = ?`, id)
	return err
}

// ListCurrentDeviceFindings returns the findings of a kind last seen since
// the cutoff, across all devices.
func ListCurrentDeviceFindings(db *sql.DB, kind string, since time.Time) ([]DeviceFinding, error) {
	rows, err := db.Query(
		`SELECT id, device_id, kind, severity, detail, first_seen, last_seen, count
		 FROM device_findings WHERE kind = ? AND last_seen >= ?
		 ORDER BY device_id, detail`, kind, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DeviceFinding, 0, 4)
	for rows.Next() {
		var f DeviceFinding
		if err := rows.Scan(&f.ID, &f.DeviceID, &f.Kind, &f.Severity, &f.Detail, &f.FirstSeen, &f.LastSeen, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
