package queries

import (
	"database/sql"
	"fmt"
	"math"
	"time"
)

// Why a device-minute has no counter coverage (device_minutes.gap_reason).
// The first that applies wins, in this order.
const (
	// GapPaused: an admin pause was in force.
	GapPaused = "paused"
	// GapUnreachable: the liveness check did not see the device up at any
	// time in the minute.
	GapUnreachable = "unreachable"
	// GapReadFailed: the counter read was sent and failed or timed out.
	GapReadFailed = "read_failed"
	// GapNoSession: the device answered liveness, but the counter read could
	// not start — no API session (a refused login, the redial cooldown).
	GapNoSession = "no_session"
	// GapNoBaseline: a counter read succeeded, but no earlier read pairs with
	// it into an interval (the first read after a start, a pause, an outage).
	GapNoBaseline = "no_baseline"
	// GapNotPolled: nothing was read in the minute and none of the above
	// explains it (the collector had not reached the device yet).
	GapNotPolled = "not_polled"
)

// DeviceMinute is one device_minutes row: a device's counter coverage, CPU
// and memory telemetry and the NMS's own cost for one minute.
type DeviceMinute struct {
	DeviceID string
	Bucket   time.Time // UTC minute start
	// PolledS is how many seconds of the minute lie inside a usable interval
	// between two Tier 0 reads; > 0 makes the minute covered.
	PolledS    int64
	Tier0OK    int64 // Tier 0 reads that succeeded in the minute
	ReachableS int64 // seconds the liveness check saw the device up
	Paused     bool
	GapReason  string // "" (NULL) when covered
	// Quiet-window CPU samples: CPUSum and CPUMax are meaningful while
	// CPUSamples > 0 (stored NULL otherwise).
	CPUSum, CPUSamples, CPUMax int64
	CPUPost                    sql.NullInt64 // CPU 3 s after a Tier 0 read
	FreeMemoryMin              sql.NullInt64 // bytes
	UptimeS                    sql.NullInt64
	Rebooted                   bool
	NMSCmds, NMSWallMs         int64
	NMSRows, NMSTimeouts       int64
	NMSReconnects              int64
}

// Covered reports whether the device's counters cover (part of) the minute.
func (m DeviceMinute) Covered() bool { return m.PolledS > 0 }

// CPUAvg is the minute's quiet-window CPU average; ok is false without a
// sample.
func (m DeviceMinute) CPUAvg() (float64, bool) {
	if m.CPUSamples <= 0 {
		return 0, false
	}
	return float64(m.CPUSum) / float64(m.CPUSamples), true
}

func nullIf(valid bool, v int64) any {
	if !valid {
		return nil
	}
	return v
}

// UpsertDeviceMinutes writes rows in one transaction, replacing a minute
// already stored. Rows of a device that no longer exists are skipped.
func UpsertDeviceMinutes(db *sql.DB, rows []DeviceMinute) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO device_minutes (device_id, bucket, polled_s, tier0_ok, reachable_s, paused, gap_reason,
		    cpu_sum, cpu_samples, cpu_max, cpu_post, free_memory_min, uptime_s, rebooted,
		    nms_cmds, nms_wall_ms, nms_rows, nms_timeouts, nms_reconnects)
		 SELECT ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM devices WHERE id = ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		sampled := r.CPUSamples > 0
		if _, err := stmt.Exec(
			r.DeviceID, r.Bucket.UTC().Truncate(time.Minute).Unix(), r.PolledS, r.Tier0OK, r.ReachableS, boolInt(r.Paused), r.GapReason,
			nullIf(sampled, r.CPUSum), r.CPUSamples, nullIf(sampled, r.CPUMax), r.CPUPost, r.FreeMemoryMin, r.UptimeS, boolInt(r.Rebooted),
			r.NMSCmds, r.NMSWallMs, r.NMSRows, r.NMSTimeouts, r.NMSReconnects,
			r.DeviceID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const deviceMinuteColumns = `device_id, bucket, polled_s, tier0_ok, reachable_s, paused, COALESCE(gap_reason, ''),
	COALESCE(cpu_sum, 0), cpu_samples, COALESCE(cpu_max, 0), cpu_post, free_memory_min, uptime_s, rebooted,
	nms_cmds, nms_wall_ms, nms_rows, nms_timeouts, nms_reconnects`

func listDeviceMinutes(db *sql.DB, query string, args ...any) ([]DeviceMinute, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceMinute
	for rows.Next() {
		var m DeviceMinute
		var bucket int64
		if err := rows.Scan(&m.DeviceID, &bucket, &m.PolledS, &m.Tier0OK, &m.ReachableS, &m.Paused, &m.GapReason,
			&m.CPUSum, &m.CPUSamples, &m.CPUMax, &m.CPUPost, &m.FreeMemoryMin, &m.UptimeS, &m.Rebooted,
			&m.NMSCmds, &m.NMSWallMs, &m.NMSRows, &m.NMSTimeouts, &m.NMSReconnects); err != nil {
			return nil, err
		}
		m.Bucket = time.Unix(bucket, 0).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListDeviceMinutes returns one device's rows with from <= bucket < to,
// ascending.
func ListDeviceMinutes(db *sql.DB, deviceID string, from, to time.Time) ([]DeviceMinute, error) {
	return listDeviceMinutes(db, `SELECT `+deviceMinuteColumns+` FROM device_minutes
		WHERE device_id = ? AND bucket >= ? AND bucket < ? ORDER BY bucket`, deviceID, from.Unix(), to.Unix())
}

// ListDeviceMinutesSince returns every device's rows with bucket >= from,
// ordered by device, then bucket (the alert rules' input).
func ListDeviceMinutesSince(db *sql.DB, from time.Time) ([]DeviceMinute, error) {
	return listDeviceMinutes(db, `SELECT `+deviceMinuteColumns+` FROM device_minutes
		WHERE bucket >= ? ORDER BY device_id, bucket`, from.Unix())
}

// LastDeviceUptime returns the newest uptime a device reported and when, so
// a restarted NMS can still tell that the device rebooted meanwhile. ok is
// false when none is stored.
func LastDeviceUptime(db *sql.DB, deviceID string) (uptimeS int64, at time.Time, ok bool, err error) {
	var bucket int64
	err = db.QueryRow(`SELECT uptime_s, bucket FROM device_minutes
		WHERE device_id = ? AND uptime_s IS NOT NULL ORDER BY bucket DESC LIMIT 1`, deviceID).Scan(&uptimeS, &bucket)
	if err == sql.ErrNoRows {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return uptimeS, time.Unix(bucket, 0).UTC(), true, nil
}

// UpdateDeviceLoad stores a load sample on the device row: the CPU load, and
// the memory in use and uptime when the sample carries them (nil / ""
// leaves the stored value). It never touches status, last_seen or
// last_error — those belong to the liveness check.
func UpdateDeviceLoad(db *sql.DB, id string, cpuLoad int, memUsed *int64, uptime string) error {
	_, err := db.Exec(
		`UPDATE devices SET cpu_load = ?, memory_used = COALESCE(?, memory_used),
		        uptime = COALESCE(NULLIF(?, ''), uptime), updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		cpuLoad, memUsed, uptime, id)
	return err
}

// ---- coverage --------------------------------------------------------------

// DeviceLoadTable names one of the two device-load resolutions.
type DeviceLoadTable string

const (
	DeviceMinutes DeviceLoadTable = "device_minutes"
	DeviceHours   DeviceLoadTable = "device_hours"
)

// idents returns the table name and its covered-seconds column for
// interpolation into SQL (neither can be bound as a parameter).
func (t DeviceLoadTable) idents() (table, covered string, err error) {
	switch t {
	case DeviceMinutes:
		return "device_minutes", "polled_s", nil
	case DeviceHours:
		return "device_hours", "covered_s", nil
	}
	return "", "", fmt.Errorf("queries: unknown device load table %q", string(t))
}

// DeviceCoverage is what a reader needs to tell a covered bucket of one
// device from a gap.
type DeviceCoverage struct {
	// First is the device's first row in the table, at any time: buckets
	// before it predate the telemetry and are judged the old way. HasFirst
	// is false when the device has no row at all.
	First    time.Time
	HasFirst bool
	// Secs holds the covered seconds of every bucket in the window that has
	// any (bucket start in unix seconds); a bucket missing here is a gap.
	Secs map[int64]int64
}

// GetDeviceCoverage reads one device's coverage of the buckets in [from, to).
func GetDeviceCoverage(db *sql.DB, t DeviceLoadTable, deviceID string, from, to time.Time) (DeviceCoverage, error) {
	c := DeviceCoverage{Secs: map[int64]int64{}}
	table, col, err := t.idents()
	if err != nil {
		return c, err
	}
	var first sql.NullInt64
	if err := db.QueryRow(`SELECT MIN(bucket) FROM `+table+` WHERE device_id = ?`, deviceID).Scan(&first); err != nil {
		return c, err
	}
	if !first.Valid {
		return c, nil
	}
	c.First, c.HasFirst = time.Unix(first.Int64, 0).UTC(), true
	rows, err := db.Query(`SELECT bucket, `+col+` FROM `+table+`
		WHERE device_id = ? AND bucket >= ? AND bucket < ? AND `+col+` > 0`, deviceID, from.Unix(), to.Unix())
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var b, s int64
		if err := rows.Scan(&b, &s); err != nil {
			return c, err
		}
		c.Secs[b] = s
	}
	return c, rows.Err()
}

// DeviceCoverageSum is one device's coverage of a window in total.
type DeviceCoverageSum struct {
	First    time.Time // as DeviceCoverage.First
	HasFirst bool
	Secs     int64 // covered seconds inside the window
}

// GetFleetCoverage returns, for every managed device, its first row in the
// table and its covered seconds in [from, to).
func GetFleetCoverage(db *sql.DB, t DeviceLoadTable, from, to time.Time) (map[string]DeviceCoverageSum, error) {
	table, col, err := t.idents()
	if err != nil {
		return nil, err
	}
	out := map[string]DeviceCoverageSum{}
	// MIN(bucket) per device is one seek on the primary key each.
	rows, err := db.Query(`SELECT d.id, (SELECT MIN(bucket) FROM ` + table + ` m WHERE m.device_id = d.id) FROM devices d`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var first sql.NullInt64
		if err := rows.Scan(&id, &first); err != nil {
			rows.Close()
			return nil, err
		}
		c := DeviceCoverageSum{}
		if first.Valid {
			c.First, c.HasFirst = time.Unix(first.Int64, 0).UTC(), true
		}
		out[id] = c
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// Served from the (bucket, covered) index alone.
	sums, err := db.Query(`SELECT device_id, SUM(`+col+`) FROM `+table+`
		WHERE bucket >= ? AND bucket < ? GROUP BY device_id`, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer sums.Close()
	for sums.Next() {
		var id string
		var secs sql.NullInt64
		if err := sums.Scan(&id, &secs); err != nil {
			return nil, err
		}
		if c, ok := out[id]; ok {
			c.Secs = secs.Int64
			out[id] = c
		}
	}
	return out, sums.Err()
}

// ---- hourly rollup ---------------------------------------------------------

// DeviceHour is one device_hours row.
type DeviceHour struct {
	DeviceID   string
	Bucket     time.Time // UTC hour start
	CoveredS   int64     // SUM(polled_s)
	CPUAvg     sql.NullFloat64
	CPUMax     sql.NullInt64
	CPUP95     sql.NullInt64 // nearest-rank p95 of the minute averages
	MemFreeMin sql.NullInt64
	NMSCmds    int64
	NMSCostMs  int64
	ROSVersion string
}

// hourAcc accumulates one device-hour of minute rows.
type hourAcc struct {
	covered, cmds      int64
	cpuSum, cpuSamples int64
	cpuMax             int64
	minuteAvgs         []int64
	memMin             sql.NullInt64
}

// RollupDeviceHours (re)computes device_hours for every hour h with from <= h
// < to (both truncated to the hour) from device_minutes. It is idempotent:
// re-rolling an hour replaces its figures, except the RouterOS version, which
// stays the one the hour was first rolled with. An hour without minute rows
// gets no row (the NMS was not running).
func RollupDeviceHours(db *sql.DB, from, to time.Time) error {
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC().Truncate(time.Hour)
	if !to.After(from) {
		return nil
	}
	rows, err := db.Query(`SELECT device_id, bucket, polled_s, COALESCE(cpu_sum, 0), cpu_samples, COALESCE(cpu_max, 0),
		        free_memory_min, nms_cmds
		 FROM device_minutes WHERE bucket >= ? AND bucket < ?`, from.Unix(), to.Unix())
	if err != nil {
		return err
	}
	type key struct {
		device string
		hour   int64
	}
	accs := map[key]*hourAcc{}
	var order []key
	for rows.Next() {
		var id string
		var bucket, polled, cpuSum, cpuSamples, cpuMax, cmds int64
		var mem sql.NullInt64
		if err := rows.Scan(&id, &bucket, &polled, &cpuSum, &cpuSamples, &cpuMax, &mem, &cmds); err != nil {
			rows.Close()
			return err
		}
		k := key{id, bucket - bucket%3600}
		a := accs[k]
		if a == nil {
			a = &hourAcc{}
			accs[k] = a
			order = append(order, k)
		}
		a.covered += polled
		a.cmds += cmds
		if cpuSamples > 0 {
			a.cpuSum += cpuSum
			a.cpuSamples += cpuSamples
			a.cpuMax = max(a.cpuMax, cpuMax)
			a.minuteAvgs = append(a.minuteAvgs, int64(math.Round(float64(cpuSum)/float64(cpuSamples))))
		}
		if mem.Valid && (!a.memMin.Valid || mem.Int64 < a.memMin.Int64) {
			a.memMin = mem
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(order) == 0 {
		return nil
	}

	versions := map[string]string{}
	vrows, err := db.Query(`SELECT id, ros_version FROM devices`)
	if err != nil {
		return err
	}
	for vrows.Next() {
		var id, v string
		if err := vrows.Scan(&id, &v); err != nil {
			vrows.Close()
			return err
		}
		versions[id] = v
	}
	if err := vrows.Close(); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(
		`INSERT INTO device_hours (device_id, bucket, covered_s, cpu_avg, cpu_max, cpu_p95, mem_free_min, nms_cmds, nms_cost_ms, ros_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULLIF(?, ''))
		 ON CONFLICT(device_id, bucket) DO UPDATE SET
		    covered_s = excluded.covered_s, cpu_avg = excluded.cpu_avg, cpu_max = excluded.cpu_max,
		    cpu_p95 = excluded.cpu_p95, mem_free_min = excluded.mem_free_min, nms_cmds = excluded.nms_cmds,
		    ros_version = COALESCE(device_hours.ros_version, excluded.ros_version)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, k := range order {
		a := accs[k]
		var avg, cpuMax, p95 any
		if a.cpuSamples > 0 {
			avg = float64(a.cpuSum) / float64(a.cpuSamples)
			cpuMax = a.cpuMax
			p95 = Percentile95(a.minuteAvgs)
		}
		if _, err := stmt.Exec(k.device, k.hour, a.covered, avg, cpuMax, p95, a.memMin, a.cmds, versions[k.device]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListDeviceHours returns one device's rows with from <= bucket < to,
// ascending.
func ListDeviceHours(db *sql.DB, deviceID string, from, to time.Time) ([]DeviceHour, error) {
	rows, err := db.Query(`SELECT device_id, bucket, COALESCE(covered_s, 0), cpu_avg, cpu_max, cpu_p95, mem_free_min,
		        COALESCE(nms_cmds, 0), COALESCE(nms_cost_ms, 0), COALESCE(ros_version, '')
		 FROM device_hours WHERE device_id = ? AND bucket >= ? AND bucket < ? ORDER BY bucket`,
		deviceID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceHour
	for rows.Next() {
		var h DeviceHour
		var bucket int64
		if err := rows.Scan(&h.DeviceID, &bucket, &h.CoveredS, &h.CPUAvg, &h.CPUMax, &h.CPUP95, &h.MemFreeMin,
			&h.NMSCmds, &h.NMSCostMs, &h.ROSVersion); err != nil {
			return nil, err
		}
		h.Bucket = time.Unix(bucket, 0).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

// LatestDeviceLoadBucket returns MAX(bucket) over the whole table; ok is
// false when it is empty.
func LatestDeviceLoadBucket(db *sql.DB, t DeviceLoadTable) (time.Time, bool, error) {
	table, _, err := t.idents()
	if err != nil {
		return time.Time{}, false, err
	}
	var b sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(bucket) FROM ` + table).Scan(&b); err != nil {
		return time.Time{}, false, err
	}
	if !b.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(b.Int64, 0).UTC(), true, nil
}

// ---- retention -------------------------------------------------------------

// deleteDeviceLoadChunks runs a chunked DELETE (the port-stats chunk size and
// pause: each chunk is its own short transaction, so a large backlog never
// holds the write lock for long) and returns the rows removed.
func deleteDeviceLoadChunks(db *sql.DB, table, where string, args ...any) (int64, error) {
	q := `DELETE FROM ` + table + ` WHERE (device_id, bucket) IN (
	          SELECT device_id, bucket FROM ` + table + ` WHERE ` + where + ` LIMIT ?)`
	args = append(args, portStatsDeleteChunk)
	var total int64
	for {
		res, err := db.Exec(q, args...)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(portStatsDeleteChunk) {
			return total, nil
		}
		time.Sleep(portStatsDeletePause)
	}
}

// DeleteOldDeviceLoad removes rows with bucket < cutoff, in bounded chunks,
// and returns how many it removed.
func DeleteOldDeviceLoad(db *sql.DB, t DeviceLoadTable, cutoff time.Time) (int64, error) {
	table, _, err := t.idents()
	if err != nil {
		return 0, err
	}
	return deleteDeviceLoadChunks(db, table, `bucket < ?`, cutoff.Unix())
}

// DeleteOrphanDeviceLoad removes the rows (both resolutions) of devices that
// no longer exist — the tables carry no foreign key — in bounded chunks.
func DeleteOrphanDeviceLoad(db *sql.DB) (int64, error) {
	var total int64
	for _, table := range []string{"device_minutes", "device_hours"} {
		ids, err := orphanDeviceLoadIDs(db, table)
		if err != nil {
			return total, err
		}
		for _, id := range ids {
			n, err := deleteDeviceLoadChunks(db, table, `device_id = ?`, id)
			total += n
			if err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// orphanDeviceLoadIDs lists the device ids in table that devices no longer
// has. The distinct ids are found by hopping along the primary key (one seek
// per device), not by scanning every row.
func orphanDeviceLoadIDs(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`WITH RECURSIVE ids(id) AS (
		    SELECT MIN(device_id) FROM ` + table + `
		    UNION ALL
		    SELECT (SELECT MIN(device_id) FROM ` + table + ` WHERE device_id > ids.id) FROM ids WHERE ids.id IS NOT NULL)
		 SELECT id FROM ids WHERE id IS NOT NULL AND id NOT IN (SELECT id FROM devices)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
