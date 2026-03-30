package queries

import (
	"database/sql"
	"time"
)

type Neighbor struct {
	ID                string    `json:"id"`
	DeviceID          string    `json:"device_id"`
	LocalInterface    string    `json:"local_interface"`
	NeighborAddress   string    `json:"neighbor_address"`
	NeighborMAC       string    `json:"neighbor_mac"`
	NeighborIdentity  string    `json:"neighbor_identity"`
	NeighborPlatform  string    `json:"neighbor_platform"`
	NeighborBoard     string    `json:"neighbor_board"`
	NeighborVersion   string    `json:"neighbor_version"`
	NeighborInterface string    `json:"neighbor_interface"`
	DiscoveredBy      string    `json:"discovered_by"`
	LastSeen          time.Time `json:"last_seen"`
}

func UpsertNeighbor(db *sql.DB, n *Neighbor) error {
	_, err := db.Exec(
		`INSERT INTO neighbors (id, device_id, local_interface, neighbor_address, neighbor_mac,
		    neighbor_identity, neighbor_platform, neighbor_board, neighbor_version,
		    neighbor_interface, discovered_by, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id, local_interface, neighbor_mac) DO UPDATE SET
		    neighbor_address=excluded.neighbor_address, neighbor_identity=excluded.neighbor_identity,
		    neighbor_platform=excluded.neighbor_platform, neighbor_board=excluded.neighbor_board,
		    neighbor_version=excluded.neighbor_version, neighbor_interface=excluded.neighbor_interface,
		    discovered_by=excluded.discovered_by, last_seen=CURRENT_TIMESTAMP`,
		n.ID, n.DeviceID, n.LocalInterface, n.NeighborAddress, n.NeighborMAC,
		n.NeighborIdentity, n.NeighborPlatform, n.NeighborBoard, n.NeighborVersion,
		n.NeighborInterface, n.DiscoveredBy,
	)
	return err
}

func ListNeighborsByDevice(db *sql.DB, deviceID string) ([]Neighbor, error) {
	rows, err := db.Query(
		`SELECT id, device_id, local_interface, neighbor_address, neighbor_mac,
		        neighbor_identity, neighbor_platform, neighbor_board, neighbor_version,
		        neighbor_interface, discovered_by, last_seen
		 FROM neighbors WHERE device_id = ? ORDER BY local_interface`, deviceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var neighbors []Neighbor
	for rows.Next() {
		var n Neighbor
		if err := rows.Scan(&n.ID, &n.DeviceID, &n.LocalInterface, &n.NeighborAddress, &n.NeighborMAC,
			&n.NeighborIdentity, &n.NeighborPlatform, &n.NeighborBoard, &n.NeighborVersion,
			&n.NeighborInterface, &n.DiscoveredBy, &n.LastSeen); err != nil {
			return nil, err
		}
		neighbors = append(neighbors, n)
	}
	return neighbors, rows.Err()
}

func ListAllNeighbors(db *sql.DB) ([]Neighbor, error) {
	rows, err := db.Query(
		`SELECT id, device_id, local_interface, neighbor_address, neighbor_mac,
		        neighbor_identity, neighbor_platform, neighbor_board, neighbor_version,
		        neighbor_interface, discovered_by, last_seen
		 FROM neighbors ORDER BY device_id, local_interface`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var neighbors []Neighbor
	for rows.Next() {
		var n Neighbor
		if err := rows.Scan(&n.ID, &n.DeviceID, &n.LocalInterface, &n.NeighborAddress, &n.NeighborMAC,
			&n.NeighborIdentity, &n.NeighborPlatform, &n.NeighborBoard, &n.NeighborVersion,
			&n.NeighborInterface, &n.DiscoveredBy, &n.LastSeen); err != nil {
			return nil, err
		}
		neighbors = append(neighbors, n)
	}
	return neighbors, rows.Err()
}

func DeleteStaleNeighbors(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM neighbors WHERE last_seen < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
