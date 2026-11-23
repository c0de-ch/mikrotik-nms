-- +goose Up
-- The newest RouterOS version of each update channel, read from MikroTik's
-- release feed (upgrade.mikrotik.com/routeros/NEWESTa7.<channel>). Firmware
-- availability is computed from it and the installed version each device
-- reports — devices never run check-for-updates with the poll credential.
-- Strictly additive (N-1 rule); times are INTEGER unix seconds.
CREATE TABLE firmware_releases (
    channel    TEXT PRIMARY KEY,
    version    TEXT NOT NULL,
    build_time INTEGER,           -- NULL when the feed did not say
    fetched_at INTEGER NOT NULL   -- last successful fetch
);

-- +goose Down
DROP TABLE IF EXISTS firmware_releases;
