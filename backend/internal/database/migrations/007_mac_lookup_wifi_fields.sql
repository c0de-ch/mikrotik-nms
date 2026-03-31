-- +goose Up

ALTER TABLE mac_lookup ADD COLUMN interface_name TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN device_id TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN ap TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN ssid TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN band TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN channel TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN frequency TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN signal TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN tx_rate TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN rx_rate TEXT DEFAULT '';
ALTER TABLE mac_lookup ADD COLUMN uptime TEXT DEFAULT '';

-- +goose Down

-- SQLite doesn't support DROP COLUMN before 3.35.0, so recreate
CREATE TABLE mac_lookup_backup AS SELECT mac_address, ip_address, host_name, dns_name, source, device_name, updated_at FROM mac_lookup;
DROP TABLE mac_lookup;
CREATE TABLE mac_lookup (
    mac_address TEXT PRIMARY KEY,
    ip_address  TEXT DEFAULT '',
    host_name   TEXT DEFAULT '',
    dns_name    TEXT DEFAULT '',
    source      TEXT DEFAULT '',
    device_name TEXT DEFAULT '',
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO mac_lookup SELECT * FROM mac_lookup_backup;
DROP TABLE mac_lookup_backup;
