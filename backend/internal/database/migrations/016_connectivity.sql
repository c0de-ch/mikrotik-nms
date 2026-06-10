-- +goose Up

-- ping_targets: things the NMS actively probes with ICMP from RouterOS devices
-- (/ping run over the API).
--
-- kind values:
--   internet — a fixed IP/hostname probed from a specific device (device_id required)
--   client   — a watched LAN client identified by MAC; its current IP is resolved
--               from mac_lookup each cycle and cached in address
--
-- device_id deliberately has NO foreign key: '' means "auto-pick an online
-- device" (client kind), and a target must survive deletion of the device it
-- pointed at (it falls back to auto / shows as unresolvable instead of
-- disappearing).
CREATE TABLE ping_targets (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL DEFAULT 'internet',  -- internet|client
    address     TEXT NOT NULL DEFAULT '',          -- fixed for internet; last-resolved IP cache for client
    mac_address TEXT NOT NULL DEFAULT '',          -- client kind only, stored uppercase
    label       TEXT NOT NULL DEFAULT '',
    device_id   TEXT NOT NULL DEFAULT '',          -- probing device; '' = auto (client kind)
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- ping_samples: one row per probe run (a /ping burst of N packets).
-- error != '' means the probe could not run at all (device offline, no API
-- connection, no known client IP, RouterOS trap); such rows have sent = 0.
-- device_id / address record what was actually probed from where, since a
-- client target's resolved IP and probing device can change over time.
CREATE TABLE ping_samples (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id   TEXT NOT NULL REFERENCES ping_targets(id) ON DELETE CASCADE,
    device_id   TEXT NOT NULL DEFAULT '',
    address     TEXT NOT NULL DEFAULT '',
    sent        INTEGER NOT NULL DEFAULT 0,
    received    INTEGER NOT NULL DEFAULT 0,
    loss_pct    REAL NOT NULL DEFAULT 0,
    rtt_min_ms  REAL,                               -- NULL when no replies came back
    rtt_avg_ms  REAL,
    rtt_max_ms  REAL,
    jitter_ms   REAL,                               -- mean abs diff of consecutive RTTs; NULL with <2 replies
    error       TEXT NOT NULL DEFAULT '',
    recorded_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_ping_samples_target_time ON ping_samples(target_id, recorded_at);
-- Time-only index so the hourly retention purge (DELETE ... WHERE recorded_at < ?)
-- uses an index range instead of a full table scan (migration 014 precedent).
CREATE INDEX idx_ping_samples_time ON ping_samples(recorded_at);

-- client_signal_samples: wifi signal time series for watched clients, captured
-- alongside the ping cycle from CAPsMAN/wifi registration tables. Lets the
-- per-client timeline correlate packet loss with RF conditions.
CREATE TABLE client_signal_samples (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    mac_address TEXT NOT NULL,                      -- stored uppercase
    ap_name     TEXT NOT NULL DEFAULT '',
    ssid        TEXT NOT NULL DEFAULT '',
    band        TEXT NOT NULL DEFAULT '',
    signal_dbm  INTEGER,                            -- NULL when the table reported no parseable signal
    tx_rate     TEXT NOT NULL DEFAULT '',
    rx_rate     TEXT NOT NULL DEFAULT '',
    recorded_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_client_signal_mac_time ON client_signal_samples(mac_address, recorded_at);
CREATE INDEX idx_client_signal_time ON client_signal_samples(recorded_at);

-- Runtime-tunable connectivity poller knobs (re-read each cycle).
INSERT INTO app_settings (key, value) VALUES ('connectivity_interval', '30') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('connectivity_ping_count', '5') ON CONFLICT(key) DO NOTHING;

-- +goose Down

DROP TABLE IF EXISTS client_signal_samples;
DROP TABLE IF EXISTS ping_samples;
DROP TABLE IF EXISTS ping_targets;
