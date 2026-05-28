package queries

import (
	"database/sql"
	"time"
)

type MACLookup struct {
	MACAddress    string    `json:"mac_address"`
	IPAddress     string    `json:"ip_address"`
	HostName      string    `json:"host_name"`
	DNSName       string    `json:"dns_name"`
	Source        string    `json:"source"`
	DeviceName    string    `json:"device_name"`
	UpdatedAt     time.Time `json:"updated_at"`
	InterfaceName string    `json:"interface"`
	DeviceID      string    `json:"device_id"`
	AP            string    `json:"ap,omitempty"`
	SSID          string    `json:"ssid,omitempty"`
	Band          string    `json:"band,omitempty"`
	Channel       string    `json:"channel,omitempty"`
	Frequency     string    `json:"frequency,omitempty"`
	Signal        string    `json:"signal,omitempty"`
	TxRate        string    `json:"tx_rate,omitempty"`
	RxRate        string    `json:"rx_rate,omitempty"`
	Uptime        string    `json:"uptime,omitempty"`
}

func UpsertMACLookup(db *sql.DB, m *MACLookup) error {
	_, err := db.Exec(
		`INSERT INTO mac_lookup (mac_address, ip_address, host_name, dns_name, source, device_name,
		    interface_name, device_id, ap, ssid, band, channel, frequency, signal, tx_rate, rx_rate, uptime, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(mac_address) DO UPDATE SET
		    ip_address=CASE WHEN excluded.ip_address != '' THEN excluded.ip_address ELSE mac_lookup.ip_address END,
		    host_name=CASE WHEN excluded.host_name != '' THEN excluded.host_name ELSE mac_lookup.host_name END,
		    dns_name=CASE WHEN excluded.dns_name != '' THEN excluded.dns_name ELSE mac_lookup.dns_name END,
		    source=excluded.source, device_name=excluded.device_name,
		    interface_name=CASE WHEN excluded.interface_name != '' THEN excluded.interface_name ELSE mac_lookup.interface_name END,
		    device_id=CASE WHEN excluded.device_id != '' THEN excluded.device_id ELSE mac_lookup.device_id END,
		    ap=CASE WHEN excluded.ap != '' THEN excluded.ap ELSE mac_lookup.ap END,
		    ssid=CASE WHEN excluded.ssid != '' THEN excluded.ssid ELSE mac_lookup.ssid END,
		    band=CASE WHEN excluded.band != '' THEN excluded.band ELSE mac_lookup.band END,
		    channel=CASE WHEN excluded.channel != '' THEN excluded.channel ELSE mac_lookup.channel END,
		    frequency=CASE WHEN excluded.frequency != '' THEN excluded.frequency ELSE mac_lookup.frequency END,
		    signal=CASE WHEN excluded.signal != '' THEN excluded.signal ELSE mac_lookup.signal END,
		    tx_rate=CASE WHEN excluded.tx_rate != '' THEN excluded.tx_rate ELSE mac_lookup.tx_rate END,
		    rx_rate=CASE WHEN excluded.rx_rate != '' THEN excluded.rx_rate ELSE mac_lookup.rx_rate END,
		    uptime=CASE WHEN excluded.uptime != '' THEN excluded.uptime ELSE mac_lookup.uptime END,
		    updated_at=CURRENT_TIMESTAMP`,
		m.MACAddress, m.IPAddress, m.HostName, m.DNSName, m.Source, m.DeviceName,
		m.InterfaceName, m.DeviceID, m.AP, m.SSID, m.Band, m.Channel, m.Frequency, m.Signal, m.TxRate, m.RxRate, m.Uptime,
	)
	return err
}

const macLookupCols = `mac_address, ip_address, host_name, dns_name, source, device_name, updated_at,
	interface_name, device_id, ap, ssid, band, channel, frequency, signal, tx_rate, rx_rate, uptime`

func scanMACLookup(scanner interface{ Scan(...interface{}) error }) (*MACLookup, error) {
	m := &MACLookup{}
	err := scanner.Scan(&m.MACAddress, &m.IPAddress, &m.HostName, &m.DNSName, &m.Source, &m.DeviceName, &m.UpdatedAt,
		&m.InterfaceName, &m.DeviceID, &m.AP, &m.SSID, &m.Band, &m.Channel, &m.Frequency, &m.Signal, &m.TxRate, &m.RxRate, &m.Uptime)
	return m, err
}

func GetMACLookup(db *sql.DB, mac string) (*MACLookup, error) {
	row := db.QueryRow(`SELECT `+macLookupCols+` FROM mac_lookup WHERE mac_address=?`, mac)
	return scanMACLookup(row)
}

func GetAllMACLookups(db *sql.DB) (map[string]*MACLookup, error) {
	rows, err := db.Query(`SELECT ` + macLookupCols + ` FROM mac_lookup`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]*MACLookup)
	for rows.Next() {
		m, err := scanMACLookup(rows)
		if err != nil {
			return nil, err
		}
		result[m.MACAddress] = m
	}
	return result, rows.Err()
}

func GetAllMACLookupsSlice(db *sql.DB) ([]MACLookup, error) {
	rows, err := db.Query(`SELECT ` + macLookupCols + ` FROM mac_lookup ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []MACLookup
	for rows.Next() {
		m, err := scanMACLookup(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *m)
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

// DeleteStaleMACLookups prunes MAC cache entries not refreshed since cutoff.
// Without this the mac_lookup table (and the /mac-lookup and /clients/cached
// responses built from it) grows unbounded with every client ever seen.
func DeleteStaleMACLookups(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM mac_lookup WHERE updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
