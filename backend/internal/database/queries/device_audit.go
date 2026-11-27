package queries

import (
	"database/sql"
	"errors"
	"time"
)

// Hygiene audit storage (P0-10): the configuration snapshots (device_audit),
// the passes that were run (device_audit_runs) and the CPU watch after a
// flow exporter is accepted (flow_export_watches). Times are unix seconds.

// Kinds of pass.
const (
	AuditKindAudit    = "audit"
	AuditKindDiagnose = "diagnose"
)

// States of a pass.
const (
	AuditRunning  = "running"
	AuditComplete = "complete" // every read was answered
	AuditPartial  = "partial"  // a read failed: what was read was judged, the rest is unknown
	AuditAborted  = "aborted"  // stopped before the end (a pause, no session, a restart)
)

// DeviceAudit is one device_audit row: a device's configuration as an audit
// read it, stored when it differed from the one before.
type DeviceAudit struct {
	ID         int64
	DeviceID   string
	TakenAt    time.Time
	Hash       string
	ROSVersion string
	Snapshot   string // JSON
	Findings   string // JSON
}

// InsertDeviceAudit stores a snapshot.
func InsertDeviceAudit(db *sql.DB, a *DeviceAudit) error {
	findings := a.Findings
	if findings == "" {
		findings = "[]"
	}
	_, err := db.Exec(`INSERT INTO device_audit (device_id, taken_at, hash, ros_version, snapshot, findings)
		VALUES (?, ?, ?, ?, ?, ?)`, a.DeviceID, a.TakenAt.Unix(), a.Hash, a.ROSVersion, a.Snapshot, findings)
	return err
}

// LatestDeviceAudit returns a device's newest snapshot, nil when it has none.
func LatestDeviceAudit(db *sql.DB, deviceID string) (*DeviceAudit, error) {
	var a DeviceAudit
	var taken int64
	err := db.QueryRow(`SELECT id, device_id, taken_at, hash, ros_version, snapshot, findings
		FROM device_audit WHERE device_id = ? ORDER BY taken_at DESC, id DESC LIMIT 1`, deviceID).
		Scan(&a.ID, &a.DeviceID, &taken, &a.Hash, &a.ROSVersion, &a.Snapshot, &a.Findings)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.TakenAt = time.Unix(taken, 0).UTC()
	return &a, nil
}

// DeleteOldDeviceAudits drops snapshots taken before cutoff, except each
// device's newest one: that is its current configuration however long ago
// it last changed.
func DeleteOldDeviceAudits(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM device_audit WHERE taken_at < ?
		AND id NOT IN (SELECT MAX(id) FROM device_audit GROUP BY device_id)`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AuditRun is one device_audit_runs row: a pass of the audit or the
// diagnose pass.
type AuditRun struct {
	ID          int64     `json:"id"`
	DeviceID    string    `json:"device_id"`
	Kind        string    `json:"kind"`   // audit | diagnose
	Reason      string    `json:"reason"` // scheduled | admin | the alert rule that asked
	AlertID     int64     `json:"alert_id,omitempty"`
	RequestedBy string    `json:"requested_by,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"` // zero while running
	Status      string    `json:"status"`
	Reads       int       `json:"reads"`
	FailedReads int       `json:"failed_reads"`
	Hash        string    `json:"-"`
	Detail      string    `json:"detail,omitempty"`
	Runtime     string    `json:"-"`
	Findings    string    `json:"-"` // JSON
}

const auditRunColumns = `id, device_id, kind, reason, alert_id, requested_by, started_at, finished_at, status,
	reads, failed_reads, hash, detail, runtime, findings`

func scanAuditRun(row rowScanner) (AuditRun, error) {
	var r AuditRun
	var started, finished int64
	if err := row.Scan(&r.ID, &r.DeviceID, &r.Kind, &r.Reason, &r.AlertID, &r.RequestedBy, &started, &finished, &r.Status,
		&r.Reads, &r.FailedReads, &r.Hash, &r.Detail, &r.Runtime, &r.Findings); err != nil {
		return r, err
	}
	r.StartedAt = time.Unix(started, 0).UTC()
	if finished > 0 {
		r.FinishedAt = time.Unix(finished, 0).UTC()
	}
	return r, nil
}

func oneAuditRun(db *sql.DB, query string, args ...any) (*AuditRun, error) {
	r, err := scanAuditRun(db.QueryRow(query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// InsertAuditRun records the start of a pass and returns its id.
func InsertAuditRun(db *sql.DB, r *AuditRun) (int64, error) {
	res, err := db.Exec(`INSERT INTO device_audit_runs (device_id, kind, reason, alert_id, requested_by, started_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, r.DeviceID, r.Kind, r.Reason, r.AlertID, r.RequestedBy, r.StartedAt.Unix(), AuditRunning)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishAuditRun records how the pass r.ID ended.
func FinishAuditRun(db *sql.DB, r *AuditRun) error {
	findings := r.Findings
	if findings == "" {
		findings = "[]"
	}
	_, err := db.Exec(`UPDATE device_audit_runs SET finished_at = ?, status = ?, reads = ?, failed_reads = ?, hash = ?,
		detail = ?, runtime = ?, findings = ? WHERE id = ?`,
		r.FinishedAt.Unix(), r.Status, r.Reads, r.FailedReads, r.Hash, r.Detail, r.Runtime, findings, r.ID)
	return err
}

// LatestAuditRun returns the pass of that kind a device started last —
// running, finished or aborted — or nil. Its start is what the hourly budget
// counts from.
func LatestAuditRun(db *sql.DB, deviceID, kind string) (*AuditRun, error) {
	return oneAuditRun(db, `SELECT `+auditRunColumns+` FROM device_audit_runs
		WHERE device_id = ? AND kind = ? ORDER BY started_at DESC, id DESC LIMIT 1`, deviceID, kind)
}

// LatestFinishedAuditRun returns the newest pass of that kind that judged
// something (complete or partial), or nil.
func LatestFinishedAuditRun(db *sql.DB, deviceID, kind string) (*AuditRun, error) {
	return oneAuditRun(db, `SELECT `+auditRunColumns+` FROM device_audit_runs
		WHERE device_id = ? AND kind = ? AND status IN (?, ?) ORDER BY finished_at DESC, id DESC LIMIT 1`,
		deviceID, kind, AuditComplete, AuditPartial)
}

// ListLatestDiagnoseRuns returns, per device, its newest diagnose pass that
// judged something (complete or partial), however old: the alert rules pick
// the "likely cause" from it — the pass that answered an alert is its cause
// for as long as the alert stays open.
func ListLatestDiagnoseRuns(db *sql.DB) (map[string]AuditRun, error) {
	rows, err := db.Query(`SELECT `+auditRunColumns+` FROM device_audit_runs r
		WHERE r.kind = ?1 AND r.status IN (?2, ?3) AND NOT EXISTS (
			SELECT 1 FROM device_audit_runs n
			WHERE n.device_id = r.device_id AND n.kind = ?1 AND n.status IN (?2, ?3)
			  AND (n.finished_at > r.finished_at OR (n.finished_at = r.finished_at AND n.id > r.id)))`,
		AuditKindDiagnose, AuditComplete, AuditPartial)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AuditRun{}
	for rows.Next() {
		r, err := scanAuditRun(rows)
		if err != nil {
			return nil, err
		}
		out[r.DeviceID] = r
	}
	return out, rows.Err()
}

// ListLastAuditTimes returns when each device last finished an audit that
// judged something.
func ListLastAuditTimes(db *sql.DB) (map[string]time.Time, error) {
	rows, err := db.Query(`SELECT device_id, MAX(finished_at) FROM device_audit_runs
		WHERE kind = ? AND status IN (?, ?) GROUP BY device_id`, AuditKindAudit, AuditComplete, AuditPartial)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at int64
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		if at > 0 {
			out[id] = time.Unix(at, 0).UTC()
		}
	}
	return out, rows.Err()
}

// LatestAuditRuntime returns the runtime (JSON) the newest pass of a device
// stored, and when that pass finished; "" when none did.
func LatestAuditRuntime(db *sql.DB, deviceID string) (runtime string, at time.Time, err error) {
	var finished int64
	err = db.QueryRow(`SELECT runtime, finished_at FROM device_audit_runs
		WHERE device_id = ? AND runtime != '' AND status IN (?, ?) ORDER BY finished_at DESC, id DESC LIMIT 1`,
		deviceID, AuditComplete, AuditPartial).Scan(&runtime, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return runtime, time.Unix(finished, 0).UTC(), nil
}

// AbortRunningAuditRuns closes every pass still marked running — after a
// restart nothing is running — and reports how many there were.
func AbortRunningAuditRuns(db *sql.DB, now time.Time, detail string) (int64, error) {
	res, err := db.Exec(`UPDATE device_audit_runs SET status = ?, finished_at = ?, detail = ? WHERE status = ?`,
		AuditAborted, now.Unix(), detail, AuditRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOldAuditRuns drops passes started before cutoff (retention).
func DeleteOldAuditRuns(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM device_audit_runs WHERE started_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FlowExportWatch is one flow_export_watches row: a managed device whose
// flow exporter was accepted at AcceptedAt, and its CPU baseline then.
type FlowExportWatch struct {
	DeviceID   string
	ExporterID int64
	Address    string
	// CollectorPort is the collector's listen port when the exporter was
	// accepted (0 = unknown), for the rollback text.
	CollectorPort int
	AcceptedAt    time.Time
	// Baseline is the device's average quiet-window CPU (percent) over the
	// 24 h before AcceptedAt; nil = no data.
	Baseline *float64
}

// StartFlowExportWatch starts (or restarts) the CPU watch of a device.
func StartFlowExportWatch(db *sql.DB, w *FlowExportWatch) error {
	var baseline any
	if w.Baseline != nil {
		baseline = *w.Baseline
	}
	_, err := db.Exec(`INSERT INTO flow_export_watches (device_id, exporter_id, address, collector_port, accepted_at, baseline)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET exporter_id = excluded.exporter_id, address = excluded.address,
		  collector_port = excluded.collector_port, accepted_at = excluded.accepted_at, baseline = excluded.baseline`,
		w.DeviceID, w.ExporterID, w.Address, w.CollectorPort, w.AcceptedAt.Unix(), baseline)
	return err
}

// ListFlowExportWatches returns every watch, by device.
func ListFlowExportWatches(db *sql.DB) ([]FlowExportWatch, error) {
	rows, err := db.Query(`SELECT device_id, exporter_id, address, collector_port, accepted_at, baseline
		FROM flow_export_watches ORDER BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlowExportWatch{}
	for rows.Next() {
		var w FlowExportWatch
		var accepted int64
		var baseline sql.NullFloat64
		if err := rows.Scan(&w.DeviceID, &w.ExporterID, &w.Address, &w.CollectorPort, &accepted, &baseline); err != nil {
			return nil, err
		}
		w.AcceptedAt = time.Unix(accepted, 0).UTC()
		if baseline.Valid {
			v := baseline.Float64
			w.Baseline = &v
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteOldFlowExportWatches drops the watches accepted before cutoff.
func DeleteOldFlowExportWatches(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM flow_export_watches WHERE accepted_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeviceCPUBaseline is a device's average quiet-window CPU (percent) over
// the device-minutes in [from, to) that have a sample, and how many minutes
// that was (0 = no data).
func DeviceCPUBaseline(db *sql.DB, deviceID string, from, to time.Time) (avg float64, minutes int, err error) {
	var a sql.NullFloat64
	err = db.QueryRow(`SELECT AVG(cpu_sum * 1.0 / cpu_samples), COUNT(*) FROM device_minutes
		WHERE device_id = ? AND bucket >= ? AND bucket < ? AND cpu_samples > 0`,
		deviceID, from.Unix(), to.Unix()).Scan(&a, &minutes)
	if err != nil {
		return 0, 0, err
	}
	return a.Float64, minutes, nil
}
