package queries

import (
	"database/sql"
	"time"
)

type TrafficSample struct {
	ID              int64     `json:"id"`
	DeviceID        string    `json:"device_id"`
	InterfaceName   string    `json:"interface_name"`
	RxBitsPerSec    int64     `json:"rx_bits_per_sec"`
	TxBitsPerSec    int64     `json:"tx_bits_per_sec"`
	RxPacketsPerSec int       `json:"rx_packets_per_sec"`
	TxPacketsPerSec int       `json:"tx_packets_per_sec"`
	CollectedAt     time.Time `json:"collected_at"`
}

func InsertTrafficSample(db *sql.DB, s *TrafficSample) error {
	_, err := db.Exec(
		`INSERT INTO traffic_samples (device_id, interface_name, rx_bits_per_sec, tx_bits_per_sec,
		    rx_packets_per_sec, tx_packets_per_sec, collected_at)
		 VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		s.DeviceID, s.InterfaceName, s.RxBitsPerSec, s.TxBitsPerSec,
		s.RxPacketsPerSec, s.TxPacketsPerSec,
	)
	return err
}

func GetTrafficSamples(db *sql.DB, deviceID, ifaceName string, from, to time.Time, limit int) ([]TrafficSample, error) {
	query := `SELECT id, device_id, interface_name, rx_bits_per_sec, tx_bits_per_sec,
	                 rx_packets_per_sec, tx_packets_per_sec, collected_at
	          FROM traffic_samples
	          WHERE device_id = ? AND interface_name = ? AND collected_at BETWEEN ? AND ?
	          ORDER BY collected_at DESC LIMIT ?`

	rows, err := db.Query(query, deviceID, ifaceName, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var samples []TrafficSample
	for rows.Next() {
		var s TrafficSample
		if err := rows.Scan(&s.ID, &s.DeviceID, &s.InterfaceName, &s.RxBitsPerSec, &s.TxBitsPerSec,
			&s.RxPacketsPerSec, &s.TxPacketsPerSec, &s.CollectedAt); err != nil {
			return nil, err
		}
		samples = append(samples, s)
	}
	return samples, rows.Err()
}

func DeleteOldTrafficSamples(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM traffic_samples WHERE collected_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
