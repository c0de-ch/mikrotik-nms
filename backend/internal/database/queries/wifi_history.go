package queries

import (
	"database/sql"
	"time"
)

type WifiHistoryEntry struct {
	ID           int64     `json:"id"`
	MACAddress   string    `json:"mac_address"`
	IPAddress    string    `json:"ip_address"`
	HostName     string    `json:"host_name"`
	APName       string    `json:"ap_name"`
	SSID         string    `json:"ssid"`
	Band         string    `json:"band"`
	Channel      string    `json:"channel"`
	Signal       string    `json:"signal"`
	TxRate       string    `json:"tx_rate"`
	RxRate       string    `json:"rx_rate"`
	Event        string    `json:"event"`
	ControllerID string    `json:"controller_id"`
	RecordedAt   time.Time `json:"recorded_at"`
}

func InsertWifiHistory(db *sql.DB, e *WifiHistoryEntry) error {
	_, err := db.Exec(
		`INSERT INTO wifi_history (mac_address, ip_address, host_name, ap_name, ssid, band, channel, signal, tx_rate, rx_rate, event, controller_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.MACAddress, e.IPAddress, e.HostName, e.APName, e.SSID, e.Band, e.Channel, e.Signal, e.TxRate, e.RxRate, e.Event, e.ControllerID,
	)
	return err
}

func GetWifiHistoryByMAC(db *sql.DB, mac string, limit int) ([]WifiHistoryEntry, error) {
	rows, err := db.Query(
		`SELECT id, mac_address, ip_address, host_name, ap_name, ssid, band, channel, signal, tx_rate, rx_rate, event, controller_id, recorded_at
		 FROM wifi_history WHERE mac_address=? ORDER BY recorded_at DESC LIMIT ?`, mac, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWifiHistory(rows)
}

func GetWifiHistoryByAP(db *sql.DB, ap string, limit int) ([]WifiHistoryEntry, error) {
	rows, err := db.Query(
		`SELECT id, mac_address, ip_address, host_name, ap_name, ssid, band, channel, signal, tx_rate, rx_rate, event, controller_id, recorded_at
		 FROM wifi_history WHERE ap_name=? ORDER BY recorded_at DESC LIMIT ?`, ap, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWifiHistory(rows)
}

func GetWifiHistoryRecent(db *sql.DB, limit int) ([]WifiHistoryEntry, error) {
	rows, err := db.Query(
		`SELECT id, mac_address, ip_address, host_name, ap_name, ssid, band, channel, signal, tx_rate, rx_rate, event, controller_id, recorded_at
		 FROM wifi_history ORDER BY recorded_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWifiHistory(rows)
}

// GetWifiClientsCurrentAP returns the latest AP for each active MAC.
func GetWifiClientsCurrentAP(db *sql.DB) ([]WifiHistoryEntry, error) {
	rows, err := db.Query(
		`SELECT w.id, w.mac_address, w.ip_address, w.host_name, w.ap_name, w.ssid, w.band, w.channel, w.signal, w.tx_rate, w.rx_rate, w.event, w.controller_id, w.recorded_at
		 FROM wifi_history w
		 INNER JOIN (SELECT mac_address, MAX(recorded_at) as max_time FROM wifi_history GROUP BY mac_address) latest
		 ON w.mac_address = latest.mac_address AND w.recorded_at = latest.max_time
		 WHERE w.event != 'leave'
		 ORDER BY w.ap_name, w.host_name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWifiHistory(rows)
}

func DeleteOldWifiHistory(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM wifi_history WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanWifiHistory(rows *sql.Rows) ([]WifiHistoryEntry, error) {
	var entries []WifiHistoryEntry
	for rows.Next() {
		var e WifiHistoryEntry
		if err := rows.Scan(&e.ID, &e.MACAddress, &e.IPAddress, &e.HostName, &e.APName, &e.SSID, &e.Band, &e.Channel, &e.Signal, &e.TxRate, &e.RxRate, &e.Event, &e.ControllerID, &e.RecordedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
