-- +goose Up

CREATE TABLE users (
    id              TEXT PRIMARY KEY,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,
    role            TEXT NOT NULL DEFAULT 'viewer' CHECK (role IN ('admin', 'viewer')),
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE devices (
    id              TEXT PRIMARY KEY,
    address         TEXT NOT NULL UNIQUE,
    identity        TEXT NOT NULL DEFAULT '',
    platform        TEXT NOT NULL DEFAULT '',
    board           TEXT NOT NULL DEFAULT '',
    ros_version     TEXT NOT NULL DEFAULT '',
    firmware_version TEXT NOT NULL DEFAULT '',
    architecture    TEXT NOT NULL DEFAULT '',

    username        TEXT NOT NULL DEFAULT 'admin',
    password_enc    TEXT NOT NULL DEFAULT '',
    use_tls         INTEGER NOT NULL DEFAULT 0,
    api_port        INTEGER NOT NULL DEFAULT 8728,

    status          TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN ('online', 'offline', 'unknown')),
    cpu_load        INTEGER,
    memory_used     INTEGER,
    memory_total    INTEGER,
    uptime          TEXT,
    last_seen       DATETIME,
    last_error      TEXT,

    tags            TEXT DEFAULT '[]',
    notes           TEXT DEFAULT '',
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE interfaces (
    id              TEXT PRIMARY KEY,
    device_id       TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    type            TEXT NOT NULL DEFAULT '',
    mac_address     TEXT NOT NULL DEFAULT '',
    mtu             INTEGER,
    running         INTEGER NOT NULL DEFAULT 0,
    disabled        INTEGER NOT NULL DEFAULT 0,
    comment         TEXT DEFAULT '',
    last_updated    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(device_id, name)
);
CREATE INDEX idx_interfaces_device ON interfaces(device_id);

CREATE TABLE neighbors (
    id                  TEXT PRIMARY KEY,
    device_id           TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    local_interface     TEXT NOT NULL,
    neighbor_address    TEXT DEFAULT '',
    neighbor_mac        TEXT NOT NULL,
    neighbor_identity   TEXT DEFAULT '',
    neighbor_platform   TEXT DEFAULT '',
    neighbor_board      TEXT DEFAULT '',
    neighbor_version    TEXT DEFAULT '',
    neighbor_interface  TEXT DEFAULT '',
    discovered_by       TEXT DEFAULT '',
    last_seen           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(device_id, local_interface, neighbor_mac)
);
CREATE INDEX idx_neighbors_device ON neighbors(device_id);
CREATE INDEX idx_neighbors_mac ON neighbors(neighbor_mac);

CREATE TABLE links (
    id              TEXT PRIMARY KEY,
    device_a_id     TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    interface_a     TEXT NOT NULL,
    device_b_id     TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    interface_b     TEXT NOT NULL,
    link_type       TEXT NOT NULL DEFAULT 'ethernet',
    discovered_by   TEXT DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'up',
    last_seen       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE(device_a_id, interface_a, device_b_id, interface_b)
);
CREATE INDEX idx_links_device_a ON links(device_a_id);
CREATE INDEX idx_links_device_b ON links(device_b_id);

CREATE TABLE traffic_samples (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id           TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    interface_name      TEXT NOT NULL,
    rx_bits_per_sec     INTEGER NOT NULL DEFAULT 0,
    tx_bits_per_sec     INTEGER NOT NULL DEFAULT 0,
    rx_packets_per_sec  INTEGER NOT NULL DEFAULT 0,
    tx_packets_per_sec  INTEGER NOT NULL DEFAULT 0,
    collected_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_traffic_device_time ON traffic_samples(device_id, interface_name, collected_at);

CREATE TABLE firmware_status (
    id                  TEXT PRIMARY KEY,
    device_id           TEXT NOT NULL UNIQUE REFERENCES devices(id) ON DELETE CASCADE,
    channel             TEXT NOT NULL DEFAULT 'stable',
    installed_version   TEXT NOT NULL DEFAULT '',
    latest_version      TEXT DEFAULT '',
    update_available    INTEGER NOT NULL DEFAULT 0,
    routerboard_current TEXT DEFAULT '',
    routerboard_upgrade TEXT DEFAULT '',
    last_checked        DATETIME,
    updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down

DROP TABLE IF EXISTS firmware_status;
DROP TABLE IF EXISTS traffic_samples;
DROP TABLE IF EXISTS links;
DROP TABLE IF EXISTS neighbors;
DROP TABLE IF EXISTS interfaces;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS users;
