package queries

import (
	"database/sql"
	"time"
)

type MACLookup struct {
	MACAddress string    `json:"mac_address"`
	IPAddress  string    `json:"ip_address"`
	HostName   string    `json:"host_name"`
	DNSName    string    `json:"dns_name"`
	Source     string    `json:"source"`
	DeviceName string    `json:"device_name"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func UpsertMACLookup(db *sql.DB, m *MACLookup) error {
	_, err := db.Exec(
		`INSERT INTO mac_lookup (mac_address, ip_address, host_name, dns_name, source, device_name, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(mac_address) DO UPDATE SET
		    ip_address=CASE WHEN excluded.ip_address != '' THEN excluded.ip_address ELSE mac_lookup.ip_address END,
		    host_name=CASE WHEN excluded.host_name != '' THEN excluded.host_name ELSE mac_lookup.host_name END,
		    dns_name=CASE WHEN excluded.dns_name != '' THEN excluded.dns_name ELSE mac_lookup.dns_name END,
		    source=excluded.source, device_name=excluded.device_name, updated_at=CURRENT_TIMESTAMP`,
		m.MACAddress, m.IPAddress, m.HostName, m.DNSName, m.Source, m.DeviceName,
	)
	return err
}

func GetMACLookup(db *sql.DB, mac string) (*MACLookup, error) {
	m := &MACLookup{}
	err := db.QueryRow(
		`SELECT mac_address, ip_address, host_name, dns_name, source, device_name, updated_at FROM mac_lookup WHERE mac_address=?`, mac,
	).Scan(&m.MACAddress, &m.IPAddress, &m.HostName, &m.DNSName, &m.Source, &m.DeviceName, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func GetAllMACLookups(db *sql.DB) (map[string]*MACLookup, error) {
	rows, err := db.Query(`SELECT mac_address, ip_address, host_name, dns_name, source, device_name, updated_at FROM mac_lookup`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]*MACLookup)
	for rows.Next() {
		m := &MACLookup{}
		if err := rows.Scan(&m.MACAddress, &m.IPAddress, &m.HostName, &m.DNSName, &m.Source, &m.DeviceName, &m.UpdatedAt); err != nil {
			return nil, err
		}
		result[m.MACAddress] = m
	}
	return result, rows.Err()
}

type ClientHistoryEntry struct {
	ID         int64     `json:"id"`
	MACAddress string    `json:"mac_address"`
	IPAddress  string    `json:"ip_address"`
	HostName   string    `json:"host_name"`
	Source     string    `json:"source"`
	DeviceName string    `json:"device_name"`
	RecordedAt time.Time `json:"recorded_at"`
}

func InsertClientHistory(db *sql.DB, e *ClientHistoryEntry) error {
	_, err := db.Exec(
		`INSERT INTO client_history (mac_address, ip_address, host_name, source, device_name) VALUES (?, ?, ?, ?, ?)`,
		e.MACAddress, e.IPAddress, e.HostName, e.Source, e.DeviceName,
	)
	return err
}

func GetClientHistoryByMAC(db *sql.DB, mac string, limit int) ([]ClientHistoryEntry, error) {
	rows, err := db.Query(
		`SELECT id, mac_address, ip_address, host_name, source, device_name, recorded_at
		 FROM client_history WHERE mac_address=? ORDER BY recorded_at DESC LIMIT ?`, mac, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []ClientHistoryEntry
	for rows.Next() {
		var e ClientHistoryEntry
		if err := rows.Scan(&e.ID, &e.MACAddress, &e.IPAddress, &e.HostName, &e.Source, &e.DeviceName, &e.RecordedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func DeleteOldClientHistory(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM client_history WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
