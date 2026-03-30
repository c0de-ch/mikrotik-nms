package queries

import (
	"database/sql"
	"time"
)

type Link struct {
	ID           string    `json:"id"`
	DeviceAID    string    `json:"device_a_id"`
	InterfaceA   string    `json:"interface_a"`
	DeviceBID    string    `json:"device_b_id"`
	InterfaceB   string    `json:"interface_b"`
	LinkType     string    `json:"link_type"`
	DiscoveredBy string    `json:"discovered_by"`
	Status       string    `json:"status"`
	LastSeen     time.Time `json:"last_seen"`
}

func UpsertLink(db *sql.DB, l *Link) error {
	_, err := db.Exec(
		`INSERT INTO links (id, device_a_id, interface_a, device_b_id, interface_b, link_type, discovered_by, status, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_a_id, interface_a, device_b_id, interface_b) DO UPDATE SET
		    link_type=excluded.link_type, discovered_by=excluded.discovered_by,
		    status=excluded.status, last_seen=CURRENT_TIMESTAMP`,
		l.ID, l.DeviceAID, l.InterfaceA, l.DeviceBID, l.InterfaceB,
		l.LinkType, l.DiscoveredBy, l.Status,
	)
	return err
}

func ListLinks(db *sql.DB) ([]Link, error) {
	rows, err := db.Query(
		`SELECT id, device_a_id, interface_a, device_b_id, interface_b, link_type, discovered_by, status, last_seen
		 FROM links ORDER BY last_seen DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.ID, &l.DeviceAID, &l.InterfaceA, &l.DeviceBID, &l.InterfaceB,
			&l.LinkType, &l.DiscoveredBy, &l.Status, &l.LastSeen); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

func MarkStaleLinksDown(db *sql.DB, cutoff time.Time) error {
	_, err := db.Exec(`UPDATE links SET status = 'down' WHERE last_seen < ? AND status = 'up'`, cutoff)
	return err
}

func DeleteOldLinks(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM links WHERE last_seen < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
