package queries

import (
	"database/sql"
	"time"
)

type Interface struct {
	ID          string    `json:"id"`
	DeviceID    string    `json:"device_id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	MACAddress  string    `json:"mac_address"`
	MTU         *int      `json:"mtu"`
	Running     bool      `json:"running"`
	Disabled    bool      `json:"disabled"`
	Comment     string    `json:"comment"`
	LastUpdated time.Time `json:"last_updated"`
}

func UpsertInterface(db *sql.DB, iface *Interface) error {
	_, err := db.Exec(
		`INSERT INTO interfaces (id, device_id, name, type, mac_address, mtu, running, disabled, comment, last_updated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id, name) DO UPDATE SET
		    type=excluded.type, mac_address=excluded.mac_address, mtu=excluded.mtu,
		    running=excluded.running, disabled=excluded.disabled, comment=excluded.comment,
		    last_updated=CURRENT_TIMESTAMP`,
		iface.ID, iface.DeviceID, iface.Name, iface.Type, iface.MACAddress,
		iface.MTU, iface.Running, iface.Disabled, iface.Comment,
	)
	return err
}

func ListInterfacesByDevice(db *sql.DB, deviceID string) ([]Interface, error) {
	rows, err := db.Query(
		`SELECT id, device_id, name, type, mac_address, mtu, running, disabled, comment, last_updated
		 FROM interfaces WHERE device_id = ? ORDER BY name`, deviceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ifaces []Interface
	for rows.Next() {
		var i Interface
		if err := rows.Scan(&i.ID, &i.DeviceID, &i.Name, &i.Type, &i.MACAddress,
			&i.MTU, &i.Running, &i.Disabled, &i.Comment, &i.LastUpdated); err != nil {
			return nil, err
		}
		ifaces = append(ifaces, i)
	}
	return ifaces, rows.Err()
}

func GetInterfaceByMAC(db *sql.DB, mac string) (*Interface, error) {
	i := &Interface{}
	err := db.QueryRow(
		`SELECT id, device_id, name, type, mac_address, mtu, running, disabled, comment, last_updated
		 FROM interfaces WHERE mac_address = ?`, mac,
	).Scan(&i.ID, &i.DeviceID, &i.Name, &i.Type, &i.MACAddress,
		&i.MTU, &i.Running, &i.Disabled, &i.Comment, &i.LastUpdated)
	if err != nil {
		return nil, err
	}
	return i, nil
}

func DeleteStaleInterfaces(db *sql.DB, deviceID string, cutoff time.Time) error {
	_, err := db.Exec(
		`DELETE FROM interfaces WHERE device_id = ? AND last_updated < ?`,
		deviceID, cutoff,
	)
	return err
}
