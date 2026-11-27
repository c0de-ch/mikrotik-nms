package queries

import (
	"database/sql"
	"time"
)

// MapPosition is where a node of the network map was placed.
type MapPosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// ListMapPositions returns every saved node position and the time of the
// newest change (zero when none is saved).
func ListMapPositions(db *sql.DB) (map[string]MapPosition, time.Time, error) {
	rows, err := db.Query(`SELECT node_id, x, y, updated_at FROM map_positions`)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	out := make(map[string]MapPosition)
	var newest int64
	for rows.Next() {
		var id string
		var p MapPosition
		var at int64
		if err := rows.Scan(&id, &p.X, &p.Y, &at); err != nil {
			return nil, time.Time{}, err
		}
		out[id] = p
		newest = max(newest, at)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, err
	}
	var t time.Time
	if newest > 0 {
		t = time.Unix(newest, 0).UTC()
	}
	return out, t, nil
}

// ReplaceMapPositions makes positions the saved layout: every node in it is
// stored, every other row removed, in one transaction.
func ReplaceMapPositions(db *sql.DB, positions map[string]MapPosition, by string, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM map_positions`); err != nil {
		return err
	}
	for id, p := range positions {
		if _, err := tx.Exec(
			`INSERT INTO map_positions (node_id, x, y, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)`,
			id, p.X, p.Y, now.Unix(), by,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearMapPositions removes the saved layout.
func ClearMapPositions(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM map_positions`)
	return err
}
