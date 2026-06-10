package queries

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

// PingTarget is something the connectivity poller probes with ICMP from a
// RouterOS device.
//
// kind "internet": Address is a fixed IP/hostname, DeviceID names the RouterOS
// device that runs /ping.
// kind "client": MACAddress (uppercase) identifies a watched LAN client; the
// poller resolves its current IP from mac_lookup each cycle and caches it in
// Address. DeviceID is optional ("" = auto-pick).
//
// SrcAddress / SrcInterface are optional probe-source selectors passed to
// /ping (and /tool/traceroute) as =src-address= / =interface= when non-empty,
// so the probe leaves FROM a specific VLAN interface / source IP (multi-ISP,
// policy-routed networks).
type PingTarget struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Address      string    `json:"address"`
	MACAddress   string    `json:"mac_address"`
	Label        string    `json:"label"`
	DeviceID     string    `json:"device_id"`
	SrcAddress   string    `json:"src_address"`
	SrcInterface string    `json:"src_interface"`
	Enabled      bool      `json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
}

// PingSample is the outcome of one /ping burst. Error != "" means the probe
// could not run (device offline / no API connection / no known IP / trap);
// such samples have Sent = 0 and nil RTTs.
type PingSample struct {
	ID         int64     `json:"id"`
	TargetID   string    `json:"target_id"`
	DeviceID   string    `json:"device_id"`
	Address    string    `json:"address"`
	Sent       int       `json:"sent"`
	Received   int       `json:"received"`
	LossPct    float64   `json:"loss_pct"`
	RTTMinMs   *float64  `json:"rtt_min_ms"`
	RTTAvgMs   *float64  `json:"rtt_avg_ms"`
	RTTMaxMs   *float64  `json:"rtt_max_ms"`
	JitterMs   *float64  `json:"jitter_ms"`
	Error      string    `json:"error"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ClientSignalSample is a wifi signal reading for a watched client, captured
// from CAPsMAN/wifi registration tables alongside the ping cycle.
type ClientSignalSample struct {
	ID         int64     `json:"id"`
	MACAddress string    `json:"mac_address"`
	APName     string    `json:"ap_name"`
	SSID       string    `json:"ssid"`
	Band       string    `json:"band"`
	SignalDBm  *int      `json:"signal_dbm"`
	TxRate     string    `json:"tx_rate"`
	RxRate     string    `json:"rx_rate"`
	RecordedAt time.Time `json:"recorded_at"`
}

func CreatePingTarget(db *sql.DB, t *PingTarget) error {
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	_, err := db.Exec(
		`INSERT INTO ping_targets (id, kind, address, mac_address, label, device_id, src_address, src_interface, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Kind, t.Address, t.MACAddress, t.Label, t.DeviceID, t.SrcAddress, t.SrcInterface, enabled,
	)
	return err
}

const pingTargetCols = `id, kind, address, mac_address, label, device_id, src_address, src_interface, enabled, created_at`

func scanPingTarget(scanner interface{ Scan(...interface{}) error }) (*PingTarget, error) {
	t := &PingTarget{}
	var enabled int
	err := scanner.Scan(&t.ID, &t.Kind, &t.Address, &t.MACAddress, &t.Label, &t.DeviceID,
		&t.SrcAddress, &t.SrcInterface, &enabled, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.Enabled = enabled == 1
	return t, nil
}

func scanPingTargets(rows *sql.Rows) ([]PingTarget, error) {
	var out []PingTarget
	for rows.Next() {
		t, err := scanPingTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func ListPingTargets(db *sql.DB) ([]PingTarget, error) {
	rows, err := db.Query(`SELECT ` + pingTargetCols + ` FROM ping_targets ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPingTargets(rows)
}

func ListEnabledPingTargets(db *sql.DB) ([]PingTarget, error) {
	rows, err := db.Query(`SELECT ` + pingTargetCols + ` FROM ping_targets WHERE enabled = 1 ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPingTargets(rows)
}

// GetPingTargetsByMAC returns the client targets watching a given MAC
// (uppercase). Used by the per-client timeline endpoint.
func GetPingTargetsByMAC(db *sql.DB, mac string) ([]PingTarget, error) {
	rows, err := db.Query(`SELECT `+pingTargetCols+` FROM ping_targets WHERE mac_address = ? ORDER BY created_at`, mac)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPingTargets(rows)
}

func GetPingTarget(db *sql.DB, id string) (*PingTarget, error) {
	row := db.QueryRow(`SELECT `+pingTargetCols+` FROM ping_targets WHERE id = ?`, id)
	return scanPingTarget(row)
}

func UpdatePingTarget(db *sql.DB, t *PingTarget) error {
	enabled := 0
	if t.Enabled {
		enabled = 1
	}
	_, err := db.Exec(
		`UPDATE ping_targets SET kind=?, address=?, mac_address=?, label=?, device_id=?,
		        src_address=?, src_interface=?, enabled=? WHERE id=?`,
		t.Kind, t.Address, t.MACAddress, t.Label, t.DeviceID, t.SrcAddress, t.SrcInterface, enabled, t.ID,
	)
	return err
}

// UpdatePingTargetAddress caches the latest resolved IP for a client target
// without touching anything else.
func UpdatePingTargetAddress(db *sql.DB, id, address string) error {
	_, err := db.Exec(`UPDATE ping_targets SET address=? WHERE id=?`, address, id)
	return err
}

func DeletePingTarget(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM ping_targets WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// InsertPingSample persists a sample, filling in RecordedAt (now, if unset) and
// ID so the same struct can be broadcast over WebSocket afterwards.
func InsertPingSample(db *sql.DB, s *PingSample) error {
	if s.RecordedAt.IsZero() {
		s.RecordedAt = time.Now().UTC()
	}
	res, err := db.Exec(
		`INSERT INTO ping_samples (target_id, device_id, address, sent, received, loss_pct,
		    rtt_min_ms, rtt_avg_ms, rtt_max_ms, jitter_ms, error, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.TargetID, s.DeviceID, s.Address, s.Sent, s.Received, s.LossPct,
		s.RTTMinMs, s.RTTAvgMs, s.RTTMaxMs, s.JitterMs, s.Error, s.RecordedAt,
	)
	if err != nil {
		return err
	}
	s.ID, _ = res.LastInsertId()
	return nil
}

const pingSampleCols = `id, target_id, device_id, address, sent, received, loss_pct,
	rtt_min_ms, rtt_avg_ms, rtt_max_ms, jitter_ms, error, recorded_at`

func scanPingSample(scanner interface{ Scan(...interface{}) error }) (*PingSample, error) {
	s := &PingSample{}
	err := scanner.Scan(&s.ID, &s.TargetID, &s.DeviceID, &s.Address, &s.Sent, &s.Received, &s.LossPct,
		&s.RTTMinMs, &s.RTTAvgMs, &s.RTTMaxMs, &s.JitterMs, &s.Error, &s.RecordedAt)
	return s, err
}

func GetPingSamples(db *sql.DB, targetID string, from, to time.Time, limit int) ([]PingSample, error) {
	rows, err := db.Query(
		`SELECT `+pingSampleCols+` FROM ping_samples
		 WHERE target_id = ? AND recorded_at BETWEEN ? AND ?
		 ORDER BY recorded_at DESC LIMIT ?`,
		targetID, from, to, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PingSample
	for rows.Next() {
		s, err := scanPingSample(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// GetLatestPingSamples returns the newest sample per target. One indexed
// reverse seek per target (the target list is tiny) instead of MAX(id) GROUP
// BY over ping_samples, which would walk the whole index on every target-list
// request and degrade linearly with retention.
func GetLatestPingSamples(db *sql.DB) (map[string]*PingSample, error) {
	rows, err := db.Query(`SELECT id FROM ping_targets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var targetIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		targetIDs = append(targetIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]*PingSample)
	for _, tid := range targetIDs {
		row := db.QueryRow(
			`SELECT `+pingSampleCols+` FROM ping_samples
			 WHERE target_id = ? ORDER BY recorded_at DESC, id DESC LIMIT 1`, tid,
		)
		s, err := scanPingSample(row)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[s.TargetID] = s
	}
	return out, nil
}

func InsertClientSignalSample(db *sql.DB, s *ClientSignalSample) error {
	if s.RecordedAt.IsZero() {
		s.RecordedAt = time.Now().UTC()
	}
	res, err := db.Exec(
		`INSERT INTO client_signal_samples (mac_address, ap_name, ssid, band, signal_dbm, tx_rate, rx_rate, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.MACAddress, s.APName, s.SSID, s.Band, s.SignalDBm, s.TxRate, s.RxRate, s.RecordedAt,
	)
	if err != nil {
		return err
	}
	s.ID, _ = res.LastInsertId()
	return nil
}

func GetClientSignalSamples(db *sql.DB, mac string, from, to time.Time, limit int) ([]ClientSignalSample, error) {
	rows, err := db.Query(
		`SELECT id, mac_address, ap_name, ssid, band, signal_dbm, tx_rate, rx_rate, recorded_at
		 FROM client_signal_samples
		 WHERE mac_address = ? AND recorded_at BETWEEN ? AND ?
		 ORDER BY recorded_at DESC LIMIT ?`,
		mac, from, to, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ClientSignalSample
	for rows.Next() {
		var s ClientSignalSample
		if err := rows.Scan(&s.ID, &s.MACAddress, &s.APName, &s.SSID, &s.Band, &s.SignalDBm,
			&s.TxRate, &s.RxRate, &s.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TracerouteRun is one stored /tool/traceroute pass against an internet ping
// target (manual run-now, or auto-captured when probe loss crossed
// traceroute_loss_threshold). Hops marshals to/from the traceroute_runs.hops
// JSON TEXT column in this layer; the hop struct lives in the routeros package
// (where it is parsed) and carries the wire-contract json tags — queries →
// routeros is a one-way import with no cycle (routeros depends only on stdlib
// and the go-routeros client).
type TracerouteRun struct {
	ID         int64                    `json:"id"`
	TargetID   string                   `json:"target_id"`
	Address    string                   `json:"address"`
	Hops       []routeros.TracerouteHop `json:"hops"`
	Error      string                   `json:"error"`
	RecordedAt time.Time                `json:"recorded_at"`
}

// InsertTracerouteRun persists a run, filling in RecordedAt (now, if unset) and
// ID so the same struct can be broadcast over WebSocket afterwards.
func InsertTracerouteRun(db *sql.DB, r *TracerouteRun) error {
	if r.RecordedAt.IsZero() {
		r.RecordedAt = time.Now().UTC()
	}
	if r.Hops == nil {
		r.Hops = []routeros.TracerouteHop{}
	}
	hops, err := json.Marshal(r.Hops)
	if err != nil {
		return err
	}
	res, err := db.Exec(
		`INSERT INTO traceroute_runs (target_id, address, hops, error, recorded_at)
		 VALUES (?, ?, ?, ?, ?)`,
		r.TargetID, r.Address, string(hops), r.Error, r.RecordedAt,
	)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

// GetTracerouteRuns returns a target's stored runs, newest first, with hops
// parsed from the JSON column (never nil).
func GetTracerouteRuns(db *sql.DB, targetID string, limit int) ([]TracerouteRun, error) {
	rows, err := db.Query(
		`SELECT id, target_id, address, hops, error, recorded_at FROM traceroute_runs
		 WHERE target_id = ? ORDER BY recorded_at DESC, id DESC LIMIT ?`,
		targetID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TracerouteRun
	for rows.Next() {
		var r TracerouteRun
		var hops string
		if err := rows.Scan(&r.ID, &r.TargetID, &r.Address, &hops, &r.Error, &r.RecordedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(hops), &r.Hops); err != nil || r.Hops == nil {
			r.Hops = []routeros.TracerouteHop{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func DeleteOldTracerouteRuns(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM traceroute_runs WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func DeleteOldPingSamples(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM ping_samples WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func DeleteOldClientSignalSamples(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM client_signal_samples WHERE recorded_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetWifiHistoryByMACRange is the time-ranged variant of GetWifiHistoryByMAC,
// for the per-client timeline endpoint.
func GetWifiHistoryByMACRange(db *sql.DB, mac string, from, to time.Time, limit int) ([]WifiHistoryEntry, error) {
	rows, err := db.Query(
		`SELECT id, mac_address, ip_address, host_name, ap_name, ssid, band, channel, signal, tx_rate, rx_rate, event, controller_id, source, reason, recorded_at
		 FROM wifi_history WHERE mac_address=? AND recorded_at BETWEEN ? AND ?
		 ORDER BY recorded_at DESC LIMIT ?`, mac, from, to, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWifiHistory(rows)
}

// GetLoopEventsByDeviceRange returns a device's network-health events within a
// time range, newest first. For the per-client timeline endpoint.
func GetLoopEventsByDeviceRange(db *sql.DB, deviceID string, from, to time.Time, limit int) ([]LoopEvent, error) {
	rows, err := db.Query(
		`SELECT id, device_id, event_type, severity, bridge_name, port_interface, mac_address, message, recorded_at, acknowledged, acknowledged_at
		 FROM loop_events WHERE device_id=? AND recorded_at BETWEEN ? AND ?
		 ORDER BY recorded_at DESC LIMIT ?`, deviceID, from, to, limit,
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

// ResolveClientProbe resolves a client-kind target into (probing device,
// address to ping, client hostname). Shared by the connectivity poller and the
// run-now endpoint so both pick the probe source identically:
//   - target.DeviceID if set and online,
//   - else mac_lookup.device_id if online,
//   - else any online device.
//
// errReason is non-empty when the client has no known IP or no online device is
// available; the other return values are best-effort in that case (address may
// still be set so the failed sample records what would have been probed).
func ResolveClientProbe(db *sql.DB, t *PingTarget) (deviceID, address, hostName, errReason string) {
	lookup, err := GetMACLookup(db, t.MACAddress)
	if err != nil {
		return "", "", "", "client not seen in ARP/DHCP (no mac_lookup entry)"
	}

	hostName = lookup.HostName
	if hostName == "" {
		hostName = lookup.DNSName
	}
	address = lookup.IPAddress
	if address == "" {
		return "", "", hostName, "no known IP address for client"
	}

	// Device statuses, to gate candidates on "online".
	status := make(map[string]string)
	var firstOnline string
	rows, err := db.Query(`SELECT id, status FROM devices ORDER BY identity, address`)
	if err != nil {
		return "", address, hostName, "failed to list devices: " + err.Error()
	}
	defer rows.Close()
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			return "", address, hostName, "failed to list devices: " + err.Error()
		}
		status[id] = st
		if firstOnline == "" && st == "online" {
			firstOnline = id
		}
	}

	if t.DeviceID != "" && status[t.DeviceID] == "online" {
		return t.DeviceID, address, hostName, ""
	}
	if lookup.DeviceID != "" && status[lookup.DeviceID] == "online" {
		return lookup.DeviceID, address, hostName, ""
	}
	if firstOnline != "" {
		return firstOnline, address, hostName, ""
	}
	return "", address, hostName, "no online device available to probe from"
}
