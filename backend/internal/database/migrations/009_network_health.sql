-- +goose Up

-- bridge_status: one row per bridge interface per device.
-- Snapshot of /interface/bridge + monitor data, refreshed on each poll cycle.
CREATE TABLE bridge_status (
    id                       TEXT PRIMARY KEY,
    device_id                TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    bridge_name              TEXT NOT NULL,
    protocol                 TEXT NOT NULL DEFAULT 'none',  -- none|stp|rstp|mstp
    stp_enabled              INTEGER NOT NULL DEFAULT 0,
    bridge_id                TEXT NOT NULL DEFAULT '',
    root_bridge_id           TEXT NOT NULL DEFAULT '',
    root_path_cost           INTEGER NOT NULL DEFAULT 0,
    root_port                TEXT NOT NULL DEFAULT '',
    topology_changes         INTEGER NOT NULL DEFAULT 0,
    last_topology_change     TEXT NOT NULL DEFAULT '',
    port_count               INTEGER NOT NULL DEFAULT 0,
    last_polled              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(device_id, bridge_name)
);
CREATE INDEX idx_bridge_status_device ON bridge_status(device_id);

-- bridge_port_status: one row per bridge port per device.
CREATE TABLE bridge_port_status (
    id              TEXT PRIMARY KEY,
    device_id       TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    bridge_name     TEXT NOT NULL,
    port_interface  TEXT NOT NULL,
    role            TEXT NOT NULL DEFAULT '',  -- root|designated|alternate|backup|disabled
    status          TEXT NOT NULL DEFAULT '',  -- forwarding|discarding|learning|blocking|disabled
    edge            INTEGER NOT NULL DEFAULT 0,
    point_to_point  INTEGER NOT NULL DEFAULT 0,
    path_cost       INTEGER NOT NULL DEFAULT 0,
    designated_bridge TEXT NOT NULL DEFAULT '',
    last_polled     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(device_id, bridge_name, port_interface)
);
CREATE INDEX idx_bridge_port_device ON bridge_port_status(device_id);
CREATE INDEX idx_bridge_port_bridge ON bridge_port_status(device_id, bridge_name);

-- loop_events: append-only log of loop / flap / STP anomalies.
--
-- event_type values:
--   stp_disabled  — bridge with multiple running ports has STP off
--   tcn_storm     — topology_changes counter rose unusually fast since last poll
--   mac_flap      — bridge log reported a MAC flap
--   loop_detected — bridge log reported "loop detected"
--   bpdu_received — log reported unexpected BPDU on edge port
--
-- severity:
--   warn      — degraded / risky configuration but no active loop
--   critical  — active loop or flap detected
CREATE TABLE loop_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id    TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    event_type   TEXT NOT NULL,
    severity     TEXT NOT NULL DEFAULT 'warn' CHECK (severity IN ('warn', 'critical')),
    bridge_name  TEXT NOT NULL DEFAULT '',
    port_interface TEXT NOT NULL DEFAULT '',
    mac_address  TEXT NOT NULL DEFAULT '',
    message      TEXT NOT NULL DEFAULT '',
    recorded_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_loop_events_time ON loop_events(recorded_at);
CREATE INDEX idx_loop_events_device ON loop_events(device_id, recorded_at);

-- +goose Down

DROP TABLE IF EXISTS loop_events;
DROP TABLE IF EXISTS bridge_port_status;
DROP TABLE IF EXISTS bridge_status;
