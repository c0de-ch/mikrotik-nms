package queries

import (
	"database/sql"
	"time"
)

// SpeedTest is a scheduled /tool/fetch download measurement run from a
// RouterOS device. DeviceID deliberately has no foreign key: a test must
// survive deletion of the device it pointed at (the poller records an error
// sample for a missing device instead of the test silently disappearing).
type SpeedTest struct {
	ID        string    `json:"id"`
	DeviceID  string    `json:"device_id"`
	URL       string    `json:"url"`
	Label     string    `json:"label"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// SpeedSample is the outcome of one measurement. Error != "" means the test
// could not produce a throughput figure (device offline/missing, dial failure,
// fetch trap/timeout/unfinished); such samples have Mbps = nil.
type SpeedSample struct {
	ID         int64     `json:"id"`
	TestID     string    `json:"test_id"`
	DeviceID   string    `json:"device_id"`
	Mbps       *float64  `json:"mbps"`
	Bytes      int64     `json:"bytes"`
	DurationMs int64     `json:"duration_ms"`
	Error      string    `json:"error"`
	RecordedAt time.Time `json:"recorded_at"`
}

func CreateSpeedTest(db *sql.DB, t *SpeedTest) error {
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	_, err := db.Exec(
		`INSERT INTO speed_tests (id, device_id, url, label, enabled)
		 VALUES (?, ?, ?, ?, ?)`,
		t.ID, t.DeviceID, t.URL, t.Label, enabled,
	)
	return err
}

const speedTestCols = `id, device_id, url, label, enabled, created_at`

func scanSpeedTest(scanner interface{ Scan(...interface{}) error }) (*SpeedTest, error) {
	t := &SpeedTest{}
	var enabled int
	err := scanner.Scan(&t.ID, &t.DeviceID, &t.URL, &t.Label, &enabled, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.Enabled = enabled == 1
	return t, nil
}

func scanSpeedTests(rows *sql.Rows) ([]SpeedTest, error) {
	var out []SpeedTest
	for rows.Next() {
		t, err := scanSpeedTest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func ListSpeedTests(db *sql.DB) ([]SpeedTest, error) {
	rows, err := db.Query(`SELECT ` + speedTestCols + ` FROM speed_tests ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSpeedTests(rows)
}

func ListEnabledSpeedTests(db *sql.DB) ([]SpeedTest, error) {
	rows, err := db.Query(`SELECT ` + speedTestCols + ` FROM speed_tests WHERE enabled = 1 ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSpeedTests(rows)
}

func GetSpeedTest(db *sql.DB, id string) (*SpeedTest, error) {
	row := db.QueryRow(`SELECT `+speedTestCols+` FROM speed_tests WHERE id = ?`, id)
	return scanSpeedTest(row)
}

func UpdateSpeedTest(db *sql.DB, t *SpeedTest) error {
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	_, err := db.Exec(
		`UPDATE speed_tests SET device_id=?, url=?, label=?, enabled=? WHERE id=?`,
		t.DeviceID, t.URL, t.Label, enabled, t.ID,
	)
	return err
}

func DeleteSpeedTest(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM speed_tests WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// InsertSpeedSample persists a sample, filling in RecordedAt (now, if unset)
// and ID so the same struct can be broadcast over WebSocket afterwards.
func InsertSpeedSample(db *sql.DB, s *SpeedSample) error {
	if s.RecordedAt.IsZero() {
		s.RecordedAt = time.Now().UTC()
	}
	res, err := db.Exec(
		`INSERT INTO speed_samples (test_id, device_id, mbps, bytes, duration_ms, error, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.TestID, s.DeviceID, s.Mbps, s.Bytes, s.DurationMs, s.Error, s.RecordedAt,
	)
	if err != nil {
		return err
	}
	s.ID, _ = res.LastInsertId()
	return nil
}

const speedSampleCols = `id, test_id, device_id, mbps, bytes, duration_ms, error, recorded_at`

func scanSpeedSample(scanner interface{ Scan(...interface{}) error }) (*SpeedSample, error) {
	s := &SpeedSample{}
	err := scanner.Scan(&s.ID, &s.TestID, &s.DeviceID, &s.Mbps, &s.Bytes, &s.DurationMs, &s.Error, &s.RecordedAt)
	return s, err
}

func GetSpeedSamples(db *sql.DB, testID string, from, to time.Time, limit int) ([]SpeedSample, error) {
	rows, err := db.Query(
		`SELECT `+speedSampleCols+` FROM speed_samples
		 WHERE test_id = ? AND recorded_at BETWEEN ? AND ?
		 ORDER BY recorded_at DESC LIMIT ?`,
		testID, from, to, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SpeedSample
	for rows.Next() {
		s, err := scanSpeedSample(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// GetLatestSpeedSamples returns the newest sample per test. One indexed
// reverse seek per test (the test list is tiny) instead of MAX(id) GROUP BY
// over speed_samples, which would walk the whole index on every test-list
// request and degrade linearly with retention.
func GetLatestSpeedSamples(db *sql.DB) (map[string]*SpeedSample, error) {
	rows, err := db.Query(`SELECT id FROM speed_tests`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var testIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		testIDs = append(testIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]*SpeedSample)
	for _, tid := range testIDs {
		row := db.QueryRow(
			`SELECT `+speedSampleCols+` FROM speed_samples
			 WHERE test_id = ? ORDER BY recorded_at DESC, id DESC LIMIT 1`, tid,
		)
		s, err := scanSpeedSample(row)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[s.TestID] = s
	}
	return out, nil
}

func DeleteOldSpeedSamples(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM speed_samples WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
