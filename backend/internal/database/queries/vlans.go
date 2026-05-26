package queries

import (
	"database/sql"
	"time"
)

type BridgeVLAN struct {
	ID              string    `json:"id"`
	DeviceID        string    `json:"device_id"`
	BridgeName      string    `json:"bridge_name"`
	VLANIDs         string    `json:"vlan_ids"`
	Tagged          string    `json:"tagged"`
	Untagged        string    `json:"untagged"`
	CurrentTagged   string    `json:"current_tagged"`
	CurrentUntagged string    `json:"current_untagged"`
	Comment         string    `json:"comment"`
	LastPolled      time.Time `json:"last_polled"`
}

type VLANLabel struct {
	VLANID    int       `json:"vlan_id"`
	Name      string    `json:"name"`
	Purpose   string    `json:"purpose"`
	Color     string    `json:"color"`
	UpdatedAt time.Time `json:"updated_at"`
}

func UpsertBridgeVLAN(db *sql.DB, v *BridgeVLAN) error {
	_, err := db.Exec(
		`INSERT INTO bridge_vlans (id, device_id, bridge_name, vlan_ids, tagged, untagged,
		   current_tagged, current_untagged, comment, last_polled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id, bridge_name, vlan_ids) DO UPDATE SET
		   tagged=excluded.tagged,
		   untagged=excluded.untagged,
		   current_tagged=excluded.current_tagged,
		   current_untagged=excluded.current_untagged,
		   comment=excluded.comment,
		   last_polled=CURRENT_TIMESTAMP`,
		v.ID, v.DeviceID, v.BridgeName, v.VLANIDs, v.Tagged, v.Untagged,
		v.CurrentTagged, v.CurrentUntagged, v.Comment,
	)
	return err
}

func ListBridgeVLANs(db *sql.DB) ([]BridgeVLAN, error) {
	rows, err := db.Query(
		`SELECT id, device_id, bridge_name, vlan_ids, tagged, untagged,
		   current_tagged, current_untagged, comment, last_polled
		 FROM bridge_vlans ORDER BY device_id, vlan_ids`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BridgeVLAN
	for rows.Next() {
		var v BridgeVLAN
		if err := rows.Scan(&v.ID, &v.DeviceID, &v.BridgeName, &v.VLANIDs, &v.Tagged, &v.Untagged,
			&v.CurrentTagged, &v.CurrentUntagged, &v.Comment, &v.LastPolled); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteStaleBridgeVLANs drops bridge_vlans rows whose last_polled is older
// than the cutoff. Used to clear VLAN entries that were removed from a device.
func DeleteStaleBridgeVLANs(db *sql.DB, cutoff time.Time) error {
	_, err := db.Exec(`DELETE FROM bridge_vlans WHERE last_polled < ?`, cutoff)
	return err
}

func ListVLANLabels(db *sql.DB) ([]VLANLabel, error) {
	rows, err := db.Query(
		`SELECT vlan_id, name, purpose, color, updated_at
		 FROM vlan_labels ORDER BY vlan_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VLANLabel
	for rows.Next() {
		var l VLANLabel
		if err := rows.Scan(&l.VLANID, &l.Name, &l.Purpose, &l.Color, &l.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func UpsertVLANLabel(db *sql.DB, l *VLANLabel) error {
	_, err := db.Exec(
		`INSERT INTO vlan_labels (vlan_id, name, purpose, color, updated_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(vlan_id) DO UPDATE SET
		   name=excluded.name,
		   purpose=excluded.purpose,
		   color=excluded.color,
		   updated_at=CURRENT_TIMESTAMP`,
		l.VLANID, l.Name, l.Purpose, l.Color,
	)
	return err
}
