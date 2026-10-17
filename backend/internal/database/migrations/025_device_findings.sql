-- +goose Up

-- device_findings: things the pollers notice about a device that are neither
-- L2 events (loop_events) nor telemetry — a second API poller logging in,
-- login failures. One row per (device, kind, detail); repeats bump count and
-- last_seen instead of adding rows. Additive (M0 schema rule).
CREATE TABLE device_findings (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id  TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,                                 -- parallel_api_poller | login_failure | …
    severity   TEXT NOT NULL DEFAULT 'info' CHECK (severity IN ('info', 'warn', 'critical')),
    detail     TEXT NOT NULL DEFAULT '',
    first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    count      INTEGER NOT NULL DEFAULT 1,
    UNIQUE (device_id, kind, detail)
);
CREATE INDEX idx_device_findings_device ON device_findings(device_id, last_seen);

-- +goose Down
DROP INDEX IF EXISTS idx_device_findings_device;
DROP TABLE IF EXISTS device_findings;
