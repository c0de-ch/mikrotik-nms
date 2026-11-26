package queries

import (
	"database/sql"
	"errors"
	"net/netip"
	"strings"
	"time"
)

// Flow-export guardrails (migration 031): the block list of senders the
// collector must not decode, and the record of what a managed device that
// must not export was seen and confirmed doing. Times are unix seconds, like
// the other flow tables.

// Sender policies and where a block came from.
const (
	FlowPolicyBlocked = "blocked"

	FlowBlockDeleted = "deleted" // the tombstone of a deleted exporter
	FlowBlockAuto    = "auto"    // a managed switch / AP / CPE / C1 device, blocked by the collector
)

// ErrFlowSenderBlocked is returned when an exporter is created or moved onto
// an address that is blocked.
var ErrFlowSenderBlocked = errors.New("flow sender is blocked")

// FlowSenderPolicy is one flow_sender_policy row: a sender whose datagrams
// are dropped before decode until an admin removes the block.
type FlowSenderPolicy struct {
	Address   string
	Policy    string // FlowPolicyBlocked
	Reason    string
	Source    string // FlowBlockDeleted | FlowBlockAuto
	Name      string
	Kind      string
	DeviceID  string // "" = none
	CreatedBy string
	CreatedAt time.Time
	LastSeen  time.Time // last dropped datagram; zero = none since the block
	Datagrams int64
}

const flowSenderPolicyCols = `address, policy, reason, source, name, kind, device_id, created_by, created_at, last_seen, datagrams`

func scanFlowSenderPolicy(s rowScanner) (FlowSenderPolicy, error) {
	var (
		p                   FlowSenderPolicy
		device              sql.NullString
		createdAt, lastSeen int64
	)
	if err := s.Scan(&p.Address, &p.Policy, &p.Reason, &p.Source, &p.Name, &p.Kind, &device, &p.CreatedBy,
		&createdAt, &lastSeen, &p.Datagrams); err != nil {
		return p, err
	}
	p.DeviceID = device.String
	p.CreatedAt, p.LastSeen = fromUnix(createdAt), fromUnix(lastSeen)
	return p, nil
}

// ListFlowSenderPolicies returns the block list ordered by name, then
// address. Rows whose address is an exporter again (a restore, an old
// binary) are left out: an exporter row wins over a tombstone.
func ListFlowSenderPolicies(db *sql.DB) ([]FlowSenderPolicy, error) {
	rows, err := db.Query(`SELECT ` + flowSenderPolicyCols + ` FROM flow_sender_policy
		WHERE address NOT IN (SELECT address FROM flow_exporters) ORDER BY name, address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlowSenderPolicy{}
	for rows.Next() {
		p, err := scanFlowSenderPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFlowSenderPolicy returns the policy row of an address, or (nil, nil).
func GetFlowSenderPolicy(db *sql.DB, address string) (*FlowSenderPolicy, error) {
	p, err := scanFlowSenderPolicy(db.QueryRow(`SELECT `+flowSenderPolicyCols+` FROM flow_sender_policy WHERE address = ?`, address))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func insertFlowSenderPolicy(x execer, p *FlowSenderPolicy) (sql.Result, error) {
	source := p.Source
	if source == "" {
		source = FlowBlockDeleted
	}
	kind := p.Kind
	if kind == "" {
		kind = "other"
	}
	return x.Exec(`INSERT INTO flow_sender_policy (address, policy, reason, source, name, kind, device_id, created_by, created_at)
		VALUES (?, 'blocked', ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(address) DO NOTHING`,
		p.Address, p.Reason, source, p.Name, kind, nullString(p.DeviceID), p.CreatedBy, unixOf(p.CreatedAt))
}

// BlockFlowSender blocks an address. A block that already exists is kept as
// it is (its reason stands) and created is false.
func BlockFlowSender(db *sql.DB, p *FlowSenderPolicy) (created bool, err error) {
	res, err := insertFlowSenderPolicy(db, p)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UnblockFlowSender removes a block. ErrFlowNotFound when there is none.
func UnblockFlowSender(db *sql.DB, address string) error {
	res, err := db.Exec(`DELETE FROM flow_sender_policy WHERE address = ?`, address)
	return affectedOrNotFound(res, err)
}

// AddFlowSenderDrops adds the datagrams the collector dropped from a blocked
// address since it last told. A missing row is not an error.
func AddFlowSenderDrops(db *sql.DB, address string, datagrams int64, lastSeen time.Time) error {
	ls := unixOf(lastSeen)
	_, err := db.Exec(`UPDATE flow_sender_policy SET datagrams = datagrams + ?,
		last_seen = CASE WHEN ? > last_seen THEN ? ELSE last_seen END WHERE address = ?`, datagrams, ls, ls, address)
	return err
}

// DeleteFlowExporterBlocking deletes an exporter (its interfaces, points and
// templates cascade, its fact rows are left for DeleteOrphanFlows) and leaves
// a tombstone for its address in the same transaction: the sender stays
// blocked until an admin unblocks or re-adds it. ErrFlowNotFound.
func DeleteFlowExporterBlocking(db *sql.DB, id int64, reason, by string, now time.Time) (*FlowSenderPolicy, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	e, err := scanFlowExporter(tx.QueryRow(`SELECT `+flowExporterCols+` FROM flow_exporters WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFlowNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM flow_exporters WHERE id = ?`, id); err != nil {
		return nil, err
	}
	// A stale row for this address (it could not be seen while the exporter
	// existed) must not keep its old reason.
	if _, err := tx.Exec(`DELETE FROM flow_sender_policy WHERE address = ?`, e.Address); err != nil {
		return nil, err
	}
	p := &FlowSenderPolicy{Address: e.Address, Policy: FlowPolicyBlocked, Reason: reason, Source: FlowBlockDeleted,
		Name: e.Name, Kind: e.Kind, DeviceID: e.DeviceID, CreatedBy: by, CreatedAt: now}
	if _, err := insertFlowSenderPolicy(tx, p); err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// CreateFlowExporterChecked is CreateFlowExporter for an admin's request: it
// refuses a blocked address with ErrFlowSenderBlocked unless unblock is set,
// in which case the block is removed in the same transaction.
func CreateFlowExporterChecked(db *sql.DB, e *FlowExporter, unblock bool) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var blocked int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM flow_sender_policy WHERE address = ?`, e.Address).Scan(&blocked); err != nil {
		return 0, err
	}
	if blocked > 0 {
		if !unblock {
			return 0, ErrFlowSenderBlocked
		}
		if _, err := tx.Exec(`DELETE FROM flow_sender_policy WHERE address = ?`, e.Address); err != nil {
			return 0, err
		}
	}
	res, err := tx.Exec(
		`INSERT INTO flow_exporters (name, address, kind, device_id, enabled, auto, sampling_override, nat_addresses)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Name, e.Address, e.Kind, nullString(e.DeviceID), boolInt(e.Enabled), boolInt(e.Auto),
		e.SamplingOverride, formatAddrCSV(e.NATAddresses))
	if isUniqueViolation(err) {
		return 0, ErrFlowExporterExists
	}
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// ---- export checks ----

// Results of the confirmation read of /ip/traffic-flow on a device.
const (
	FlowExportExporting    = "exporting"     // traffic-flow is enabled with an enabled target
	FlowExportNotExporting = "not_exporting" // the device says it does not export
	FlowExportCheckFailed  = "failed"        // the read did not complete
)

// FlowExportCheck is one flow_export_checks row.
type FlowExportCheck struct {
	DeviceID      string
	Address       string    // the sender address of the last sighting
	CollectorPort int       // the collector's listen port at that sighting (0 = unknown)
	FirstSeen     time.Time // first flow datagram from the device's address
	LastSeen      time.Time
	CheckedAt     time.Time // last confirmation read; zero = never
	Result        string    // "" | FlowExport*
	Detail        string    // what the read found, for the operator
	// Targets are the device's enabled traffic-flow targets as the last
	// read found them; empty when it found none or did not complete.
	Targets []netip.AddrPort
}

// FlowExportConfirmationValid is how long a read that found the device
// exporting counts as a confirmation. The read is repeated every hour while
// datagrams keep arriving, so a live export stays confirmed; a confirmation
// nobody renewed — the datagrams stopped, the device is paused — expires
// after two missed reads and a margin. An old "yes" must not make a new,
// possibly forged datagram critical.
const FlowExportConfirmationValid = 2*time.Hour + 15*time.Minute

// Confirmed reports whether the device's own settings confirmed the export
// recently enough to count at now.
func (c FlowExportCheck) Confirmed(now time.Time) bool {
	return c.Result == FlowExportExporting && !c.CheckedAt.IsZero() && now.Sub(c.CheckedAt) <= FlowExportConfirmationValid
}

const flowExportCheckCols = `device_id, address, collector_port, first_seen, last_seen, checked_at, result, detail, targets`

func scanFlowExportCheck(s rowScanner) (FlowExportCheck, error) {
	var (
		c                    FlowExportCheck
		first, last, checked int64
		targets              string
	)
	if err := s.Scan(&c.DeviceID, &c.Address, &c.CollectorPort, &first, &last, &checked, &c.Result, &c.Detail, &targets); err != nil {
		return c, err
	}
	c.FirstSeen, c.LastSeen, c.CheckedAt = fromUnix(first), fromUnix(last), fromUnix(checked)
	for _, f := range strings.Fields(targets) {
		if ap, err := netip.ParseAddrPort(f); err == nil {
			c.Targets = append(c.Targets, ap)
		}
	}
	return c, nil
}

// NoteFlowExportSighting records that a flow datagram arrived from the
// address of a device that must not export, on the collector's listen port.
func NoteFlowExportSighting(db *sql.DB, deviceID, address string, collectorPort int, at time.Time) error {
	ts := unixOf(at)
	_, err := db.Exec(`INSERT INTO flow_export_checks (device_id, address, collector_port, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET address = excluded.address, collector_port = excluded.collector_port,
		  last_seen = CASE WHEN excluded.last_seen > last_seen THEN excluded.last_seen ELSE last_seen END`,
		deviceID, address, collectorPort, ts, ts)
	return err
}

// GetFlowExportCheck returns a device's row, or (nil, nil).
func GetFlowExportCheck(db *sql.DB, deviceID string) (*FlowExportCheck, error) {
	c, err := scanFlowExportCheck(db.QueryRow(`SELECT `+flowExportCheckCols+` FROM flow_export_checks WHERE device_id = ?`, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListFlowExportChecks returns every row, ordered by device.
func ListFlowExportChecks(db *sql.DB) ([]FlowExportCheck, error) {
	rows, err := db.Query(`SELECT ` + flowExportCheckCols + ` FROM flow_export_checks ORDER BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlowExportCheck{}
	for rows.Next() {
		c, err := scanFlowExportCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetFlowExportCheckResult stores what a confirmation read found: the
// result, a line for the operator and the enabled targets. The row exists
// already (a sighting triggers the read); a missing one is created.
func SetFlowExportCheckResult(db *sql.DB, deviceID string, at time.Time, result, detail string, targets []netip.AddrPort) error {
	ts := unixOf(at)
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.IsValid() {
			parts = append(parts, netip.AddrPortFrom(t.Addr().Unmap(), t.Port()).String())
		}
	}
	_, err := db.Exec(`INSERT INTO flow_export_checks (device_id, checked_at, result, detail, targets) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET checked_at = excluded.checked_at, result = excluded.result,
		  detail = excluded.detail, targets = excluded.targets`, deviceID, ts, result, detail, strings.Join(parts, " "))
	return err
}

// exportFindingSeverity is the severity a device's finding carries, read in
// the same statement that writes it so a sighting and a confirmation read
// landing together cannot leave it stale: critical while the device's own
// settings confirmed the export recently enough (FlowExportCheck.Confirmed),
// warn otherwise (unconfirmed). Binds: the confirmation cutoff (unix
// seconds), the device id.
const exportFindingSeverity = `COALESCE((SELECT CASE WHEN result = 'exporting' AND checked_at >= ? THEN 'critical' ELSE 'warn' END
	FROM flow_export_checks WHERE device_id = ?), 'warn')`

// UpsertFlowExportFinding records one sighting at now as a device_findings
// row of the given kind and detail: a repeat bumps count and last_seen.
func UpsertFlowExportFinding(db *sql.DB, deviceID, kind, detail string, now time.Time) error {
	_, err := db.Exec(`INSERT INTO device_findings (device_id, kind, severity, detail)
		VALUES (?, ?, `+exportFindingSeverity+`, ?)
		ON CONFLICT(device_id, kind, detail) DO UPDATE SET
		  severity  = excluded.severity,
		  last_seen = CURRENT_TIMESTAMP,
		  count     = device_findings.count + 1`,
		deviceID, kind, now.Add(-FlowExportConfirmationValid).Unix(), deviceID, detail)
	return err
}

// RefreshFlowExportFindingSeverity sets the severity of a device's findings
// of that kind from its check row, after a confirmation read: no new
// sighting, so count and last_seen stay.
func RefreshFlowExportFindingSeverity(db *sql.DB, deviceID, kind string, now time.Time) error {
	_, err := db.Exec(`UPDATE device_findings SET severity = `+exportFindingSeverity+`
		WHERE device_id = ? AND kind = ?`,
		now.Add(-FlowExportConfirmationValid).Unix(), deviceID, deviceID, kind)
	return err
}

// DeleteFlowExportCheck forgets a device's row (it is no longer seen
// exporting, or it may export after all).
func DeleteFlowExportCheck(db *sql.DB, deviceID string) error {
	_, err := db.Exec(`DELETE FROM flow_export_checks WHERE device_id = ?`, deviceID)
	return err
}
