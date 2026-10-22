-- +goose Up
-- Device CPU classes, per-device polling profile / pause, and the audit log.
--
-- Strictly additive (N-1 rule): the previous release's binary keeps running
-- on this schema. Classes are derived by internal/devclass from the probed
-- /system/resource facts; the columns below only cache them. The existing
-- `architecture` and `memory_total` columns already hold two of the facts.
ALTER TABLE devices ADD COLUMN device_class        TEXT;      -- probed: C0 (unprobed), C1, C2, C3
ALTER TABLE devices ADD COLUMN class_override      TEXT;      -- admin override, only ever gentler than device_class
ALTER TABLE devices ADD COLUMN class_probed_at     DATETIME;  -- when device_class was last derived
ALTER TABLE devices ADD COLUMN poll_profile        TEXT NOT NULL DEFAULT 'auto'; -- 'auto' | 'light' (C1 cadences)
ALTER TABLE devices ADD COLUMN polling_paused_until DATETIME; -- NULL = not paused; liveness keeps running
ALTER TABLE devices ADD COLUMN cpu_count           INTEGER;   -- cpu-count from /system/resource
ALTER TABLE devices ADD COLUMN cpu_mhz_max         INTEGER;   -- highest cpu-frequency seen (scaling lowers it)

-- Who changed what on a device from the UI/API (pauses, profile and class
-- overrides, and from later milestones credential and job actions).
CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    user_id     TEXT,
    username    TEXT,
    action      TEXT NOT NULL,
    device_id   TEXT,
    detail      TEXT
);
CREATE INDEX idx_audit_log_device_at ON audit_log(device_id, at);

-- +goose Down
DROP INDEX IF EXISTS idx_audit_log_device_at;
DROP TABLE IF EXISTS audit_log;
ALTER TABLE devices DROP COLUMN cpu_mhz_max;
ALTER TABLE devices DROP COLUMN cpu_count;
ALTER TABLE devices DROP COLUMN polling_paused_until;
ALTER TABLE devices DROP COLUMN poll_profile;
ALTER TABLE devices DROP COLUMN class_probed_at;
ALTER TABLE devices DROP COLUMN class_override;
ALTER TABLE devices DROP COLUMN device_class;
