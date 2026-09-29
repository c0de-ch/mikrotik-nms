package queries

import (
	"bytes"
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Flow tables (migration 022) deliberately differ from the Phase 1 port-stats
// tables: buckets and every *_seen / *_at column are INTEGER unix seconds and
// addresses are 4- or 16-byte BLOBs (the empty blob is the folded "other"
// row). Nothing here binds a time.Time; conversions go through unixOf /
// fromUnix.

// Errors returned by the flow queries.
var (
	ErrFlowExporterExists = errors.New("flow exporter address already exists") // UNIQUE(address)
	ErrFlowPointExists    = errors.New("flow point already exists")            // native/derived unique indexes
	ErrFlowNotFound       = errors.New("flow object not found")                // update/delete of a missing id
)

// FlowIPBlob converts an address to its stored form: 4 bytes for IPv4 (a
// v4-mapped IPv6 address is unmapped first), 16 for IPv6, and the empty,
// non-nil blob for the invalid Addr (the folded "other" row). A nil slice
// would bind NULL and violate NOT NULL.
func FlowIPBlob(a netip.Addr) []byte {
	if !a.IsValid() {
		return []byte{}
	}
	return a.Unmap().AsSlice()
}

// FlowBlobIP is the inverse of FlowIPBlob: 4 or 16 bytes give an address
// (v4-mapped unmapped), anything else the invalid Addr.
func FlowBlobIP(b []byte) netip.Addr {
	switch len(b) {
	case 4:
		return netip.AddrFrom4([4]byte(b))
	case 16:
		return netip.AddrFrom16([16]byte(b)).Unmap()
	}
	return netip.Addr{}
}

// FlowTable names one of the two flow fact resolutions. Each has a meta table
// (<name>_meta) recording which buckets an exporter covered.
type FlowTable string

const (
	Flows1m FlowTable = "flow_1m"
	Flows1h FlowTable = "flow_1h"
)

// idents returns the fact and meta table names for interpolation into SQL.
// Table names cannot be bound, so anything but the two known tables fails.
func (t FlowTable) idents() (data, meta string, err error) {
	switch t {
	case Flows1m, Flows1h:
		return string(t), string(t) + "_meta", nil
	}
	return "", "", fmt.Errorf("queries: unknown flow table %q", string(t))
}

// native returns the table's bucket length.
func (t FlowTable) native() time.Duration {
	if t == Flows1h {
		return time.Hour
	}
	return time.Minute
}

// unixOf converts a time to unix seconds, 0 for the zero time.
func unixOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// fromUnix converts unix seconds to a UTC time, the zero time for 0.
func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// isUniqueViolation reports whether err is a SQLite UNIQUE / PRIMARY KEY
// constraint failure.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// ---- exporters ----

// FlowExporter is one flow_exporters row: an allow-listed NetFlow / IPFIX /
// sFlow sender plus the health the collector persists about it.
type FlowExporter struct {
	ID               int64
	Name, Address    string
	Kind             string // routeros | opnsense | other
	DeviceID         string // "" = none
	Enabled, Auto    bool
	SamplingOverride int
	NATAddresses     []netip.Addr
	// Persisted health (written by the collector only; read-only for the API).
	LastSeen                              time.Time // zero = never
	Protocol                              string
	SamplingRate                          int
	Datagrams, Flows, Bytes               int64
	DecodeErrors, TemplateMisses, SeqLost int64
	ClockSkewMs                           int64
	LastError                             string
	LastErrorAt                           time.Time
	CreatedAt                             time.Time
}

// formatAddrCSV renders addresses as a canonical CSV (unmapped).
func formatAddrCSV(as []netip.Addr) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		if a.IsValid() {
			parts = append(parts, a.Unmap().String())
		}
	}
	return strings.Join(parts, ",")
}

// parseAddrCSV parses a CSV of addresses, skipping junk. Never nil.
func parseAddrCSV(s string) []netip.Addr {
	out := []netip.Addr{}
	for _, p := range strings.Split(s, ",") {
		if a, err := netip.ParseAddr(strings.TrimSpace(p)); err == nil {
			out = append(out, a.Unmap())
		}
	}
	return out
}

const flowExporterCols = `id, name, address, kind, device_id, enabled, auto, sampling_override, nat_addresses,
	last_seen, protocol, sampling_rate, datagrams, flows, bytes, decode_errors, template_misses, seq_lost,
	clock_skew_ms, last_error, last_error_at, created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanFlowExporter(s rowScanner) (FlowExporter, error) {
	var (
		e                                FlowExporter
		device                           sql.NullString
		nat                              string
		lastSeen, lastErrorAt, createdAt int64
		enabled, auto                    int
	)
	if err := s.Scan(&e.ID, &e.Name, &e.Address, &e.Kind, &device, &enabled, &auto, &e.SamplingOverride, &nat,
		&lastSeen, &e.Protocol, &e.SamplingRate, &e.Datagrams, &e.Flows, &e.Bytes, &e.DecodeErrors,
		&e.TemplateMisses, &e.SeqLost, &e.ClockSkewMs, &e.LastError, &lastErrorAt, &createdAt); err != nil {
		return e, err
	}
	e.DeviceID = device.String
	e.Enabled, e.Auto = enabled != 0, auto != 0
	e.NATAddresses = parseAddrCSV(nat)
	e.LastSeen, e.LastErrorAt, e.CreatedAt = fromUnix(lastSeen), fromUnix(lastErrorAt), fromUnix(createdAt)
	return e, nil
}

// ListFlowExporters returns every exporter ordered by name, then id.
func ListFlowExporters(db *sql.DB) ([]FlowExporter, error) {
	rows, err := db.Query(`SELECT ` + flowExporterCols + ` FROM flow_exporters ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowExporter
	for rows.Next() {
		e, err := scanFlowExporter(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetFlowExporter returns one exporter, or (nil, nil) when it does not exist.
func GetFlowExporter(db *sql.DB, id int64) (*FlowExporter, error) {
	e, err := scanFlowExporter(db.QueryRow(`SELECT `+flowExporterCols+` FROM flow_exporters WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// CreateFlowExporter inserts name, address, kind, device_id, enabled, auto,
// sampling_override and nat_addresses and returns the new id;
// ErrFlowExporterExists on a duplicate address.
func CreateFlowExporter(db *sql.DB, e *FlowExporter) (int64, error) {
	res, err := db.Exec(
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
	return res.LastInsertId()
}

// UpdateFlowExporterConfig updates name, address, kind, device_id, enabled,
// sampling_override and nat_addresses (never auto or the health columns).
// ErrFlowNotFound / ErrFlowExporterExists.
func UpdateFlowExporterConfig(db *sql.DB, e *FlowExporter) error {
	res, err := db.Exec(
		`UPDATE flow_exporters SET name = ?, address = ?, kind = ?, device_id = ?, enabled = ?,
		    sampling_override = ?, nat_addresses = ?
		 WHERE id = ?`,
		e.Name, e.Address, e.Kind, nullString(e.DeviceID), boolInt(e.Enabled), e.SamplingOverride,
		formatAddrCSV(e.NATAddresses), e.ID)
	if isUniqueViolation(err) {
		return ErrFlowExporterExists
	}
	return affectedOrNotFound(res, err)
}

// DeleteFlowExporter deletes the exporter; its interfaces, points and
// templates cascade. Its fact rows are left as orphans for DeleteOrphanFlows
// (a synchronous purge of days of rows would be slow). ErrFlowNotFound.
func DeleteFlowExporter(db *sql.DB, id int64) error {
	res, err := db.Exec(`DELETE FROM flow_exporters WHERE id = ?`, id)
	return affectedOrNotFound(res, err)
}

func affectedOrNotFound(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrFlowNotFound
	}
	return nil
}

// FlowExporterStatusDelta is what the collector adds to an exporter's
// persisted health.
type FlowExporterStatusDelta struct {
	LastSeen                                                       time.Time // replaces when non-zero
	Protocol                                                       string    // replaces when non-empty
	SamplingRate                                                   int       // replaces when > 0
	Datagrams, Flows, Bytes, DecodeErrors, TemplateMisses, SeqLost int64     // added
	ClockSkewMs                                                    int64     // replaces (always)
	LastError                                                      string    // replaces when LastErrorAt non-zero
	LastErrorAt                                                    time.Time
}

// AddFlowExporterStatus applies d to exporter id. A missing exporter (deleted
// meanwhile) is not an error.
func AddFlowExporterStatus(db *sql.DB, id int64, d FlowExporterStatusDelta) error {
	ls, lea := unixOf(d.LastSeen), unixOf(d.LastErrorAt)
	_, err := db.Exec(
		`UPDATE flow_exporters SET
		    last_seen = CASE WHEN ? > 0 THEN ? ELSE last_seen END,
		    protocol = CASE WHEN ? != '' THEN ? ELSE protocol END,
		    sampling_rate = CASE WHEN ? > 0 THEN ? ELSE sampling_rate END,
		    datagrams = datagrams + ?, flows = flows + ?, bytes = bytes + ?,
		    decode_errors = decode_errors + ?, template_misses = template_misses + ?, seq_lost = seq_lost + ?,
		    clock_skew_ms = ?,
		    last_error = CASE WHEN ? > 0 THEN ? ELSE last_error END,
		    last_error_at = CASE WHEN ? > 0 THEN ? ELSE last_error_at END
		 WHERE id = ?`,
		ls, ls, d.Protocol, d.Protocol, d.SamplingRate, d.SamplingRate,
		d.Datagrams, d.Flows, d.Bytes, d.DecodeErrors, d.TemplateMisses, d.SeqLost,
		d.ClockSkewMs, lea, d.LastError, lea, lea, id)
	return err
}

// ---- exporter interfaces ----

// FlowIface is one flow_exporter_ifaces row: an exporter's ifIndex with its
// name (device sync, learned, or set by an admin).
type FlowIface struct {
	ExporterID          int64
	IfIndex             uint32
	Name, Type          string
	VLANID              int
	Parent              string
	Role, Source, Hint  string
	FirstSeen, LastSeen time.Time
}

// ListFlowIfaces returns the interfaces of one exporter (0 = all exporters),
// ordered by exporter_id, if_index.
func ListFlowIfaces(db *sql.DB, exporterID int64) ([]FlowIface, error) {
	q := `SELECT exporter_id, if_index, name, type, vlan_id, parent, role, source, hint, first_seen, last_seen
	      FROM flow_exporter_ifaces`
	var args []any
	if exporterID != 0 {
		q += ` WHERE exporter_id = ?`
		args = append(args, exporterID)
	}
	rows, err := db.Query(q+` ORDER BY exporter_id, if_index`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowIface
	for rows.Next() {
		var f FlowIface
		var idx, first, last int64
		if err := rows.Scan(&f.ExporterID, &idx, &f.Name, &f.Type, &f.VLANID, &f.Parent, &f.Role, &f.Source,
			&f.Hint, &first, &last); err != nil {
			return nil, err
		}
		f.IfIndex = uint32(idx)
		f.FirstSeen, f.LastSeen = fromUnix(first), fromUnix(last)
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpsertDeviceFlowIfaces writes source=device rows (name, type, vlan_id,
// parent) in one transaction. Rows an admin named (source=manual) keep their
// name and role and only get type, vlan_id and parent updated. last_seen and
// hint are never touched. ifIndex 0 ("local") is skipped.
func UpsertDeviceFlowIfaces(db *sql.DB, exporterID int64, ifs []FlowIface) error {
	if len(ifs) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(
		`INSERT INTO flow_exporter_ifaces (exporter_id, if_index, name, type, vlan_id, parent, source)
		 VALUES (?, ?, ?, ?, ?, ?, 'device')
		 ON CONFLICT(exporter_id, if_index) DO UPDATE SET
		    type = excluded.type, vlan_id = excluded.vlan_id, parent = excluded.parent,
		    name = CASE WHEN flow_exporter_ifaces.source = 'manual' THEN flow_exporter_ifaces.name ELSE excluded.name END,
		    source = CASE WHEN flow_exporter_ifaces.source = 'manual' THEN 'manual' ELSE 'device' END`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, f := range ifs {
		if f.IfIndex == 0 {
			continue
		}
		if _, err := stmt.Exec(exporterID, int64(f.IfIndex), f.Name, f.Type, f.VLANID, f.Parent); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FlowIfaceSeen is one ifIndex the collector saw bytes on.
type FlowIfaceSeen struct {
	IfIndex uint32
	Hint    string // "" = leave hint unchanged
}

// TouchFlowIfaces inserts missing rows (source=learned, name=”,
// first_seen=at) and, for every entry, raises last_seen to at, sets
// first_seen=at when it is still 0 (device-synced rows) and replaces the hint
// when the given hint is non-empty. ifIndex 0 is skipped.
func TouchFlowIfaces(db *sql.DB, exporterID int64, seen []FlowIfaceSeen, at time.Time) error {
	if len(seen) == 0 {
		return nil
	}
	ts := unixOf(at)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(
		`INSERT INTO flow_exporter_ifaces (exporter_id, if_index, source, hint, first_seen, last_seen)
		 VALUES (?, ?, 'learned', ?, ?, ?)
		 ON CONFLICT(exporter_id, if_index) DO UPDATE SET
		    last_seen = MAX(flow_exporter_ifaces.last_seen, excluded.last_seen),
		    first_seen = CASE WHEN flow_exporter_ifaces.first_seen = 0 THEN excluded.first_seen
		                      ELSE flow_exporter_ifaces.first_seen END,
		    hint = CASE WHEN excluded.hint != '' THEN excluded.hint ELSE flow_exporter_ifaces.hint END`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, s := range seen {
		if s.IfIndex == 0 {
			continue
		}
		if _, err := stmt.Exec(exporterID, int64(s.IfIndex), s.Hint, ts, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetFlowIfaceManual upserts name and role with source=manual. name == "" and
// role == "" revert the row to source=learned with both cleared (the next
// device sync may rename it).
func SetFlowIfaceManual(db *sql.DB, exporterID int64, ifIndex uint32, name, role string) error {
	source := "manual"
	if name == "" && role == "" {
		source = "learned"
	}
	_, err := db.Exec(
		`INSERT INTO flow_exporter_ifaces (exporter_id, if_index, name, role, source)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(exporter_id, if_index) DO UPDATE SET
		    name = excluded.name, role = excluded.role, source = excluded.source`,
		exporterID, int64(ifIndex), name, role, source)
	return err
}

// DeleteOldLearnedFlowIfaces deletes source=learned rows with last_seen <
// cutoff and returns how many it removed.
func DeleteOldLearnedFlowIfaces(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM flow_exporter_ifaces WHERE source = 'learned' AND last_seen < ?`, unixOf(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- observation points ----

// PortRef names one managed port.
type PortRef struct {
	DeviceID string `json:"device_id"`
	Iface    string `json:"iface"`
}

// FlowPoint is one flow_points row: an observation point = one exporter plus
// a set of its ifIndexes.
type FlowPoint struct {
	ID, ExporterID   int64
	Name, Kind       string   // kind: native | derived
	IfIndexes        []uint32 // ascending, non-empty, no 0
	Facing, PortSide string   // up|down ; ''|same|peer
	DeviceID, Iface  string
	LocalInternal    bool
	ExcludeAttach    []PortRef // never nil after a read
	Note             string
	Auto, Enabled    bool
	CreatedAt        time.Time
}

// canonicalIfIndexes sorts, de-duplicates and drops 0.
func canonicalIfIndexes(in []uint32) []uint32 {
	out := make([]uint32, 0, len(in))
	for _, i := range in {
		if i != 0 {
			out = append(out, i)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// formatIfIndexes renders the canonical ascending decimal CSV ("10,13").
func formatIfIndexes(in []uint32) string {
	parts := make([]string, len(in))
	for i, v := range in {
		parts[i] = strconv.FormatUint(uint64(v), 10)
	}
	return strings.Join(parts, ",")
}

// parseIfIndexes parses the CSV form, skipping junk.
func parseIfIndexes(s string) []uint32 {
	var out []uint32
	for _, p := range strings.Split(s, ",") {
		if v, err := strconv.ParseUint(strings.TrimSpace(p), 10, 32); err == nil {
			out = append(out, uint32(v))
		}
	}
	return canonicalIfIndexes(out)
}

func formatExcludeAttach(refs []PortRef) (string, error) {
	if refs == nil {
		refs = []PortRef{}
	}
	b, err := json.Marshal(refs)
	return string(b), err
}

func parseExcludeAttach(s string) []PortRef {
	out := []PortRef{}
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []PortRef{}
	}
	return out
}

const flowPointCols = `id, exporter_id, name, kind, if_indexes, facing, port_side, device_id, iface, local_internal,
	exclude_attach, note, auto, enabled, created_at`

func scanFlowPoint(s rowScanner) (FlowPoint, error) {
	var (
		p                    FlowPoint
		ifs, excl            string
		device               sql.NullString
		local, auto, enabled int
		created              int64
	)
	if err := s.Scan(&p.ID, &p.ExporterID, &p.Name, &p.Kind, &ifs, &p.Facing, &p.PortSide, &device, &p.Iface,
		&local, &excl, &p.Note, &auto, &enabled, &created); err != nil {
		return p, err
	}
	p.IfIndexes = parseIfIndexes(ifs)
	p.DeviceID = device.String
	p.LocalInternal, p.Auto, p.Enabled = local != 0, auto != 0, enabled != 0
	p.ExcludeAttach = parseExcludeAttach(excl)
	p.CreatedAt = fromUnix(created)
	return p, nil
}

// ListFlowPoints returns every point ordered by exporter_id, kind (derived
// before native), name, id.
func ListFlowPoints(db *sql.DB) ([]FlowPoint, error) {
	rows, err := db.Query(`SELECT ` + flowPointCols + ` FROM flow_points ORDER BY exporter_id, kind, name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowPoint
	for rows.Next() {
		p, err := scanFlowPoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFlowPoint returns one point, or (nil, nil) when it does not exist.
func GetFlowPoint(db *sql.DB, id int64) (*FlowPoint, error) {
	p, err := scanFlowPoint(db.QueryRow(`SELECT `+flowPointCols+` FROM flow_points WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// errNoIfIndexes rejects a point without a usable ifIndex.
var errNoIfIndexes = errors.New("queries: flow point needs at least one non-zero ifIndex")

// CreateFlowPoint inserts p (if_indexes canonicalised) and returns its id;
// ErrFlowPointExists when a native point with the same (exporter, if_indexes)
// or a derived one with the same (exporter, device_id, iface) exists.
func CreateFlowPoint(db *sql.DB, p *FlowPoint) (int64, error) {
	ifs := canonicalIfIndexes(p.IfIndexes)
	if len(ifs) == 0 {
		return 0, errNoIfIndexes
	}
	if len(ifs) > MaxFlowIfs {
		return 0, errTooManyIfIndexes
	}
	excl, err := formatExcludeAttach(p.ExcludeAttach)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(
		`INSERT INTO flow_points (exporter_id, name, kind, if_indexes, facing, port_side, device_id, iface,
		    local_internal, exclude_attach, note, auto, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ExporterID, p.Name, p.Kind, formatIfIndexes(ifs), p.Facing, p.PortSide, nullString(p.DeviceID), p.Iface,
		boolInt(p.LocalInternal), excl, p.Note, boolInt(p.Auto), boolInt(p.Enabled))
	if isUniqueViolation(err) {
		return 0, ErrFlowPointExists
	}
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateFlowPoint rewrites every config field of point p.ID and sets auto=0
// (an admin edit takes the point over). ErrFlowNotFound / ErrFlowPointExists.
func UpdateFlowPoint(db *sql.DB, p *FlowPoint) error {
	ifs := canonicalIfIndexes(p.IfIndexes)
	if len(ifs) == 0 {
		return errNoIfIndexes
	}
	if len(ifs) > MaxFlowIfs {
		return errTooManyIfIndexes
	}
	excl, err := formatExcludeAttach(p.ExcludeAttach)
	if err != nil {
		return err
	}
	res, err := db.Exec(
		`UPDATE flow_points SET exporter_id = ?, name = ?, kind = ?, if_indexes = ?, facing = ?, port_side = ?,
		    device_id = ?, iface = ?, local_internal = ?, exclude_attach = ?, note = ?, enabled = ?, auto = 0
		 WHERE id = ?`,
		p.ExporterID, p.Name, p.Kind, formatIfIndexes(ifs), p.Facing, p.PortSide, nullString(p.DeviceID), p.Iface,
		boolInt(p.LocalInternal), excl, p.Note, boolInt(p.Enabled), p.ID)
	if isUniqueViolation(err) {
		return ErrFlowPointExists
	}
	return affectedOrNotFound(res, err)
}

// DeleteFlowPoint deletes one point. ErrFlowNotFound.
func DeleteFlowPoint(db *sql.DB, id int64) error {
	res, err := db.Exec(`DELETE FROM flow_points WHERE id = ?`, id)
	return affectedOrNotFound(res, err)
}

// EnsureNativeFlowPoint inserts p with kind forced to native and auto=1
// unless a native point with the same (exporter_id, if_indexes) exists.
func EnsureNativeFlowPoint(db *sql.DB, p *FlowPoint) (created bool, err error) {
	c, err := EnsureNativeFlowPoints(db, []FlowPoint{*p})
	if err != nil {
		return false, err
	}
	return c[0], nil
}

// EnsureNativeFlowPoints is EnsureNativeFlowPoint for several points in one
// transaction; created[i] reports whether ps[i] was inserted. A point
// without a usable ifIndex fails the whole batch.
func EnsureNativeFlowPoints(db *sql.DB, ps []FlowPoint) (created []bool, err error) {
	created = make([]bool, len(ps))
	if len(ps) == 0 {
		return created, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO flow_points (exporter_id, name, kind, if_indexes, facing, port_side, device_id, iface,
		    local_internal, exclude_attach, note, auto, enabled)
		 VALUES (?, ?, 'native', ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for i, p := range ps {
		ifs := canonicalIfIndexes(p.IfIndexes)
		if len(ifs) == 0 {
			return nil, errNoIfIndexes
		}
		excl, err := formatExcludeAttach(p.ExcludeAttach)
		if err != nil {
			return nil, err
		}
		res, err := stmt.Exec(p.ExporterID, p.Name, formatIfIndexes(ifs), p.Facing, p.PortSide, nullString(p.DeviceID),
			p.Iface, boolInt(p.LocalInternal), excl, p.Note, boolInt(p.Enabled))
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		created[i] = n > 0
	}
	return created, tx.Commit()
}

// UpdateAutoFlowPoint rewrites name, facing, port_side, device_id and iface
// of point p.ID only while it is still auto=1; a no-op otherwise.
func UpdateAutoFlowPoint(db *sql.DB, p *FlowPoint) error {
	_, err := db.Exec(
		`UPDATE flow_points SET name = ?, facing = ?, port_side = ?, device_id = ?, iface = ?
		 WHERE id = ? AND auto = 1`,
		p.Name, p.Facing, p.PortSide, nullString(p.DeviceID), p.Iface, p.ID)
	return err
}

// DeleteStaleAutoFlowPoints deletes native auto=1 points created before
// cutoff none of whose ifIndexes has last_seen >= cutoff (dynamic CAPsMAN
// interfaces that were recreated under a new index), and returns how many.
func DeleteStaleAutoFlowPoints(db *sql.DB, cutoff time.Time) (int64, error) {
	ifaces, err := ListFlowIfaces(db, 0)
	if err != nil {
		return 0, err
	}
	type ifKey struct {
		exp int64
		idx uint32
	}
	fresh := make(map[ifKey]bool, len(ifaces))
	for _, f := range ifaces {
		if !f.LastSeen.Before(cutoff) {
			fresh[ifKey{f.ExporterID, f.IfIndex}] = true
		}
	}
	points, err := ListFlowPoints(db)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, p := range points {
		if p.Kind != "native" || !p.Auto || !p.CreatedAt.Before(cutoff) {
			continue
		}
		stale := true
		for _, idx := range p.IfIndexes {
			if fresh[ifKey{p.ExporterID, idx}] {
				stale = false
				break
			}
		}
		if !stale {
			continue
		}
		res, err := db.Exec(`DELETE FROM flow_points WHERE id = ? AND auto = 1 AND kind = 'native'`, p.ID)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// ---- fact rows ----

// FlowRow is one flow_1m / flow_1h row. An invalid Src and Dst is the folded
// "other" row of its (in_if, out_if) pair.
type FlowRow struct {
	ExporterID            int64
	Bucket                time.Time
	InIf, OutIf           uint32
	Src, Dst              netip.Addr // invalid = other
	Proto                 uint8
	Port                  uint16
	Bytes, Packets, Flows int64
}

// FlowMeta is one flow_1m_meta / flow_1h_meta row: the exporter covered the
// bucket (it was alive), even with zero records.
type FlowMeta struct {
	ExporterID                                                   int64
	Bucket                                                       time.Time
	Datagrams, Records, Bytes, Sampling, Overflow, Late, Minutes int64 // Minutes: 1h only
}

// InsertFlows1m writes rows + meta in ONE transaction with ADDITIVE upserts:
// an existing row's bytes/packets/flows and an existing meta row's counters
// are summed (sampling = MAX), so a minute flushed twice (shutdown, then
// restart) accumulates instead of being overwritten. Buckets are truncated
// to the minute.
func InsertFlows1m(db *sql.DB, rows []FlowRow, meta []FlowMeta) error {
	if len(rows) == 0 && len(meta) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if len(rows) > 0 {
		stmt, err := tx.Prepare(
			`INSERT INTO flow_1m (exporter_id, bucket, in_if, out_if, src, dst, proto, port, bytes, packets, flows)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(exporter_id, bucket, in_if, out_if, src, dst, proto, port) DO UPDATE SET
			    bytes = bytes + excluded.bytes, packets = packets + excluded.packets, flows = flows + excluded.flows`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows {
			if _, err := stmt.Exec(r.ExporterID, r.Bucket.UTC().Truncate(time.Minute).Unix(), int64(r.InIf),
				int64(r.OutIf), FlowIPBlob(r.Src), FlowIPBlob(r.Dst), int64(r.Proto), int64(r.Port),
				r.Bytes, r.Packets, r.Flows); err != nil {
				return err
			}
		}
	}
	if len(meta) > 0 {
		stmt, err := tx.Prepare(
			`INSERT INTO flow_1m_meta (exporter_id, bucket, datagrams, records, bytes, sampling, overflow, late)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(exporter_id, bucket) DO UPDATE SET
			    datagrams = datagrams + excluded.datagrams, records = records + excluded.records,
			    bytes = bytes + excluded.bytes, sampling = MAX(sampling, excluded.sampling),
			    overflow = overflow + excluded.overflow, late = late + excluded.late`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, m := range meta {
			if _, err := stmt.Exec(m.ExporterID, m.Bucket.UTC().Truncate(time.Minute).Unix(), m.Datagrams,
				m.Records, m.Bytes, m.Sampling, m.Overflow, m.Late); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// flowAggKey identifies a conversation inside one bucket (addresses in their
// 16-byte form so IPv4 and IPv6 sort together deterministically).
type flowAggKey struct {
	inIf, outIf uint32
	src, dst    [16]byte
	srcOK       bool // false = invalid (the folded other row)
	dstOK       bool
	proto       uint8
	port        uint16
}

type flowPair struct{ inIf, outIf uint32 }

type flowSums struct{ bytes, packets, flows int64 }

func addr16(a netip.Addr) ([16]byte, bool) {
	if !a.IsValid() {
		return [16]byte{}, false
	}
	return a.As16(), true
}

// compareFlowKeys orders keys by their key bytes ascending (src, dst, proto,
// port) — the deterministic tie-break after bytes.
func compareFlowKeys(a, b flowAggKey) int {
	if c := bytes.Compare(a.src[:], b.src[:]); c != 0 {
		return c
	}
	if c := bytes.Compare(a.dst[:], b.dst[:]); c != 0 {
		return c
	}
	if c := cmp.Compare(a.proto, b.proto); c != 0 {
		return c
	}
	return cmp.Compare(a.port, b.port)
}

func keyAddr(b [16]byte, ok bool) netip.Addr {
	if !ok {
		return netip.Addr{}
	}
	return netip.AddrFrom16(b).Unmap()
}

// RollupFlows1h recomputes one (exporter, hour) of flow_1h and flow_1h_meta
// from flow_1m in ONE transaction: the hour's 1h rows are deleted, the 1m rows
// aggregated in Go, and per (in_if, out_if) the top topN conversations by
// bytes (ties: key bytes ascending) kept plus one exact "other" row holding
// the rest (existing 1m other rows fold into it). Meta is summed (sampling =
// MAX) with minutes = the count of 1m meta rows. Idempotent.
func RollupFlows1h(db *sql.DB, exporterID int64, hour time.Time, topN int) error {
	hour = hour.UTC().Truncate(time.Hour)
	from, to := hour.Unix(), hour.Add(time.Hour).Unix()
	topN = max(topN, 1)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Delete first: it takes the write lock before the reads, so the
	// transaction never has to upgrade a stale read snapshot (SQLITE_BUSY).
	if _, err := tx.Exec(`DELETE FROM flow_1h WHERE exporter_id = ? AND bucket = ?`, exporterID, from); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM flow_1h_meta WHERE exporter_id = ? AND bucket = ?`, exporterID, from); err != nil {
		return err
	}

	sums := make(map[flowAggKey]*flowSums)
	others := make(map[flowPair]*flowSums)
	rows, err := tx.Query(
		`SELECT in_if, out_if, src, dst, proto, port, bytes, packets, flows FROM flow_1m
		 WHERE exporter_id = ? AND bucket >= ? AND bucket < ?`, exporterID, from, to)
	if err != nil {
		return err
	}
	for rows.Next() {
		var (
			inIf, outIf, proto, port int64
			src, dst                 []byte
			s                        flowSums
		)
		if err := rows.Scan(&inIf, &outIf, &src, &dst, &proto, &port, &s.bytes, &s.packets, &s.flows); err != nil {
			rows.Close()
			return err
		}
		pair := flowPair{uint32(inIf), uint32(outIf)}
		sa, sok := addr16(FlowBlobIP(src))
		da, dok := addr16(FlowBlobIP(dst))
		if !sok && !dok {
			acc := others[pair]
			if acc == nil {
				acc = &flowSums{}
				others[pair] = acc
			}
			acc.bytes, acc.packets, acc.flows = acc.bytes+s.bytes, acc.packets+s.packets, acc.flows+s.flows
			continue
		}
		k := flowAggKey{inIf: pair.inIf, outIf: pair.outIf, src: sa, srcOK: sok, dst: da, dstOK: dok,
			proto: uint8(proto), port: uint16(port)}
		acc := sums[k]
		if acc == nil {
			acc = &flowSums{}
			sums[k] = acc
		}
		acc.bytes, acc.packets, acc.flows = acc.bytes+s.bytes, acc.packets+s.packets, acc.flows+s.flows
	}
	if err := rows.Close(); err != nil {
		return err
	}

	var meta FlowMeta
	if err := tx.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(datagrams), 0), COALESCE(SUM(records), 0), COALESCE(SUM(bytes), 0),
		        COALESCE(MAX(sampling), 0), COALESCE(SUM(overflow), 0), COALESCE(SUM(late), 0)
		 FROM flow_1m_meta WHERE exporter_id = ? AND bucket >= ? AND bucket < ?`, exporterID, from, to,
	).Scan(&meta.Minutes, &meta.Datagrams, &meta.Records, &meta.Bytes, &meta.Sampling, &meta.Overflow, &meta.Late); err != nil {
		return err
	}

	// Per pair: top topN by bytes, the rest into the pair's other row.
	byPair := make(map[flowPair][]flowAggKey)
	for k := range sums {
		p := flowPair{k.inIf, k.outIf}
		byPair[p] = append(byPair[p], k)
	}
	ins, err := tx.Prepare(
		`INSERT INTO flow_1h (exporter_id, bucket, in_if, out_if, src, dst, proto, port, bytes, packets, flows)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for pair, keys := range byPair {
		slices.SortFunc(keys, func(a, b flowAggKey) int {
			if c := cmp.Compare(sums[b].bytes, sums[a].bytes); c != 0 {
				return c
			}
			return compareFlowKeys(a, b)
		})
		for i, k := range keys {
			s := sums[k]
			if i >= topN {
				acc := others[pair]
				if acc == nil {
					acc = &flowSums{}
					others[pair] = acc
				}
				acc.bytes, acc.packets, acc.flows = acc.bytes+s.bytes, acc.packets+s.packets, acc.flows+s.flows
				continue
			}
			if _, err := ins.Exec(exporterID, from, int64(k.inIf), int64(k.outIf),
				FlowIPBlob(keyAddr(k.src, k.srcOK)), FlowIPBlob(keyAddr(k.dst, k.dstOK)),
				int64(k.proto), int64(k.port), s.bytes, s.packets, s.flows); err != nil {
				return err
			}
		}
	}
	for pair, s := range others {
		if _, err := ins.Exec(exporterID, from, int64(pair.inIf), int64(pair.outIf), []byte{}, []byte{}, 0, 0,
			s.bytes, s.packets, s.flows); err != nil {
			return err
		}
	}
	if meta.Minutes > 0 {
		if _, err := tx.Exec(
			`INSERT INTO flow_1h_meta (exporter_id, bucket, datagrams, records, bytes, sampling, overflow, late, minutes)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			exporterID, from, meta.Datagrams, meta.Records, meta.Bytes, meta.Sampling, meta.Overflow, meta.Late,
			meta.Minutes); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ExporterHour is one (exporter, hour start) pair.
type ExporterHour struct {
	ExporterID int64
	Hour       time.Time
}

// UnrolledFlowHours returns, oldest first, the (exporter, hour) pairs with
// flow_1m_meta minutes in [from, to) but no flow_1h_meta row for that hour:
// hours whose rollup never ran (the process stopped before xx:02, or the
// shutdown flush wrote a partial hour, and the downtime outlasted the
// startup re-roll).
func UnrolledFlowHours(db *sql.DB, from, to time.Time) ([]ExporterHour, error) {
	rows, err := db.Query(
		`SELECT DISTINCT m.exporter_id, m.bucket - m.bucket % 3600 AS h FROM flow_1m_meta m
		 WHERE m.bucket >= ? AND m.bucket < ?
		   AND NOT EXISTS (SELECT 1 FROM flow_1h_meta x
		                   WHERE x.exporter_id = m.exporter_id AND x.bucket = m.bucket - m.bucket % 3600)
		 ORDER BY h, m.exporter_id`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExporterHour
	for rows.Next() {
		var id, h int64
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out = append(out, ExporterHour{ExporterID: id, Hour: time.Unix(h, 0).UTC()})
	}
	return out, rows.Err()
}

// FlowAgg is one aggregated group returned by AggregateFlows.
type FlowAgg struct {
	Bucket                time.Time // step-aligned group start; = from when step == 0
	InIf, OutIf           uint32
	Src, Dst              netip.Addr
	Proto                 uint8
	Port                  uint16
	Bytes, Packets, Flows int64
}

// MaxFlowIfs bounds the interfaces of a point and the IN lists of
// AggregateFlows (SQLite's bound-parameter limit is far higher).
const MaxFlowIfs = 256

// errTooManyIfIndexes rejects a point with more than MaxFlowIfs interfaces:
// it could be stored but never read.
var errTooManyIfIndexes = fmt.Errorf("queries: flow point has more than %d ifIndexes", MaxFlowIfs)

// AggregateFlows sums the rows of table t for one exporter with from <=
// bucket < to whose in_if OR out_if is in ifs, grouped by (step bucket, in_if,
// out_if, src, dst, proto, port). step 0 = one group over the whole range;
// otherwise step is a multiple of the table's native bucket and groups start
// at from + k*step. len(ifs) == 0 returns no rows. Ordered by bucket, then
// bytes DESC.
func AggregateFlows(db *sql.DB, t FlowTable, exporterID int64, ifs []uint32, from, to time.Time, step time.Duration) ([]FlowAgg, error) {
	grp, where, args, err := flowAggSQL(t, exporterID, ifs, from, to, step)
	if err != nil || grp == "" {
		return nil, err
	}
	q := `SELECT ` + grp + ` AS g, in_if, out_if, src, dst, proto, port, SUM(bytes) AS b, SUM(packets), SUM(flows)
	      FROM ` + where + `
	      GROUP BY g, in_if, out_if, src, dst, proto, port
	      ORDER BY g, b DESC, in_if, out_if, src, dst, proto, port`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowAgg
	for rows.Next() {
		var (
			a                        FlowAgg
			g, inIf, outIf, pr, port int64
			src, dst                 []byte
		)
		if err := rows.Scan(&g, &inIf, &outIf, &src, &dst, &pr, &port, &a.Bytes, &a.Packets, &a.Flows); err != nil {
			return nil, err
		}
		a.Bucket = time.Unix(g, 0).UTC()
		a.InIf, a.OutIf = uint32(inIf), uint32(outIf)
		a.Src, a.Dst = FlowBlobIP(src), FlowBlobIP(dst)
		a.Proto, a.Port = uint8(pr), uint16(port)
		out = append(out, a)
	}
	return out, rows.Err()
}

// AggregateFlowBytes is AggregateFlows reduced to what per-direction byte
// totals need: rows grouped by (step bucket, in_if, out_if) — plus src and
// dst when withEndpoints (a point's local_internal / exclude_attach filters
// look at the local endpoint; proto and port never matter). Without
// endpoints Src/Dst are invalid, so flowview treats every row like the
// folded "other" row (never filtered). Packets and Flows are not summed.
// Ordered by bucket.
func AggregateFlowBytes(db *sql.DB, t FlowTable, exporterID int64, ifs []uint32, from, to time.Time, step time.Duration,
	withEndpoints bool) ([]FlowAgg, error) {
	grp, where, args, err := flowAggSQL(t, exporterID, ifs, from, to, step)
	if err != nil || grp == "" {
		return nil, err
	}
	cols := `in_if, out_if`
	if withEndpoints {
		cols += `, src, dst`
	}
	rows, err := db.Query(`SELECT `+grp+` AS g, `+cols+`, SUM(bytes) FROM `+where+`
	      GROUP BY g, `+cols+` ORDER BY g`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowAgg
	for rows.Next() {
		var (
			a              FlowAgg
			g, inIf, outIf int64
			src, dst       []byte
		)
		dest := []any{&g, &inIf, &outIf}
		if withEndpoints {
			dest = append(dest, &src, &dst)
		}
		if err := rows.Scan(append(dest, &a.Bytes)...); err != nil {
			return nil, err
		}
		a.Bucket = time.Unix(g, 0).UTC()
		a.InIf, a.OutIf = uint32(inIf), uint32(outIf)
		if withEndpoints {
			a.Src, a.Dst = FlowBlobIP(src), FlowBlobIP(dst)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// flowAggSQL validates the AggregateFlows arguments and returns the group
// expression, the "table WHERE …" clause and their args. grp is "" (and err
// nil) when the query cannot return rows.
func flowAggSQL(t FlowTable, exporterID int64, ifs []uint32, from, to time.Time, step time.Duration) (grp, where string, args []any, err error) {
	data, _, err := t.idents()
	if err != nil {
		return "", "", nil, err
	}
	ifs = canonicalIfIndexes(ifs)
	if len(ifs) == 0 || !to.After(from) {
		return "", "", nil, nil
	}
	if len(ifs) > MaxFlowIfs {
		return "", "", nil, fmt.Errorf("queries: too many interfaces (%d)", len(ifs))
	}
	if step < 0 || (step > 0 && step%t.native() != 0) {
		return "", "", nil, fmt.Errorf("queries: step %s is not a multiple of %s", step, t.native())
	}
	f, tt := from.Unix(), to.Unix()
	stepSec := int64(step / time.Second)

	in := strings.Repeat(",?", len(ifs))[1:]
	grp = `?`
	if stepSec > 0 {
		grp = `(? + ((bucket - ?) / ?) * ?)`
		args = append(args, f, f, stepSec, stepSec)
	} else {
		args = append(args, f)
	}
	args = append(args, exporterID, f, tt)
	for _, i := range ifs {
		args = append(args, int64(i))
	}
	for _, i := range ifs {
		args = append(args, int64(i))
	}
	where = data + `
	      WHERE exporter_id = ? AND bucket >= ? AND bucket < ? AND (in_if IN (` + in + `) OR out_if IN (` + in + `))`
	return grp, where, args, nil
}

// FlowMetaMinutes returns the covered minutes (flow_1h_meta.minutes) of the
// exporter's hours in [from, to), keyed by hour start (Unix seconds).
func FlowMetaMinutes(db *sql.DB, exporterID int64, from, to time.Time) (map[int64]int64, error) {
	rows, err := db.Query(`SELECT bucket, minutes FROM flow_1h_meta WHERE exporter_id = ? AND bucket >= ? AND bucket < ?`,
		exporterID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var b, m int64
		if err := rows.Scan(&b, &m); err != nil {
			return nil, err
		}
		out[b] = m
	}
	return out, rows.Err()
}

// FlowMetaBuckets returns, ascending, the buckets in [from, to) the exporter
// covered (meta rows present).
func FlowMetaBuckets(db *sql.DB, t FlowTable, exporterID int64, from, to time.Time) ([]time.Time, error) {
	_, meta, err := t.idents()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT bucket FROM `+meta+` WHERE exporter_id = ? AND bucket >= ? AND bucket < ? ORDER BY bucket`,
		exporterID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, time.Unix(b, 0).UTC())
	}
	return out, rows.Err()
}

// FlowMetaTotals summarises an exporter's meta rows over a window.
type FlowMetaTotals struct {
	Buckets                                   int64
	Datagrams, Records, Bytes, Overflow, Late int64
	MaxSampling                               int64 // 0 when no rows
}

// FlowMetaSummary sums the exporter's meta rows with from <= bucket < to.
func FlowMetaSummary(db *sql.DB, t FlowTable, exporterID int64, from, to time.Time) (FlowMetaTotals, error) {
	var s FlowMetaTotals
	_, meta, err := t.idents()
	if err != nil {
		return s, err
	}
	err = db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(datagrams), 0), COALESCE(SUM(records), 0), COALESCE(SUM(bytes), 0),
		        COALESCE(SUM(overflow), 0), COALESCE(SUM(late), 0), COALESCE(MAX(sampling), 0)
		 FROM `+meta+` WHERE exporter_id = ? AND bucket >= ? AND bucket < ?`,
		exporterID, from.Unix(), to.Unix(),
	).Scan(&s.Buckets, &s.Datagrams, &s.Records, &s.Bytes, &s.Overflow, &s.Late, &s.MaxSampling)
	return s, err
}

// LatestFlowBucket returns the newest meta bucket of an exporter (0 = any
// exporter); ok is false when there is none.
func LatestFlowBucket(db *sql.DB, t FlowTable, exporterID int64) (time.Time, bool, error) {
	_, meta, err := t.idents()
	if err != nil {
		return time.Time{}, false, err
	}
	q := `SELECT MAX(bucket) FROM ` + meta
	var args []any
	if exporterID != 0 {
		q += ` WHERE exporter_id = ?`
		args = append(args, exporterID)
	}
	var b sql.NullInt64
	if err := db.QueryRow(q, args...).Scan(&b); err != nil {
		return time.Time{}, false, err
	}
	if !b.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(b.Int64, 0).UTC(), true, nil
}

// flowDeleteChunk bounds how many rows one DELETE removes and flowDeletePause
// is the pause between chunks, as for port stats: a single DELETE of a large
// backlog would hold SQLite's write lock long enough for other writers to hit
// the busy timeout. Variables so tests can shrink them.
var (
	flowDeleteChunk = 5000
	flowDeletePause = 5 * time.Millisecond
)

// flowTableExporterIDs returns the distinct exporter_id values of a flow
// table with a loose index scan (one primary-key probe per id) rather than a
// full scan: every flow table's primary key starts with exporter_id.
func flowTableExporterIDs(db *sql.DB, table string) ([]int64, error) {
	rows, err := db.Query(
		`WITH RECURSIVE ids(x) AS (
		    SELECT MIN(exporter_id) FROM ` + table + `
		    UNION ALL
		    SELECT (SELECT MIN(exporter_id) FROM ` + table + ` WHERE exporter_id > x) FROM ids WHERE x IS NOT NULL)
		 SELECT x FROM ids WHERE x IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// deleteFlowChunks deletes one exporter's rows of table matching cond (with
// its args) in bounded chunks, selecting primary keys through the exporter's
// key range.
func deleteFlowChunks(db *sql.DB, table string, meta bool, exporterID int64, cond string, args ...any) (int64, error) {
	pk := `exporter_id, bucket, in_if, out_if, src, dst, proto, port`
	if meta {
		pk = `exporter_id, bucket`
	}
	q := `DELETE FROM ` + table + ` WHERE (` + pk + `) IN (
	          SELECT ` + pk + ` FROM ` + table + ` WHERE exporter_id = ?` + cond + ` LIMIT ?)`
	var total int64
	for {
		a := append(append([]any{exporterID}, args...), flowDeleteChunk)
		res, err := db.Exec(q, a...)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(flowDeleteChunk) {
			return total, nil
		}
		time.Sleep(flowDeletePause)
	}
}

// DeleteOldFlows removes data and meta rows with bucket < cutoff from table t
// and its meta table, per exporter in bounded chunks (see flowDeleteChunk),
// and returns how many rows (data + meta) it removed.
func DeleteOldFlows(db *sql.DB, t FlowTable, cutoff time.Time) (int64, error) {
	data, meta, err := t.idents()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, tbl := range []struct {
		name   string
		isMeta bool
	}{{data, false}, {meta, true}} {
		ids, err := flowTableExporterIDs(db, tbl.name)
		if err != nil {
			return total, err
		}
		for _, id := range ids {
			n, err := deleteFlowChunks(db, tbl.name, tbl.isMeta, id, ` AND bucket < ?`, cutoff.Unix())
			total += n
			if err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// DeleteOrphanFlows removes data and meta rows (both resolutions) whose
// exporter_id is no longer in flow_exporters, in bounded chunks, and returns
// how many rows it removed.
func DeleteOrphanFlows(db *sql.DB) (int64, error) {
	live := map[int64]bool{}
	rows, err := db.Query(`SELECT id FROM flow_exporters`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		live[id] = true
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var total int64
	for _, tbl := range []struct {
		name   string
		isMeta bool
	}{{"flow_1m", false}, {"flow_1m_meta", true}, {"flow_1h", false}, {"flow_1h_meta", true}} {
		ids, err := flowTableExporterIDs(db, tbl.name)
		if err != nil {
			return total, err
		}
		for _, id := range ids {
			if live[id] {
				continue
			}
			n, err := deleteFlowChunks(db, tbl.name, tbl.isMeta, id, ``)
			total += n
			if err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// ---- templates ----

// FlowTemplate is one persisted v9 / IPFIX (options) template. Fields holds
// the blob: scope_count u16 BE, then per field type u16 (bit 15 =
// enterprise), length u16, pen u32 (all BE). Kind 0 = data, 1 = options.
type FlowTemplate struct {
	ExporterID int64
	Version    uint16
	ObsDomain  uint32
	TemplateID uint16
	Kind       int // 0 data, 1 options
	Fields     []byte
	UpdatedAt  time.Time
}

// UpsertFlowTemplate inserts or replaces one template.
func UpsertFlowTemplate(db *sql.DB, t FlowTemplate) error {
	fields := t.Fields
	if fields == nil {
		fields = []byte{}
	}
	_, err := db.Exec(
		`INSERT INTO flow_templates (exporter_id, version, obs_domain, template_id, kind, fields, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(exporter_id, version, obs_domain, template_id) DO UPDATE SET
		    kind = excluded.kind, fields = excluded.fields, updated_at = excluded.updated_at`,
		t.ExporterID, int64(t.Version), int64(t.ObsDomain), int64(t.TemplateID), t.Kind, fields, unixOf(t.UpdatedAt))
	return err
}

// LoadFlowTemplates returns the exporter's templates updated after newerThan,
// newest first (ties: version, obs_domain, template_id), at most limit of them
// (limit <= 0: all): the decoder stores a bounded number per exporter, and
// the newest are the ones the exporter still uses.
func LoadFlowTemplates(db *sql.DB, exporterID int64, newerThan time.Time, limit int) ([]FlowTemplate, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := db.Query(
		`SELECT exporter_id, version, obs_domain, template_id, kind, fields, updated_at FROM flow_templates
		 WHERE exporter_id = ? AND updated_at > ?
		 ORDER BY updated_at DESC, version, obs_domain, template_id LIMIT ?`,
		exporterID, unixOf(newerThan), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowTemplate
	for rows.Next() {
		var (
			t                   FlowTemplate
			version, obs, id, u int64
		)
		if err := rows.Scan(&t.ExporterID, &version, &obs, &id, &t.Kind, &t.Fields, &u); err != nil {
			return nil, err
		}
		t.Version, t.ObsDomain, t.TemplateID = uint16(version), uint32(obs), uint16(id)
		t.UpdatedAt = fromUnix(u)
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteFlowTemplates deletes every template of one exporter (reboot
// detected: its template ids may have been reassigned).
func DeleteFlowTemplates(db *sql.DB, exporterID int64) error {
	_, err := db.Exec(`DELETE FROM flow_templates WHERE exporter_id = ?`, exporterID)
	return err
}

// FlowTemplateKey identifies one persisted template of an exporter.
type FlowTemplateKey struct {
	Version    uint16
	ObsDomain  uint32
	TemplateID uint16
}

// DeleteFlowTemplateKeys deletes the given templates of one exporter (the
// exporter withdrew them).
func DeleteFlowTemplateKeys(db *sql.DB, exporterID int64, keys []FlowTemplateKey) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, k := range keys {
		if _, err := tx.Exec(`DELETE FROM flow_templates WHERE exporter_id = ? AND version = ? AND obs_domain = ? AND template_id = ?`,
			exporterID, int64(k.Version), int64(k.ObsDomain), int64(k.TemplateID)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TrimFlowTemplates keeps the exporter's keep most recently updated
// templates (ties: version, obs_domain, template_id) and deletes the rest,
// returning how many it removed: withdraw / re-announce churn must not grow
// the table without bound.
func TrimFlowTemplates(db *sql.DB, exporterID int64, keep int) (int64, error) {
	res, err := db.Exec(
		`DELETE FROM flow_templates WHERE exporter_id = ? AND (version, obs_domain, template_id) NOT IN (
		    SELECT version, obs_domain, template_id FROM flow_templates WHERE exporter_id = ?
		    ORDER BY updated_at DESC, version, obs_domain, template_id LIMIT ?)`,
		exporterID, exporterID, max(keep, 0))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOldFlowTemplates deletes templates with updated_at < cutoff.
func DeleteOldFlowTemplates(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM flow_templates WHERE updated_at < ?`, unixOf(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- address plan ----

// FlowPrefix is one address-plan row derived from managed devices'
// addresses.
type FlowPrefix struct {
	Prefix          netip.Prefix
	Class           string // internal | vpn | transit
	DeviceID, Iface string
	UpdatedAt       time.Time
}

// ReplaceFlowPrefixes swaps the whole table for ps in one transaction.
func ReplaceFlowPrefixes(db *sql.DB, ps []FlowPrefix) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM flow_prefixes`); err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO flow_prefixes (prefix, class, device_id, iface, updated_at) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range ps {
		if !p.Prefix.IsValid() {
			continue
		}
		if _, err := stmt.Exec(p.Prefix.Masked().String(), p.Class, p.DeviceID, p.Iface, unixOf(p.UpdatedAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListFlowPrefixes returns the address plan ordered by prefix; unparseable
// rows are skipped.
func ListFlowPrefixes(db *sql.DB) ([]FlowPrefix, error) {
	rows, err := db.Query(`SELECT prefix, class, device_id, iface, updated_at FROM flow_prefixes ORDER BY prefix`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlowPrefix
	for rows.Next() {
		var (
			p   FlowPrefix
			s   string
			upd int64
		)
		if err := rows.Scan(&s, &p.Class, &p.DeviceID, &p.Iface, &upd); err != nil {
			return nil, err
		}
		pfx, err := netip.ParsePrefix(s)
		if err != nil {
			continue
		}
		p.Prefix = pfx.Masked()
		p.UpdatedAt = fromUnix(upd)
		out = append(out, p)
	}
	return out, rows.Err()
}
