package queries

import (
	"database/sql"
	"time"
)

// DeviceUplink is one discovered egress path of a device: an active IPv4
// default route (kind "default-route") or a running VPN tunnel interface
// (kind "vpn").
type DeviceUplink struct {
	ID        string    `json:"id"`
	DeviceID  string    `json:"device_id"`
	Kind      string    `json:"kind"`
	Interface string    `json:"interface"`
	IfaceType string    `json:"iface_type"`
	GatewayIP string    `json:"gateway_ip"`
	LastSeen  time.Time `json:"last_seen"`
}

// ReplaceDeviceUplinks swaps the device's uplink rows for the given set.
func ReplaceDeviceUplinks(db *sql.DB, deviceID string, ups []DeviceUplink) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM device_uplinks WHERE device_id = ?`, deviceID); err != nil {
		return err
	}
	for _, u := range ups {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO device_uplinks (id, device_id, kind, interface, iface_type, gateway_ip, last_seen)
			 VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
			u.DeviceID+":"+u.Kind+":"+u.Interface+":"+u.GatewayIP, u.DeviceID, u.Kind, u.Interface, u.IfaceType, u.GatewayIP,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListDeviceUplinks returns uplinks seen within the freshness window.
func ListDeviceUplinks(db *sql.DB, notOlderThan time.Duration) ([]DeviceUplink, error) {
	cutoff := time.Now().Add(-notOlderThan).UTC().Format("2006-01-02 15:04:05")
	rows, err := db.Query(
		`SELECT id, device_id, kind, interface, iface_type, gateway_ip, last_seen
		 FROM device_uplinks WHERE last_seen >= ? ORDER BY device_id, kind, interface`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeviceUplink
	for rows.Next() {
		var u DeviceUplink
		if err := rows.Scan(&u.ID, &u.DeviceID, &u.Kind, &u.Interface, &u.IfaceType, &u.GatewayIP, &u.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// HostnameForIP returns a display name for an IP from the client cache
// (DHCP/DNS discovery), or "" when unknown. Used to label gateway nodes.
func HostnameForIP(db *sql.DB, ip string) string {
	var host, dns sql.NullString
	err := db.QueryRow(
		`SELECT host_name, dns_name FROM mac_lookup WHERE ip_address = ? AND (host_name != '' OR dns_name != '') LIMIT 1`,
		ip).Scan(&host, &dns)
	if err != nil {
		return ""
	}
	if host.Valid && host.String != "" {
		return host.String
	}
	if dns.Valid {
		return dns.String
	}
	return ""
}
