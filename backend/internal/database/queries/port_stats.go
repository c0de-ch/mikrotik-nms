package queries

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// sqlTimeLayout is the text form SQLite's CURRENT_TIMESTAMP writes. The
// port-stats tables store their DATETIME columns in it (UTC), and every
// comparison binds a string in the same layout: modernc binds a time.Time as
// t.String(), which compares wrongly against this text when the server TZ is
// not UTC.
const sqlTimeLayout = "2006-01-02 15:04:05"

// sqlTime formats t for binding against a port-stats/port-hosts DATETIME column.
func sqlTime(t time.Time) string {
	return t.UTC().Format(sqlTimeLayout)
}

// dbTime scans a DATETIME value into a UTC time. modernc returns a time.Time
// for a plain DATETIME column but a string for an aggregate such as MIN(bucket)
// or a strftime(...) result, so both are accepted. NULL leaves Valid false.
type dbTime struct {
	Time  time.Time
	Valid bool
}

func (d *dbTime) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		d.Time, d.Valid = time.Time{}, false
		return nil
	case time.Time:
		d.Time, d.Valid = x.UTC(), true
		return nil
	case string:
		return d.parse(x)
	case []byte:
		return d.parse(string(x))
	}
	return fmt.Errorf("queries: cannot scan %T into a time", v)
}

func (d *dbTime) parse(s string) error {
	for _, layout := range []string{sqlTimeLayout, time.RFC3339Nano, "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			d.Time, d.Valid = t.UTC(), true
			return nil
		}
	}
	return fmt.Errorf("queries: unparseable time %q", s)
}

// PortStatsTable names one of the two port-stats resolutions.
type PortStatsTable string

const (
	PortStats1m PortStatsTable = "port_stats_1m"
	PortStats1h PortStatsTable = "port_stats_1h"
)

// ident returns the table name for interpolation into SQL. Table names cannot
// be bound as parameters, so anything but the two known tables is rejected.
func (t PortStatsTable) ident() (string, error) {
	switch t {
	case PortStats1m, PortStats1h:
		return string(t), nil
	}
	return "", fmt.Errorf("queries: unknown port stats table %q", string(t))
}

// PortStatRow is one (device, interface, bucket) row of port_stats_1m or
// port_stats_1h. Rates are bits/s (packets/s for *PpsAvg).
type PortStatRow struct {
	DeviceID           string
	InterfaceName      string
	Bucket             time.Time // UTC bucket start
	RxBpsAvg, TxBpsAvg int64
	RxBpsMax, TxBpsMax int64
	RxBytes, TxBytes   int64
	RxPpsAvg, TxPpsAvg int64
	Samples            int64
}

// InsertPortStats1m writes rows in ONE transaction (INSERT OR REPLACE on the
// PK). Buckets are truncated to the minute. Rows for a device that no longer
// exists (deleted since the collector sampled it) are skipped rather than
// failing the batch on the foreign key.
func InsertPortStats1m(db *sql.DB, rows []PortStatRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO port_stats_1m (device_id, interface_name, bucket,
		    rx_bps_avg, tx_bps_avg, rx_bps_max, tx_bps_max, rx_bytes, tx_bytes,
		    rx_pps_avg, tx_pps_avg, samples)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 WHERE EXISTS (SELECT 1 FROM devices WHERE id = ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.Exec(
			r.DeviceID, r.InterfaceName, sqlTime(r.Bucket.UTC().Truncate(time.Minute)),
			r.RxBpsAvg, r.TxBpsAvg, r.RxBpsMax, r.TxBpsMax, r.RxBytes, r.TxBytes,
			r.RxPpsAvg, r.TxPpsAvg, r.Samples,
			r.DeviceID,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RollupPortStats1h (re)computes port_stats_1h for every hour bucket h with
// from <= h < to (both truncated to the hour) from port_stats_1m. It is
// idempotent: re-rolling an hour replaces its rows. Per port and hour:
// avg = SUM(bytes)*8/3600 (minutes without a row count as idle),
// max = MAX(minute max), bytes = SUM, pps = SUM(minute pps_avg)/60,
// samples = SUM. Averages are rounded to the nearest integer.
func RollupPortStats1h(db *sql.DB, from, to time.Time) error {
	from = from.UTC().Truncate(time.Hour)
	to = to.UTC().Truncate(time.Hour)
	if !to.After(from) {
		return nil
	}
	_, err := db.Exec(
		`INSERT OR REPLACE INTO port_stats_1h (device_id, interface_name, bucket,
		    rx_bps_avg, tx_bps_avg, rx_bps_max, tx_bps_max, rx_bytes, tx_bytes,
		    rx_pps_avg, tx_pps_avg, samples)
		 SELECT device_id, interface_name, strftime('%Y-%m-%d %H:00:00', bucket) AS hour,
		        (SUM(rx_bytes) * 8 + 1800) / 3600, (SUM(tx_bytes) * 8 + 1800) / 3600,
		        MAX(rx_bps_max), MAX(tx_bps_max), SUM(rx_bytes), SUM(tx_bytes),
		        (SUM(rx_pps_avg) + 30) / 60, (SUM(tx_pps_avg) + 30) / 60, SUM(samples)
		 FROM port_stats_1m
		 WHERE bucket >= ? AND bucket < ?
		 GROUP BY device_id, interface_name, hour`,
		sqlTime(from), sqlTime(to))
	return err
}

// GetPortStatsSeries returns one port's rows with from <= bucket < to,
// ascending by bucket.
func GetPortStatsSeries(db *sql.DB, table PortStatsTable, deviceID, iface string, from, to time.Time) ([]PortStatRow, error) {
	name, err := table.ident()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT device_id, interface_name, bucket, rx_bps_avg, tx_bps_avg, rx_bps_max, tx_bps_max,
		        rx_bytes, tx_bytes, rx_pps_avg, tx_pps_avg, samples
		 FROM `+name+`
		 WHERE device_id = ? AND interface_name = ? AND bucket >= ? AND bucket < ?
		 ORDER BY bucket`,
		deviceID, iface, sqlTime(from), sqlTime(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PortStatRow
	for rows.Next() {
		var r PortStatRow
		var b dbTime
		if err := rows.Scan(&r.DeviceID, &r.InterfaceName, &b, &r.RxBpsAvg, &r.TxBpsAvg,
			&r.RxBpsMax, &r.TxBpsMax, &r.RxBytes, &r.TxBytes, &r.RxPpsAvg, &r.TxPpsAvg, &r.Samples); err != nil {
			return nil, err
		}
		r.Bucket = b.Time
		out = append(out, r)
	}
	return out, rows.Err()
}

// PortAggregate is one port's totals over a window.
type PortAggregate struct {
	DeviceID           string
	InterfaceName      string
	RxBytes, TxBytes   int64 // SUM
	RxBpsMax, TxBpsMax int64 // MAX of *_bps_max
}

// AggregatePortStats groups every port with rows in [from, to), ordered by
// device_id, interface_name. deviceID "" = all devices. With a device, ifaces
// (optional) limits the scan to those ports: the query then walks the full
// primary key (device, iface, bucket) and reads only the window, where
// device_id alone makes the planner read the device's whole retention.
// Rows of a port missing from ifaces are not returned.
func AggregatePortStats(db *sql.DB, table PortStatsTable, from, to time.Time, deviceID string, ifaces []string) ([]PortAggregate, error) {
	name, err := table.ident()
	if err != nil {
		return nil, err
	}
	q := `SELECT device_id, interface_name, SUM(rx_bytes), SUM(tx_bytes), MAX(rx_bps_max), MAX(tx_bps_max)
	      FROM ` + name + ` WHERE `
	var args []any
	if deviceID != "" {
		q += `device_id = ? AND `
		args = append(args, deviceID)
		if len(ifaces) > 0 {
			q += `interface_name IN (?` + strings.Repeat(`, ?`, len(ifaces)-1) + `) AND `
			for _, i := range ifaces {
				args = append(args, i)
			}
		}
	}
	q += `bucket >= ? AND bucket < ?
	      GROUP BY device_id, interface_name ORDER BY device_id, interface_name`
	args = append(args, sqlTime(from), sqlTime(to))

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PortAggregate
	for rows.Next() {
		var a PortAggregate
		if err := rows.Scan(&a.DeviceID, &a.InterfaceName, &a.RxBytes, &a.TxBytes, &a.RxBpsMax, &a.TxBpsMax); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PortStatsCoverageStart returns MIN(bucket) over the WHOLE table within
// [from, to) — fleet-wide, i.e. since when the collector has been writing.
// ok is false when the window has no rows.
func PortStatsCoverageStart(db *sql.DB, table PortStatsTable, from, to time.Time) (time.Time, bool, error) {
	name, err := table.ident()
	if err != nil {
		return time.Time{}, false, err
	}
	var b dbTime
	if err := db.QueryRow(
		`SELECT MIN(bucket) FROM `+name+` WHERE bucket >= ? AND bucket < ?`,
		sqlTime(from), sqlTime(to)).Scan(&b); err != nil {
		return time.Time{}, false, err
	}
	return b.Time, b.Valid, nil
}

// PortStatsPresentBuckets returns, ascending, the distinct buckets in
// [from, to) that hold a row for any port — the native buckets the collector
// was running for. MNDP/ARP chatter makes nearly every running port non-idle
// every minute, so a bucket without a single row fleet-wide means the
// collector was down (restart, deploy, outage), not that the network was
// silent. Served from the bucket index alone.
func PortStatsPresentBuckets(db *sql.DB, table PortStatsTable, from, to time.Time) ([]time.Time, error) {
	name, err := table.ident()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT DISTINCT bucket FROM `+name+` WHERE bucket >= ? AND bucket < ? ORDER BY bucket`,
		sqlTime(from), sqlTime(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var b dbTime
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if b.Valid {
			out = append(out, b.Time)
		}
	}
	return out, rows.Err()
}

// FirstCoveredMinute returns the first port_stats_1m bucket inside the hour
// starting at hour, for an hour the collector only covered in part (it
// started or restarted inside it). ok is false when the hour has no 1-minute
// rows, or when port_stats_1m no longer holds all of it — its fleet-wide
// bytes fall short of the hourly rollup's — so a retention cut inside the
// hour is not mistaken for a late start.
func FirstCoveredMinute(db *sql.DB, hour time.Time) (time.Time, bool, error) {
	hour = hour.UTC().Truncate(time.Hour)
	var minuteBytes, hourBytes int64
	var first dbTime
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(rx_bytes + tx_bytes), 0), MIN(bucket) FROM port_stats_1m WHERE bucket >= ? AND bucket < ?`,
		sqlTime(hour), sqlTime(hour.Add(time.Hour))).Scan(&minuteBytes, &first); err != nil {
		return time.Time{}, false, err
	}
	if !first.Valid {
		return time.Time{}, false, nil
	}
	if err := db.QueryRow(`SELECT COALESCE(SUM(rx_bytes + tx_bytes), 0) FROM port_stats_1h WHERE bucket = ?`,
		sqlTime(hour)).Scan(&hourBytes); err != nil {
		return time.Time{}, false, err
	}
	if minuteBytes < hourBytes {
		return time.Time{}, false, nil
	}
	return first.Time, true, nil
}

// LatestPortStatsBucket returns MAX(bucket) over the whole table; ok is false
// when the table is empty.
func LatestPortStatsBucket(db *sql.DB, table PortStatsTable) (time.Time, bool, error) {
	name, err := table.ident()
	if err != nil {
		return time.Time{}, false, err
	}
	var b dbTime
	if err := db.QueryRow(`SELECT MAX(bucket) FROM ` + name).Scan(&b); err != nil {
		return time.Time{}, false, err
	}
	return b.Time, b.Valid, nil
}

// portStatsDeleteChunk bounds how many rows one DELETE statement removes, and
// portStatsDeletePause is the pause between chunks. A single DELETE of a
// multi-million-row backlog (lowering port_stats_1m_days, the first sweep
// after a long outage, an admin purge) would hold SQLite's write lock for tens
// of seconds, and every other writer would fail with SQLITE_BUSY after the
// 5 s busy_timeout. Each chunk is its own short transaction; the pause lets
// waiting writers take the lock in between. Variables so tests can shrink
// them.
var (
	portStatsDeleteChunk = 5000
	portStatsDeletePause = 5 * time.Millisecond
)

// DeleteOldPortStats removes rows with bucket < cutoff, in bounded chunks
// (see portStatsDeleteChunk), and returns how many rows it removed.
func DeleteOldPortStats(db *sql.DB, table PortStatsTable, cutoff time.Time) (int64, error) {
	name, err := table.ident()
	if err != nil {
		return 0, err
	}
	// The tables are WITHOUT ROWID: select a chunk of primary keys through
	// the bucket index, then delete those rows by key.
	q := `DELETE FROM ` + name + ` WHERE (device_id, interface_name, bucket) IN (
	          SELECT device_id, interface_name, bucket FROM ` + name + ` WHERE bucket < ? LIMIT ?)`
	var total int64
	for {
		res, err := db.Exec(q, sqlTime(cutoff), portStatsDeleteChunk)
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

// ---- pure helpers (no DB) ----

// maxSeriesBuckets bounds how many buckets the pure helpers allocate, so a
// nonsensical from/to/step combination cannot allocate unbounded memory.
const maxSeriesBuckets = 200_000

// SeriesPoint is one chart point. A Gap point covers a span the collector
// was not running for: it marshals with null rates so charts draw a gap
// rather than a drop to zero.
type SeriesPoint struct {
	TS    time.Time
	RxBps int64
	TxBps int64
	Gap   bool
}

type seriesPointJSON struct {
	TS    time.Time `json:"ts"`
	RxBps *int64    `json:"rx_bps"`
	TxBps *int64    `json:"tx_bps"`
}

// MarshalJSON writes {"ts", "rx_bps", "tx_bps"}; the rates are null on a gap.
func (p SeriesPoint) MarshalJSON() ([]byte, error) {
	j := seriesPointJSON{TS: p.TS}
	if !p.Gap {
		j.RxBps, j.TxBps = &p.RxBps, &p.TxBps
	}
	return json.Marshal(j)
}

// UnmarshalJSON reads the MarshalJSON form; null rates mark a gap.
func (p *SeriesPoint) UnmarshalJSON(b []byte) error {
	var j seriesPointJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*p = SeriesPoint{TS: j.TS, Gap: j.RxBps == nil && j.TxBps == nil}
	if j.RxBps != nil {
		p.RxBps = *j.RxBps
	}
	if j.TxBps != nil {
		p.TxBps = *j.TxBps
	}
	return nil
}

// Coverage records which native buckets of a window the collector covered,
// so series and stats can tell "collector down" (a gap, left out of averages)
// from "port idle" (a zero).
type Coverage struct {
	Native  time.Duration  // bucket length of the table
	Present map[int64]bool // covered bucket starts, Unix seconds
	Start   time.Time      // nothing is covered before it; may fall inside the first covered bucket
	// Secs optionally overrides how many seconds of a present bucket were
	// covered (e.g. flow_1h_meta.minutes·60 for a partially covered hour);
	// buckets without an entry count in full.
	Secs map[int64]float64
}

// NewCoverage builds a Coverage from the present buckets (see
// PortStatsPresentBuckets) and the coverage start.
func NewCoverage(native time.Duration, present []time.Time, start time.Time) *Coverage {
	c := &Coverage{Native: native, Present: make(map[int64]bool, len(present)), Start: start.UTC()}
	for _, b := range present {
		c.Present[b.Unix()] = true
	}
	return c
}

// Seconds is how much of the native bucket starting at b was covered: 0 for
// a bucket without rows, less than Native for the bucket holding Start.
func (c *Coverage) Seconds(b time.Time) float64 {
	if c == nil || c.Native <= 0 || !c.Present[b.Unix()] {
		return 0
	}
	secs := c.Native.Seconds()
	if c.Start.After(b) {
		secs -= c.Start.Sub(b).Seconds()
	}
	if o, ok := c.Secs[b.Unix()]; ok {
		secs = min(secs, o)
	}
	return max(secs, 0)
}

// SecondsIn sums Seconds over the native buckets in [from, to) (from
// native-aligned).
func (c *Coverage) SecondsIn(from, to time.Time) float64 {
	if c == nil || c.Native <= 0 {
		return 0
	}
	var secs float64
	for i, b := 0, from; b.Before(to) && i < maxSeriesBuckets; i, b = i+1, b.Add(c.Native) {
		secs += c.Seconds(b)
	}
	return secs
}

// DownsampleSeries buckets rows into [from + k*step, from + (k+1)*step) for
// k = 0 .. (to-from)/step - 1 (step must be a multiple of the table's native
// resolution). Each point: TS = bucket start (UTC), Rx/TxBps = SUM(bytes)*8 /
// step.Seconds(), rounded. Buckets without rows are zero points (zero-filled).
// Rows outside the covered buckets are ignored. Never returns nil.
func DownsampleSeries(rows []PortStatRow, from, to time.Time, step time.Duration) []SeriesPoint {
	return DownsampleSeriesCovered(rows, from, to, step, nil)
}

// DownsampleSeriesCovered is DownsampleSeries that divides each point's bytes
// by the seconds the collector actually covered inside it (cov) rather than
// the whole step: a step the collector never covered is a Gap point, and a
// partly covered one (coverage starting mid-step, a restart) is not diluted.
// A nil cov means every step was fully covered.
func DownsampleSeriesCovered(rows []PortStatRow, from, to time.Time, step time.Duration, cov *Coverage) []SeriesPoint {
	if step <= 0 || !to.After(from) {
		return []SeriesPoint{}
	}
	n := min(int(to.Sub(from)/step), maxSeriesBuckets)
	if n <= 0 {
		return []SeriesPoint{}
	}
	end := from.Add(time.Duration(n) * step)
	rx := make([]int64, n)
	tx := make([]int64, n)
	for _, r := range rows {
		if r.Bucket.Before(from) || !r.Bucket.Before(end) {
			continue
		}
		k := int(r.Bucket.Sub(from) / step)
		rx[k] += r.RxBytes
		tx[k] += r.TxBytes
	}
	out := make([]SeriesPoint, n)
	for k := range out {
		ts := from.Add(time.Duration(k) * step).UTC()
		secs := step.Seconds()
		if cov != nil {
			secs = cov.SecondsIn(ts, ts.Add(step))
		}
		if secs <= 0 {
			out[k] = SeriesPoint{TS: ts, Gap: true}
			continue
		}
		out[k] = SeriesPoint{
			TS:    ts,
			RxBps: roundInt(float64(rx[k]) * 8 / secs),
			TxBps: roundInt(float64(tx[k]) * 8 / secs),
		}
	}
	return out
}

// SeriesStats summarises a port over a window.
type SeriesStats struct {
	RxAvg   int64 `json:"rx_avg"`
	TxAvg   int64 `json:"tx_avg"`
	RxMax   int64 `json:"rx_max"`
	TxMax   int64 `json:"tx_max"`
	RxP95   int64 `json:"rx_p95"`
	TxP95   int64 `json:"tx_p95"`
	RxBytes int64 `json:"rx_bytes"`
	TxBytes int64 `json:"tx_bytes"`
}

// ComputeSeriesStats summarises rows in [from, to) at native resolution:
// avg = SUM(bytes)*8 / (to-from).Seconds(); max = MAX(*_bps_max);
// p95 = Percentile95 over the ZERO-FILLED per-native-bucket rates
// ((to-from)/native values); bytes = SUM. from == to → zero stats.
//
// A bucket's rate is its *_bps_avg; a row that carries bytes but no avg (e.g.
// raw traffic_samples converted to rows) falls back to bytes*8/native. Two rows
// in the same native bucket keep the higher rate.
func ComputeSeriesStats(rows []PortStatRow, from, to time.Time, native time.Duration) SeriesStats {
	var s SeriesStats
	if !to.After(from) {
		return s
	}
	n := 0
	if native > 0 {
		n = int(to.Sub(from) / native)
	}
	zeroFill := n > 0 && n <= maxSeriesBuckets
	var rxVals, txVals []int64
	if zeroFill {
		rxVals = make([]int64, n)
		txVals = make([]int64, n)
	}

	for _, r := range rows {
		if r.Bucket.Before(from) || !r.Bucket.Before(to) {
			continue
		}
		s.RxBytes += r.RxBytes
		s.TxBytes += r.TxBytes
		s.RxMax = max(s.RxMax, r.RxBpsMax)
		s.TxMax = max(s.TxMax, r.TxBpsMax)

		rxRate := bucketRate(r.RxBpsAvg, r.RxBytes, native)
		txRate := bucketRate(r.TxBpsAvg, r.TxBytes, native)
		if !zeroFill {
			rxVals = append(rxVals, rxRate)
			txVals = append(txVals, txRate)
			continue
		}
		if k := int(r.Bucket.Sub(from) / native); k < n {
			rxVals[k] = max(rxVals[k], rxRate)
			txVals[k] = max(txVals[k], txRate)
		}
	}

	secs := to.Sub(from).Seconds()
	s.RxAvg = roundInt(float64(s.RxBytes) * 8 / secs)
	s.TxAvg = roundInt(float64(s.TxBytes) * 8 / secs)
	s.RxP95 = Percentile95(rxVals)
	s.TxP95 = Percentile95(txVals)
	return s
}

// ComputeSeriesStatsCovered is ComputeSeriesStats over only the buckets the
// collector covered (cov): the average divides by the covered seconds, and
// the percentile runs over the covered buckets (zero-filled where the port
// was idle), so collector downtime neither drags the average down nor fills
// the percentile with zeros. from must be native-aligned (the bucket holding
// cov.Start); a partly covered bucket's rate is its bytes over its covered
// seconds. A nil cov is ComputeSeriesStats.
func ComputeSeriesStatsCovered(rows []PortStatRow, from, to time.Time, native time.Duration, cov *Coverage) SeriesStats {
	if cov == nil {
		return ComputeSeriesStats(rows, from, to, native)
	}
	var s SeriesStats
	if !to.After(from) || native <= 0 {
		return s
	}
	n := min(int(to.Sub(from)/native), maxSeriesBuckets)
	rxRate := make(map[int]int64)
	txRate := make(map[int]int64)
	for _, r := range rows {
		if r.Bucket.Before(from) || !r.Bucket.Before(to) {
			continue
		}
		s.RxBytes += r.RxBytes
		s.TxBytes += r.TxBytes
		s.RxMax = max(s.RxMax, r.RxBpsMax)
		s.TxMax = max(s.TxMax, r.TxBpsMax)
		k := int(r.Bucket.Sub(from) / native)
		if k >= n {
			continue
		}
		rx, tx := bucketRate(r.RxBpsAvg, r.RxBytes, native), bucketRate(r.TxBpsAvg, r.TxBytes, native)
		if secs := cov.Seconds(r.Bucket); secs > 0 && secs < native.Seconds() {
			rx, tx = roundInt(float64(r.RxBytes)*8/secs), roundInt(float64(r.TxBytes)*8/secs)
		}
		rxRate[k], txRate[k] = max(rxRate[k], rx), max(txRate[k], tx)
	}
	var secs float64
	var rxVals, txVals []int64
	for k := 0; k < n; k++ {
		b := from.Add(time.Duration(k) * native)
		bs := cov.Seconds(b)
		if bs <= 0 {
			continue
		}
		secs += bs
		rxVals = append(rxVals, rxRate[k])
		txVals = append(txVals, txRate[k])
	}
	if secs > 0 {
		s.RxAvg = roundInt(float64(s.RxBytes) * 8 / secs)
		s.TxAvg = roundInt(float64(s.TxBytes) * 8 / secs)
	}
	s.RxP95 = Percentile95(rxVals)
	s.TxP95 = Percentile95(txVals)
	return s
}

// bucketRate is a bucket's bits/s for the percentile: the stored average, or
// bytes over the native bucket length when no average was recorded.
func bucketRate(avg, bytes int64, native time.Duration) int64 {
	if avg > 0 || bytes <= 0 || native <= 0 {
		return avg
	}
	return roundInt(float64(bytes) * 8 / native.Seconds())
}

// Percentile95 is the nearest-rank 95th percentile: sort ascending, take index
// ceil(0.95*n)-1. Empty → 0. The input is not modified.
func Percentile95(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := slices.Clone(vals)
	slices.Sort(sorted)
	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	return sorted[max(idx, 0)]
}

func roundInt(f float64) int64 {
	return int64(math.Round(f))
}
