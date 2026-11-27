package queries

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// FindingSourceAudit marks the findings the hygiene audit holds: a pass saw
// the condition and wrote the row with its explanation and fix, and no pass
// has seen it gone since. Such a finding does not age: it stands until a
// pass looks again and no longer finds it. For the audit's own kinds that
// pass removes the row (DeleteAuditFindingsExcept); for a kind it shares
// with another watcher — an API session that is not the NMS's, which the
// login watch records too — it only lets go of it (ReleaseAuditFindings),
// and the row then counts by when it was last seen.
const FindingSourceAudit = "audit"

// FindingFix is one step of a finding's suggested fix: advice for an
// operator, never applied by the NMS.
type FindingFix struct {
	Text string `json:"text"`
	// Commands are RouterOS commands for the device's terminal.
	Commands []string `json:"commands,omitempty"`
	// ReadOnly: the commands only print.
	ReadOnly bool `json:"read_only,omitempty"`
	// Window: the step interrupts traffic — plan a maintenance window.
	Window bool `json:"window,omitempty"`
}

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
	// What the hygiene audit adds to its findings (empty for the others):
	// a title, a plain explanation and the suggested fix. Source is
	// FindingSourceAudit for them.
	Title       string       `json:"title,omitempty"`
	Explanation string       `json:"explanation,omitempty"`
	Fix         []FindingFix `json:"fix,omitempty"`
	Source      string       `json:"source,omitempty"`
}

// FindingKey identifies a finding of a device.
type FindingKey struct {
	Kind, Detail string
}

const findingColumns = `id, device_id, kind, severity, detail, first_seen, last_seen, count,
	COALESCE(title, ''), COALESCE(explanation, ''), COALESCE(fix, ''), COALESCE(source, '')`

func scanFindings(rows *sql.Rows) ([]DeviceFinding, error) {
	out := make([]DeviceFinding, 0, 16)
	for rows.Next() {
		var f DeviceFinding
		var fix string
		if err := rows.Scan(&f.ID, &f.DeviceID, &f.Kind, &f.Severity, &f.Detail, &f.FirstSeen, &f.LastSeen, &f.Count,
			&f.Title, &f.Explanation, &fix, &f.Source); err != nil {
			return nil, err
		}
		if strings.TrimSpace(fix) != "" {
			_ = json.Unmarshal([]byte(fix), &f.Fix) // a damaged fix is no fix
		}
		out = append(out, f)
	}
	return out, rows.Err()
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

// UpsertAuditFinding records a finding of the hygiene audit as seen at now,
// with its title, explanation and fix: a new (device, kind, detail) inserts a
// row, a repeat bumps count and last_seen and takes the latest severity and
// text.
func UpsertAuditFinding(db *sql.DB, f *DeviceFinding, now time.Time) error {
	fix := ""
	if len(f.Fix) > 0 {
		b, err := json.Marshal(f.Fix)
		if err != nil {
			return err
		}
		fix = string(b)
	}
	at := sqlTime(now) // the text CURRENT_TIMESTAMP writes for the other findings
	_, err := db.Exec(
		`INSERT INTO device_findings (device_id, kind, severity, detail, first_seen, last_seen, title, explanation, fix, source)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(device_id, kind, detail) DO UPDATE SET
		   severity    = excluded.severity,
		   last_seen   = excluded.last_seen,
		   count       = device_findings.count + 1,
		   title       = excluded.title,
		   explanation = excluded.explanation,
		   fix         = excluded.fix,
		   source      = excluded.source`,
		f.DeviceID, f.Kind, f.Severity, f.Detail, at, at, f.Title, f.Explanation, fix, FindingSourceAudit,
	)
	return err
}

// DeleteAuditFindingsExcept removes a device's audit findings of the given
// kinds that are not among keep: a pass evaluated those kinds and no longer
// found them. Findings of other kinds, of other sources and of other
// devices are never touched.
func DeleteAuditFindingsExcept(db *sql.DB, deviceID string, kinds []string, keep []FindingKey) (int64, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	kept := make(map[FindingKey]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}
	q := `SELECT id, kind, detail FROM device_findings WHERE device_id = ? AND source = ? AND kind IN (?` +
		strings.Repeat(",?", len(kinds)-1) + `)`
	args := []any{deviceID, FindingSourceAudit}
	for _, k := range kinds {
		args = append(args, k)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return 0, err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		var key FindingKey
		if err := rows.Scan(&id, &key.Kind, &key.Detail); err != nil {
			rows.Close()
			return 0, err
		}
		if !kept[key] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int64
	for _, id := range gone {
		res, err := db.Exec(`DELETE FROM device_findings WHERE id = ?`, id)
		if err != nil {
			return n, err
		}
		affected, _ := res.RowsAffected()
		n += affected
	}
	return n, nil
}

// ReleaseAuditFindings ends the audit's hold on a device's findings of a
// kind it shares with another watcher, except the ones among keep (their
// details): a pass looked and no longer saw them. The rows stay — the other
// watcher may be seeing the same thing — and from then on count by their
// last_seen, like any row the audit never held. It reports how many rows
// were let go.
func ReleaseAuditFindings(db *sql.DB, deviceID, kind string, keep []string) (int64, error) {
	q := `UPDATE device_findings SET source = NULL WHERE device_id = ? AND kind = ? AND source = ?`
	args := []any{deviceID, kind, FindingSourceAudit}
	if len(keep) > 0 {
		q += ` AND detail NOT IN (?` + strings.Repeat(",?", len(keep)-1) + `)`
		for _, d := range keep {
			args = append(args, d)
		}
	}
	res, err := db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListDeviceFindings returns a device's findings, most recent first.
func ListDeviceFindings(db *sql.DB, deviceID string, limit int) ([]DeviceFinding, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := db.Query(
		`SELECT `+findingColumns+`
		 FROM device_findings WHERE device_id = ?
		 ORDER BY last_seen DESC, id DESC LIMIT ?`, deviceID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFindings(rows)
}

// ListAuditFindings returns every device's hygiene findings, most severe
// first.
func ListAuditFindings(db *sql.DB) ([]DeviceFinding, error) {
	rows, err := db.Query(
		`SELECT `+findingColumns+`
		 FROM device_findings WHERE source = ?
		 ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'warn' THEN 1 ELSE 2 END, device_id, kind, detail`,
		FindingSourceAudit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFindings(rows)
}

// DeleteOldDeviceFindings drops findings not seen since cutoff (retention).
func DeleteOldDeviceFindings(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM device_findings WHERE last_seen < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
