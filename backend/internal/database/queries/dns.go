package queries

import (
	"database/sql"
	"time"
)

type DNSServer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	Port      int       `json:"port"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

func CreateDNSServer(db *sql.DB, s *DNSServer) error {
	_, err := db.Exec(
		`INSERT INTO dns_servers (id, name, address, port, enabled) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.Name, s.Address, s.Port, s.Enabled,
	)
	return err
}

func ListDNSServers(db *sql.DB) ([]DNSServer, error) {
	rows, err := db.Query(`SELECT id, name, address, port, enabled, created_at FROM dns_servers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []DNSServer
	for rows.Next() {
		var s DNSServer
		if err := rows.Scan(&s.ID, &s.Name, &s.Address, &s.Port, &s.Enabled, &s.CreatedAt); err != nil {
			return nil, err
		}
		servers = append(servers, s)
	}
	return servers, rows.Err()
}

func ListEnabledDNSServers(db *sql.DB) ([]DNSServer, error) {
	rows, err := db.Query(`SELECT id, name, address, port, enabled, created_at FROM dns_servers WHERE enabled=1 ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var servers []DNSServer
	for rows.Next() {
		var s DNSServer
		if err := rows.Scan(&s.ID, &s.Name, &s.Address, &s.Port, &s.Enabled, &s.CreatedAt); err != nil {
			return nil, err
		}
		servers = append(servers, s)
	}
	return servers, rows.Err()
}

func UpdateDNSServer(db *sql.DB, s *DNSServer) error {
	_, err := db.Exec(
		`UPDATE dns_servers SET name=?, address=?, port=?, enabled=? WHERE id=?`,
		s.Name, s.Address, s.Port, s.Enabled, s.ID,
	)
	return err
}

func DeleteDNSServer(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM dns_servers WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
