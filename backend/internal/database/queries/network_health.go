package queries

import (
	"database/sql"
	"time"
)

type BridgeStatus struct {
	ID                 string    `json:"id"`
	DeviceID           string    `json:"device_id"`
	BridgeName         string    `json:"bridge_name"`
	Protocol           string    `json:"protocol"`
	STPEnabled         bool      `json:"stp_enabled"`
	BridgeID           string    `json:"bridge_id"`
	RootBridgeID       string    `json:"root_bridge_id"`
	RootPathCost       int       `json:"root_path_cost"`
	RootPort           string    `json:"root_port"`
	TopologyChanges    int       `json:"topology_changes"`
	LastTopologyChange string    `json:"last_topology_change"`
	PortCount          int       `json:"port_count"`
	STPSource          string    `json:"stp_source"` // routeros.BridgePath*: how the port STP state was read
	LastPolled         time.Time `json:"last_polled"`
}

type BridgePortStatus struct {
	ID               string `json:"id"`
	DeviceID         string `json:"device_id"`
	BridgeName       string `json:"bridge_name"`
	PortInterface    string `json:"port_interface"`
	Role             string `json:"role"`
	Status           string `json:"status"`
	Edge             bool   `json:"edge"`
	PointToPoint     bool   `json:"point_to_point"`
	PathCost         int    `json:"path_cost"`
	DesignatedBridge string `json:"designated_bridge"` // designated-bridge-id
	// Fields a device may not report: nil (NULL, JSON null) means unknown,
	// which detectors must not read as 0 or false.
	TopologyChanges *int      `json:"topology_changes"`
	Forwarding      *bool     `json:"forwarding"`
	Learning        *bool     `json:"learning"`
	Inactive        *bool     `json:"inactive"`
	Disabled        *bool     `json:"disabled"`
	HWOffload       *bool     `json:"hw_offload"`
	HWOffloadGroup  string    `json:"hw_offload_group"`
	PVID            *int      `json:"pvid"`
	PortState       string    `json:"port_state"`  // raw RouterOS status: in-bridge | inactive
	EdgeConfig      string    `json:"edge_config"` // configured edge: auto | yes | no | yes-discover | no-discover
	LastPolled      time.Time `json:"last_polled"`
}

type LoopEvent struct {
	ID             int64      `json:"id"`
	DeviceID       string     `json:"device_id"`
	EventType      string     `json:"event_type"`
	Severity       string     `json:"severity"`
	BridgeName     string     `json:"bridge_name"`
	PortInterface  string     `json:"port_interface"`
	MACAddress     string     `json:"mac_address"`
	Message        string     `json:"message"`
	RecordedAt     time.Time  `json:"recorded_at"`
	Acknowledged   bool       `json:"acknowledged"`
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
}

func UpsertBridgeStatus(db *sql.DB, b *BridgeStatus) error {
	stp := 0
	if b.STPEnabled {
		stp = 1
	}
	_, err := db.Exec(
		`INSERT INTO bridge_status (id, device_id, bridge_name, protocol, stp_enabled, bridge_id,
		   root_bridge_id, root_path_cost, root_port, topology_changes, last_topology_change,
		   port_count, stp_source, last_polled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id, bridge_name) DO UPDATE SET
		   protocol=excluded.protocol,
		   stp_enabled=excluded.stp_enabled,
		   bridge_id=excluded.bridge_id,
		   root_bridge_id=excluded.root_bridge_id,
		   root_path_cost=excluded.root_path_cost,
		   root_port=excluded.root_port,
		   topology_changes=excluded.topology_changes,
		   last_topology_change=excluded.last_topology_change,
		   port_count=excluded.port_count,
		   stp_source=excluded.stp_source,
		   last_polled=CURRENT_TIMESTAMP`,
		b.ID, b.DeviceID, b.BridgeName, b.Protocol, stp, b.BridgeID,
		b.RootBridgeID, b.RootPathCost, b.RootPort, b.TopologyChanges, b.LastTopologyChange,
		b.PortCount, b.STPSource,
	)
	return err
}

func UpsertBridgePortStatus(db *sql.DB, p *BridgePortStatus) error {
	edge := 0
	if p.Edge {
		edge = 1
	}
	p2p := 0
	if p.PointToPoint {
		p2p = 1
	}
	_, err := db.Exec(
		`INSERT INTO bridge_port_status (id, device_id, bridge_name, port_interface, role, status,
		   edge, point_to_point, path_cost, designated_bridge,
		   topology_changes, forwarding, learning, inactive, disabled, hw_offload, hw_offload_group,
		   pvid, port_state, edge_config, last_polled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(device_id, bridge_name, port_interface) DO UPDATE SET
		   role=excluded.role,
		   status=excluded.status,
		   edge=excluded.edge,
		   point_to_point=excluded.point_to_point,
		   path_cost=excluded.path_cost,
		   designated_bridge=excluded.designated_bridge,
		   topology_changes=excluded.topology_changes,
		   forwarding=excluded.forwarding,
		   learning=excluded.learning,
		   inactive=excluded.inactive,
		   disabled=excluded.disabled,
		   hw_offload=excluded.hw_offload,
		   hw_offload_group=excluded.hw_offload_group,
		   pvid=excluded.pvid,
		   port_state=excluded.port_state,
		   edge_config=excluded.edge_config,
		   last_polled=CURRENT_TIMESTAMP`,
		p.ID, p.DeviceID, p.BridgeName, p.PortInterface, p.Role, p.Status,
		edge, p2p, p.PathCost, p.DesignatedBridge,
		nullInt(p.TopologyChanges), nullBool(p.Forwarding), nullBool(p.Learning), nullBool(p.Inactive),
		nullBool(p.Disabled), nullBool(p.HWOffload), p.HWOffloadGroup,
		nullInt(p.PVID), p.PortState, p.EdgeConfig,
	)
	return err
}

func ListBridgeStatus(db *sql.DB) ([]BridgeStatus, error) {
	rows, err := db.Query(
		`SELECT id, device_id, bridge_name, protocol, stp_enabled, bridge_id,
		   root_bridge_id, root_path_cost, root_port, topology_changes, last_topology_change,
		   port_count, stp_source, last_polled
		 FROM bridge_status ORDER BY device_id, bridge_name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BridgeStatus
	for rows.Next() {
		var b BridgeStatus
		var stp int
		if err := rows.Scan(&b.ID, &b.DeviceID, &b.BridgeName, &b.Protocol, &stp, &b.BridgeID,
			&b.RootBridgeID, &b.RootPathCost, &b.RootPort, &b.TopologyChanges, &b.LastTopologyChange,
			&b.PortCount, &b.STPSource, &b.LastPolled); err != nil {
			return nil, err
		}
		b.STPEnabled = stp == 1
		out = append(out, b)
	}
	return out, rows.Err()
}

func ListBridgePortStatus(db *sql.DB) ([]BridgePortStatus, error) {
	rows, err := db.Query(
		`SELECT id, device_id, bridge_name, port_interface, role, status,
		   edge, point_to_point, path_cost, designated_bridge,
		   topology_changes, forwarding, learning, inactive, disabled, hw_offload,
		   COALESCE(hw_offload_group, ''), pvid, COALESCE(port_state, ''), COALESCE(edge_config, ''),
		   last_polled
		 FROM bridge_port_status ORDER BY device_id, bridge_name, port_interface`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []BridgePortStatus
	for rows.Next() {
		var p BridgePortStatus
		var edge, p2p int
		var tcn, pvid sql.NullInt64
		var fwd, lrn, inactive, disabled, hw sql.NullInt64
		if err := rows.Scan(&p.ID, &p.DeviceID, &p.BridgeName, &p.PortInterface, &p.Role, &p.Status,
			&edge, &p2p, &p.PathCost, &p.DesignatedBridge,
			&tcn, &fwd, &lrn, &inactive, &disabled, &hw,
			&p.HWOffloadGroup, &pvid, &p.PortState, &p.EdgeConfig,
			&p.LastPolled); err != nil {
			return nil, err
		}
		p.Edge = edge == 1
		p.PointToPoint = p2p == 1
		p.TopologyChanges = intPtr(tcn)
		p.PVID = intPtr(pvid)
		p.Forwarding = boolPtr(fwd)
		p.Learning = boolPtr(lrn)
		p.Inactive = boolPtr(inactive)
		p.Disabled = boolPtr(disabled)
		p.HWOffload = boolPtr(hw)
		out = append(out, p)
	}
	return out, rows.Err()
}

// nullInt / nullBool bind an optional field as NULL when it is unknown;
// intPtr / boolPtr read it back the same way.
func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullBool(v *bool) any {
	if v == nil {
		return nil
	}
	if *v {
		return 1
	}
	return 0
}

func intPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func boolPtr(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 == 1
	return &b
}

// DeleteStaleBridgeRows drops one device's bridge_status / bridge_port_status
// rows whose last_polled is older than the cutoff. Used to clear bridges and
// ports that were removed from the device.
func DeleteStaleBridgeRows(db *sql.DB, deviceID string, cutoff time.Time) error {
	if _, err := db.Exec(`DELETE FROM bridge_port_status WHERE device_id = ? AND last_polled < ?`,
		deviceID, sqlTime(cutoff)); err != nil {
		return err
	}
	_, err := db.Exec(`DELETE FROM bridge_status WHERE device_id = ? AND last_polled < ?`, deviceID, sqlTime(cutoff))
	return err
}

func InsertLoopEvent(db *sql.DB, e *LoopEvent) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO loop_events (device_id, event_type, severity, bridge_name, port_interface, mac_address, message)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.DeviceID, e.EventType, e.Severity, e.BridgeName, e.PortInterface, e.MACAddress, e.Message,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func ListLoopEvents(db *sql.DB, limit int) ([]LoopEvent, error) {
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	rows, err := db.Query(
		`SELECT id, device_id, event_type, severity, bridge_name, port_interface, mac_address, message, recorded_at, acknowledged, acknowledged_at
		 FROM loop_events ORDER BY recorded_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LoopEvent
	for rows.Next() {
		var e LoopEvent
		var ack int
		if err := rows.Scan(&e.ID, &e.DeviceID, &e.EventType, &e.Severity, &e.BridgeName,
			&e.PortInterface, &e.MACAddress, &e.Message, &e.RecordedAt, &ack, &e.AcknowledgedAt); err != nil {
			return nil, err
		}
		e.Acknowledged = ack == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// AckLoopEvent marks a single loop event as acknowledged.
func AckLoopEvent(db *sql.DB, id int64) error {
	_, err := db.Exec(
		`UPDATE loop_events SET acknowledged = 1, acknowledged_at = CURRENT_TIMESTAMP WHERE id = ?`,
		id,
	)
	return err
}

// AckAllLoopEvents acknowledges every currently-unacknowledged loop event and
// returns the number of rows affected.
func AckAllLoopEvents(db *sql.DB) (int64, error) {
	res, err := db.Exec(
		`UPDATE loop_events SET acknowledged = 1, acknowledged_at = CURRENT_TIMESTAMP WHERE acknowledged = 0`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func DeleteOldLoopEvents(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM loop_events WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountRecentLoopEvents returns the count of loop_events grouped by severity
// since the cutoff. Used for the network health summary.
func CountRecentLoopEvents(db *sql.DB, cutoff time.Time) (warn, critical int, err error) {
	rows, err := db.Query(
		`SELECT severity, COUNT(*) FROM loop_events WHERE recorded_at >= ? AND acknowledged = 0 GROUP BY severity`,
		cutoff,
	)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var c int
		if err := rows.Scan(&s, &c); err != nil {
			return 0, 0, err
		}
		switch s {
		case "warn":
			warn = c
		case "critical":
			critical = c
		}
	}
	return warn, critical, rows.Err()
}
