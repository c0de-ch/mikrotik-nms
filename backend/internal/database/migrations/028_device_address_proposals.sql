-- +goose Up

-- device_address_proposals: IP auto-follow no longer re-points a device on its
-- own. A managed device's MAC sighted by MNDP/CDP at a new address becomes a
-- proposal here; an admin approves it (the NMS then logs in at the new
-- address with the stored credential, verifies board/version/MAC and commits)
-- or dismisses it. MNDP is unauthenticated, so sending the device's
-- credential to an address learned from it needs a human decision until
-- P0-13 can verify a pinned TLS certificate before any login.
--
-- At most one pending row per (device, new address); a repeat sighting bumps
-- last_seen. Times are INTEGER unix seconds. Additive (M0 schema rule).
CREATE TABLE device_address_proposals (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    old_address TEXT NOT NULL,
    new_address TEXT NOT NULL,
    mac         TEXT NOT NULL DEFAULT '',
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'approved', 'dismissed', 'superseded', 'expired', 'failed')),
    decided_by  TEXT,
    decided_at  INTEGER,
    result      TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX device_address_proposals_pending
    ON device_address_proposals(device_id, new_address) WHERE status = 'pending';
CREATE INDEX device_address_proposals_recent ON device_address_proposals(last_seen);

-- +goose Down
DROP INDEX IF EXISTS device_address_proposals_recent;
DROP INDEX IF EXISTS device_address_proposals_pending;
DROP TABLE IF EXISTS device_address_proposals;
