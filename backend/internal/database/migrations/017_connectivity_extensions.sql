-- +goose Up

-- Probe-source selection for ping targets: probe FROM a specific VLAN
-- interface / source IP (multi-ISP, policy-routed networks). Both are passed
-- to /ping (and /tool/traceroute) as =src-address= / =interface= when non-empty.
ALTER TABLE ping_targets ADD COLUMN src_address TEXT NOT NULL DEFAULT '';
ALTER TABLE ping_targets ADD COLUMN src_interface TEXT NOT NULL DEFAULT '';

-- speed_tests: scheduled /tool/fetch download measurements run from a RouterOS
-- device. device_id deliberately has NO foreign key: the test must survive
-- deletion of the device it pointed at (the poller records an error sample for
-- a missing device instead of the test silently disappearing).
CREATE TABLE speed_tests (
    id         TEXT PRIMARY KEY,
    device_id  TEXT NOT NULL DEFAULT '',
    url        TEXT NOT NULL,
    label      TEXT NOT NULL DEFAULT '',
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- speed_samples: one row per measurement. error != '' means the test could not
-- run (device offline/missing, dial failure, fetch trap/timeout); such rows
-- have mbps NULL.
CREATE TABLE speed_samples (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    test_id     TEXT NOT NULL REFERENCES speed_tests(id) ON DELETE CASCADE,
    device_id   TEXT NOT NULL DEFAULT '',
    mbps        REAL,                               -- NULL when the test failed
    bytes       INTEGER NOT NULL DEFAULT 0,         -- downloaded payload bytes
    duration_ms INTEGER NOT NULL DEFAULT 0,         -- elapsed download time
    error       TEXT NOT NULL DEFAULT '',
    recorded_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_speed_samples_test_time ON speed_samples(test_id, recorded_at);
-- Time-only index so the hourly retention purge (DELETE ... WHERE recorded_at < ?)
-- uses an index range instead of a full table scan (migration 014 precedent).
CREATE INDEX idx_speed_samples_time ON speed_samples(recorded_at);

-- traceroute_runs: path snapshots for internet ping targets — manual (run-now
-- endpoint) or auto-captured when a probe's loss crosses
-- traceroute_loss_threshold. hops is a JSON array of hop objects
-- (hop/address/loss_pct/sent/last_ms/avg_ms/best_ms/worst_ms/status).
CREATE TABLE traceroute_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    target_id   TEXT NOT NULL REFERENCES ping_targets(id) ON DELETE CASCADE,
    address     TEXT NOT NULL DEFAULT '',
    hops        TEXT NOT NULL DEFAULT '[]',         -- JSON array of hop objects
    error       TEXT NOT NULL DEFAULT '',
    recorded_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_traceroute_runs_target_time ON traceroute_runs(target_id, recorded_at);
CREATE INDEX idx_traceroute_runs_time ON traceroute_runs(recorded_at);

-- Runtime-tunable knobs (re-read each cycle / each probe).
INSERT INTO app_settings (key, value) VALUES ('speedtest_interval', '21600') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('traceroute_loss_threshold', '50') ON CONFLICT(key) DO NOTHING;

-- +goose Down

DROP TABLE IF EXISTS traceroute_runs;
DROP TABLE IF EXISTS speed_samples;
DROP TABLE IF EXISTS speed_tests;
-- SQLite 3.35+ (modernc) supports DROP COLUMN and neither column is indexed;
-- dropping them keeps a down->up cycle from failing on duplicate columns.
-- app_settings rows are left as-is (migration 015 precedent).
ALTER TABLE ping_targets DROP COLUMN src_interface;
ALTER TABLE ping_targets DROP COLUMN src_address;
