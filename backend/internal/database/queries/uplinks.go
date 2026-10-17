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

// ReplaceDeviceUplinks swaps the device's egress rows (default-route / vpn)
// for the given set. kind='gateway-host' rows are owned by the fleet-wide
// attachment pass (ReplaceGatewayHosts) and are left alone.
func ReplaceDeviceUplinks(db *sql.DB, deviceID string, ups []DeviceUplink) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`DELETE FROM device_uplinks WHERE device_id = ? AND kind IN ('default-route', 'vpn')`, deviceID); err != nil {
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

// DefaultUplinkWindow is how long an uplink row stays current when no
// per-device rule is given (ListFreshDeviceUplinks with a nil UplinkAge):
// the fixed window every reader applied before the per-device cadences.
const DefaultUplinkWindow = 10 * time.Minute

// UplinkAge tells how long dev's device_uplinks rows stay current at now —
// window — and whether its polling is paused, when they do not age at all:
// its routes are not read while the pause lasts, and its last known egress
// must not age off the map and out of the flow classification meanwhile.
// poller.UplinkAge builds it from the device's route-read cadence.
type UplinkAge func(dev Device, now time.Time) (window time.Duration, paused bool)

// ListFreshDeviceUplinks returns the uplink rows current at now, judged per
// device by age: last seen within the device's window, or of any age while
// the device is paused (each route read replaces the device's rows, so those
// are exactly its last read). A nil age applies DefaultUplinkWindow to every
// device, the pause still honoured. Rows of devices that no longer exist are
// dropped. Every reader of the egress facts — the map, the traffic role
// graph, the gateway attachment, the flow plan and points — goes through
// here, so they all agree on which egress is current.
func ListFreshDeviceUplinks(db *sql.DB, age UplinkAge, now time.Time) ([]DeviceUplink, error) {
	devices, err := ListDevices(db)
	if err != nil {
		return nil, err
	}
	type rule struct {
		cutoff time.Time
		paused bool
	}
	rules := make(map[string]rule, len(devices))
	for i := range devices {
		d := &devices[i]
		window, paused := DefaultUplinkWindow, d.PollingPaused(now)
		if age != nil {
			window, paused = age(*d, now)
		}
		rules[d.ID] = rule{cutoff: now.Add(-window), paused: paused}
	}
	all, err := listUplinksSince(db, time.Time{})
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, u := range all {
		r, ok := rules[u.DeviceID]
		if ok && (r.paused || !u.LastSeen.Before(r.cutoff)) {
			out = append(out, u)
		}
	}
	return out, nil
}

// listUplinksSince returns every row last seen at or after since (the zero
// time: all of them).
func listUplinksSince(db *sql.DB, since time.Time) ([]DeviceUplink, error) {
	rows, err := db.Query(
		`SELECT id, device_id, kind, interface, iface_type, gateway_ip, last_seen
		 FROM device_uplinks WHERE last_seen >= ?
		 ORDER BY device_id, kind, interface`, sqlTime(since))
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

// ListDeviceUplinks returns uplinks seen within notOlderThan, and every row of
// a device whose polling is paused. Freshness readers use
// ListFreshDeviceUplinks (the per-device window); this stays for the "every
// row ever seen" queries (a year-long window) that tell a device with stale
// uplink rows from one without any.
func ListDeviceUplinks(db *sql.DB, notOlderThan time.Duration) ([]DeviceUplink, error) {
	now := time.Now()
	rows, err := db.Query(
		`SELECT id, device_id, kind, interface, iface_type, gateway_ip, last_seen
		 FROM device_uplinks
		 WHERE last_seen >= ? OR device_id IN (`+pausedDevicesSQL+`)
		 ORDER BY device_id, kind, interface`,
		sqlTime(now.Add(-notOlderThan)), sqlTime(now))
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

// ReplaceGatewayHosts swaps ALL kind='gateway-host' rows for the given set.
// One row per gateway IP: which device+port physically learned the gateway's
// MAC (from the bridge FDB) — the map anchors the gateway node there.
func ReplaceGatewayHosts(db *sql.DB, rows []DeviceUplink) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM device_uplinks WHERE kind = 'gateway-host'`); err != nil {
		return err
	}
	for _, u := range rows {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO device_uplinks (id, device_id, kind, interface, iface_type, gateway_ip, last_seen)
			 VALUES (?, ?, 'gateway-host', ?, ?, ?, CURRENT_TIMESTAMP)`,
			"gwhost:"+u.GatewayIP, u.DeviceID, u.Interface, u.IfaceType, u.GatewayIP,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MACForIP returns the MAC the client cache last saw for an IP, or "".
func MACForIP(db *sql.DB, ip string) string {
	var mac string
	if err := db.QueryRow(
		`SELECT mac_address FROM mac_lookup WHERE ip_address = ? LIMIT 1`, ip).Scan(&mac); err != nil {
		return ""
	}
	return mac
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
