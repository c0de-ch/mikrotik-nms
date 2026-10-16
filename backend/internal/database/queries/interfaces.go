package queries

import (
	"database/sql"
	"strings"
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

// DeleteInterfacesNotIn removes the device's interface rows whose name is not
// in names — interfaces removed or renamed on the device since, and blank
// legacy rows — after a full /interface/print. An empty names is a no-op, so
// an empty reply never wipes a device.
func DeleteInterfacesNotIn(db *sql.DB, deviceID string, names []string) (int64, error) {
	return deleteInterfacesNotIn(db, deviceID, names)
}

// execer is the part of *sql.DB and *sql.Tx the interface writers need.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func deleteInterfacesNotIn(db execer, deviceID string, names []string) (int64, error) {
	if len(names) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(names)+1)
	args = append(args, deviceID)
	for _, n := range names {
		args = append(args, n)
	}
	res, err := db.Exec(`DELETE FROM interfaces WHERE device_id = ? AND name NOT IN (?`+
		strings.Repeat(`, ?`, len(names)-1)+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SyncInterfaces applies one device's interface inventory in ONE
// transaction: every row in upserts is written like UpsertInterface (its
// DeviceID is forced to deviceID), then, when keep is non-empty, the device's
// rows whose name is not in keep are deleted (DeleteInterfacesNotIn's rule,
// including its empty-list guard). It returns how many rows were pruned.
func SyncInterfaces(db *sql.DB, deviceID string, upserts []Interface, keep []string) (int64, error) {
	if len(upserts) == 0 && len(keep) == 0 {
		return 0, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for i := range upserts {
		r := upserts[i]
		if _, err := tx.Exec(
			`INSERT INTO interfaces (id, device_id, name, type, mac_address, mtu, running, disabled, comment, last_updated)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(device_id, name) DO UPDATE SET
			    type=excluded.type, mac_address=excluded.mac_address, mtu=excluded.mtu,
			    running=excluded.running, disabled=excluded.disabled, comment=excluded.comment,
			    last_updated=CURRENT_TIMESTAMP`,
			r.ID, deviceID, r.Name, r.Type, r.MACAddress, r.MTU, r.Running, r.Disabled, r.Comment,
		); err != nil {
			return 0, err
		}
	}
	pruned, err := deleteInterfacesNotIn(tx, deviceID, keep)
	if err != nil {
		return 0, err
	}
	return pruned, tx.Commit()
}

func DeleteStaleInterfaces(db *sql.DB, deviceID string, cutoff time.Time) error {
	_, err := db.Exec(
		`DELETE FROM interfaces WHERE device_id = ? AND last_updated < ?`,
		deviceID, cutoff,
	)
	return err
}
