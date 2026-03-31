-- +goose Up

-- Persistent MAC-to-hostname/IP lookup cache built from client scans
CREATE TABLE mac_lookup (
    mac_address TEXT PRIMARY KEY,
    ip_address  TEXT DEFAULT '',
    host_name   TEXT DEFAULT '',
    dns_name    TEXT DEFAULT '',
    source      TEXT DEFAULT '',
    device_name TEXT DEFAULT '',
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Client discovery history (periodic snapshots every ~15min)
CREATE TABLE client_history (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    mac_address     TEXT NOT NULL,
    ip_address      TEXT DEFAULT '',
    host_name       TEXT DEFAULT '',
    source          TEXT DEFAULT '',
    device_name     TEXT DEFAULT '',
    recorded_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_client_history_mac ON client_history(mac_address, recorded_at);
CREATE INDEX idx_client_history_time ON client_history(recorded_at);

-- +goose Down

DROP TABLE IF EXISTS client_history;
DROP TABLE IF EXISTS mac_lookup;
