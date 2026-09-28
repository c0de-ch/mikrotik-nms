package queries

import (
	"database/sql"
	"strings"
	"time"
)

// PortHost is one MAC learned behind a device port (bridge FDB, local=false).
type PortHost struct {
	DeviceID      string    `json:"device_id"`
	InterfaceName string    `json:"interface"`
	MACAddress    string    `json:"mac_address"`
	VID           int       `json:"vid"`
	Bridge        string    `json:"bridge"`
	FirstSeen     time.Time `json:"first_seen"`
	LastSeen      time.Time `json:"last_seen"`
}

// UpsertPortHosts stores one device's FDB dump in ONE transaction: new
// (interface, MAC) rows get first_seen = last_seen = seenAt, existing rows get
// vid, bridge and last_seen updated. DeviceID on each host is ignored (the
// deviceID argument wins). MACs are uppercased; rows without an interface or
// MAC are skipped; a duplicate (interface, MAC) in one dump keeps the last one.
// Rows the dump no longer contains are left alone — readers filter on
// last_seen and retention drops them once stale.
func UpsertPortHosts(db *sql.DB, deviceID string, hosts []PortHost, seenAt time.Time) error {
	type key struct{ iface, mac string }
	idx := make(map[key]int, len(hosts))
	deduped := make([]PortHost, 0, len(hosts))
	for _, h := range hosts {
		h.MACAddress = strings.ToUpper(strings.TrimSpace(h.MACAddress))
		if h.InterfaceName == "" || h.MACAddress == "" {
			continue
		}
		k := key{h.InterfaceName, h.MACAddress}
		if i, ok := idx[k]; ok {
			deduped[i] = h
			continue
		}
		idx[k] = len(deduped)
		deduped = append(deduped, h)
	}
	if len(deduped) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		`INSERT INTO port_hosts (device_id, interface_name, mac_address, vid, bridge, first_seen, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(device_id, interface_name, mac_address) DO UPDATE SET
		    vid = excluded.vid, bridge = excluded.bridge, last_seen = excluded.last_seen`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	seen := sqlTime(seenAt)
	for _, h := range deduped {
		if _, err := stmt.Exec(deviceID, h.InterfaceName, h.MACAddress, h.VID, h.Bridge, seen, seen); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListPortHosts returns rows with last_seen >= seenSince, ordered by
// device_id, interface_name, mac_address. deviceID "" = all devices.
func ListPortHosts(db *sql.DB, seenSince time.Time, deviceID string) ([]PortHost, error) {
	q := `SELECT device_id, interface_name, mac_address, vid, bridge, first_seen, last_seen
	      FROM port_hosts WHERE last_seen >= ?`
	args := []any{sqlTime(seenSince)}
	if deviceID != "" {
		q += ` AND device_id = ?`
		args = append(args, deviceID)
	}
	q += ` ORDER BY device_id, interface_name, mac_address`

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PortHost
	for rows.Next() {
		var h PortHost
		var first, last dbTime
		if err := rows.Scan(&h.DeviceID, &h.InterfaceName, &h.MACAddress, &h.VID, &h.Bridge, &first, &last); err != nil {
			return nil, err
		}
		h.FirstSeen, h.LastSeen = first.Time, last.Time
		out = append(out, h)
	}
	return out, rows.Err()
}

// DeleteStalePortHosts removes rows with last_seen < cutoff.
func DeleteStalePortHosts(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM port_hosts WHERE last_seen < ?`, sqlTime(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
