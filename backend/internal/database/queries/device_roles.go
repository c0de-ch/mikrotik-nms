package queries

import "database/sql"

// DeviceRoleCounts returns, per device, how many wireless interfaces the
// stored interface list holds and how many ports its bridges have — two of
// the facts the device role is derived from (devclass.RoleFacts). Both come
// from tables the pollers already keep; a device without rows is absent.
func DeviceRoleCounts(db *sql.DB) (radios, bridgePorts map[string]int, err error) {
	radios, bridgePorts = map[string]int{}, map[string]int{}
	count := func(query string, into map[string]int) error {
		rows, err := db.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				return err
			}
			into[id] = n
		}
		return rows.Err()
	}
	// The radio types: legacy wireless, wifi / wifiwave2, 60 GHz. A "cap"
	// interface is a controller's view of someone else's radio.
	if err := count(`SELECT device_id, COUNT(*) FROM interfaces
		WHERE lower(type) IN ('wlan', 'wireless') OR lower(type) LIKE '%wifi%' OR lower(type) LIKE '%60g%'
		GROUP BY device_id`, radios); err != nil {
		return nil, nil, err
	}
	if err := count(`SELECT device_id, COUNT(*) FROM bridge_port_status GROUP BY device_id`, bridgePorts); err != nil {
		return nil, nil, err
	}
	return radios, bridgePorts, nil
}
