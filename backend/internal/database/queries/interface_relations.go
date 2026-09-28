package queries

import (
	"database/sql"
	"strings"
)

// InterfaceRelation is one interface-stacking fact of a device: bond
// membership (Kind "bond": InterfaceName is a member of the bond Parent) or
// the interface a VLAN / PPPoE client runs over (Kind "vlan" / "pppoe").
type InterfaceRelation struct {
	DeviceID      string `json:"device_id"`
	InterfaceName string `json:"interface"`
	Kind          string `json:"kind"`
	Parent        string `json:"parent"`
}

// ReplaceInterfaceRelations swaps one device's rows for a fresh dump in ONE
// transaction. DeviceID on each row is ignored (the deviceID argument wins);
// rows without an interface, parent or kind are skipped, and a duplicate
// (interface, kind) keeps the last one.
func ReplaceInterfaceRelations(db *sql.DB, deviceID string, rels []InterfaceRelation) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM interface_relations WHERE device_id = ?`, deviceID); err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO interface_relations (device_id, interface_name, kind, parent, updated_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rels {
		iface, kind, parent := strings.TrimSpace(r.InterfaceName), strings.TrimSpace(r.Kind), strings.TrimSpace(r.Parent)
		if iface == "" || kind == "" || parent == "" {
			continue
		}
		if _, err := stmt.Exec(deviceID, iface, kind, parent); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListInterfaceRelations returns every device's rows, ordered by device,
// interface and kind.
func ListInterfaceRelations(db *sql.DB) ([]InterfaceRelation, error) {
	rows, err := db.Query(
		`SELECT device_id, interface_name, kind, parent FROM interface_relations
		 ORDER BY device_id, interface_name, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InterfaceRelation
	for rows.Next() {
		var r InterfaceRelation
		if err := rows.Scan(&r.DeviceID, &r.InterfaceName, &r.Kind, &r.Parent); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
