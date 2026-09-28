-- +goose Up

-- port_stats_1m: per-(device, interface) traffic in 1-minute buckets, written by
-- the port-stats collector (poller/portstats.go) from /interface/print byte
-- counters. bucket = UTC minute start as text 'YYYY-MM-DD HH:MM:00'. Rates are
-- bits/s: *_bps_avg = bytes*8 / seconds covered by samples in that minute,
-- *_bps_max = the highest single poll-interval rate in the minute. Idle minutes
-- (0 bytes both ways) and non-running interfaces are not stored: readers treat
-- a missing row as 0.
CREATE TABLE port_stats_1m (
    device_id      TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    interface_name TEXT NOT NULL,
    bucket         DATETIME NOT NULL,
    rx_bps_avg     INTEGER NOT NULL,
    tx_bps_avg     INTEGER NOT NULL,
    rx_bps_max     INTEGER NOT NULL,
    tx_bps_max     INTEGER NOT NULL,
    rx_bytes       INTEGER NOT NULL,
    tx_bytes       INTEGER NOT NULL,
    rx_pps_avg     INTEGER NOT NULL DEFAULT 0,
    tx_pps_avg     INTEGER NOT NULL DEFAULT 0,
    samples        INTEGER NOT NULL,
    PRIMARY KEY (device_id, interface_name, bucket)
) WITHOUT ROWID;
-- Range scans across all ports (top talkers) and the retention purge.
CREATE INDEX idx_port_stats_1m_bucket ON port_stats_1m(bucket);

-- port_stats_1h: hourly rollup of port_stats_1m (bucket = UTC hour start
-- 'YYYY-MM-DD HH:00:00'). *_bps_avg = bytes*8/3600 (minutes without a row count
-- as idle), *_bps_max = max of the minute maxima, bytes/samples summed.
CREATE TABLE port_stats_1h (
    device_id      TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    interface_name TEXT NOT NULL,
    bucket         DATETIME NOT NULL,
    rx_bps_avg     INTEGER NOT NULL,
    tx_bps_avg     INTEGER NOT NULL,
    rx_bps_max     INTEGER NOT NULL,
    tx_bps_max     INTEGER NOT NULL,
    rx_bytes       INTEGER NOT NULL,
    tx_bytes       INTEGER NOT NULL,
    rx_pps_avg     INTEGER NOT NULL DEFAULT 0,
    tx_pps_avg     INTEGER NOT NULL DEFAULT 0,
    samples        INTEGER NOT NULL,
    PRIMARY KEY (device_id, interface_name, bucket)
) WITHOUT ROWID;
CREATE INDEX idx_port_stats_1h_bucket ON port_stats_1h(bucket);

-- port_hosts: bridge FDB (/interface/bridge/host, local=false) per device port,
-- refreshed every port_hosts_interval. Rows are upserted (last_seen bumped);
-- readers only trust rows seen recently, and retention drops rows older than
-- port_hosts_stale_days. mac_address is uppercase.
CREATE TABLE port_hosts (
    device_id      TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    interface_name TEXT NOT NULL,
    mac_address    TEXT NOT NULL,
    vid            INTEGER NOT NULL DEFAULT 0,
    bridge         TEXT NOT NULL DEFAULT '',
    first_seen     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (device_id, interface_name, mac_address)
) WITHOUT ROWID;
CREATE INDEX idx_port_hosts_mac ON port_hosts(mac_address);
CREATE INDEX idx_port_hosts_last_seen ON port_hosts(last_seen);

-- Runtime-tunable traffic-analytics knobs (see Settings → Traffic analytics).
INSERT INTO app_settings (key, value) VALUES ('port_stats_interval', '15') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('port_hosts_interval', '300') ON CONFLICT(key) DO NOTHING;
-- port_stats_1m_days: the UI reads at most 24 h of 1-minute data (longer ranges
-- use port_stats_1h), so two days is plenty; ~60 MB/day on a ~200-port fleet.
INSERT INTO app_settings (key, value) VALUES ('port_stats_1m_days', '2') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('port_stats_1h_days', '365') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('port_hosts_stale_days', '7') ON CONFLICT(key) DO NOTHING;

-- +goose Down

DROP TABLE IF EXISTS port_hosts;
DROP TABLE IF EXISTS port_stats_1h;
DROP TABLE IF EXISTS port_stats_1m;
