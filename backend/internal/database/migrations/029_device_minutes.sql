-- +goose Up

-- device_minutes: one row per device-minute the NMS was running for — the
-- device's counter coverage, its CPU and memory telemetry and what the NMS
-- itself sent it (P0-6). Written by the poller's load recorder when a minute
-- becomes final, only on an instance that polls. bucket = UTC minute start in
-- unix seconds, like the flow tables. No foreign key, for size: a deleted
-- device's rows are swept by retention.
--
--   polled_s     seconds of the minute inside a usable interval between two
--                Tier 0 (interface counter) reads; > 0 = the minute is covered
--   tier0_ok     Tier 0 reads that succeeded in the minute
--   reachable_s  seconds of the minute the liveness check saw the device up
--   paused       1 = an admin pause was in force during the minute
--   gap_reason   why polled_s is 0 (paused, unreachable, no_session,
--                read_failed, no_baseline, not_polled); NULL when covered
--   cpu_sum / cpu_samples   quiet-window CPU samples (average = sum/samples)
--   cpu_max      highest quiet-window sample
--   cpu_post     CPU 3 s after a Tier 0 read (every 10 min): the NMS's own effect
--   nms_*        commands, wall time, reply rows, timeouts and reconnects of
--                this device's API sessions in the minute
--   protect_level, nms_cost_ms   reserved (0) until the scheduler exists
--
-- A minute without a row was not observed at all (the NMS was down). Kept 14 d.
CREATE TABLE device_minutes (
    device_id       TEXT    NOT NULL,
    bucket          INTEGER NOT NULL,
    polled_s        INTEGER NOT NULL DEFAULT 0,
    tier0_ok        INTEGER NOT NULL DEFAULT 0,
    reachable_s     INTEGER NOT NULL DEFAULT 0,
    paused          INTEGER NOT NULL DEFAULT 0,
    protect_level   INTEGER NOT NULL DEFAULT 0,
    gap_reason      TEXT,
    cpu_sum         INTEGER,
    cpu_samples     INTEGER NOT NULL DEFAULT 0,
    cpu_max         INTEGER,
    cpu_post        INTEGER,
    free_memory_min INTEGER,
    uptime_s        INTEGER,
    rebooted        INTEGER NOT NULL DEFAULT 0,
    nms_cmds        INTEGER NOT NULL DEFAULT 0,
    nms_wall_ms     INTEGER NOT NULL DEFAULT 0,
    nms_cost_ms     INTEGER NOT NULL DEFAULT 0,
    nms_rows        INTEGER NOT NULL DEFAULT 0,
    nms_timeouts    INTEGER NOT NULL DEFAULT 0,
    nms_reconnects  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (device_id, bucket)
) WITHOUT ROWID;
-- Fleet-wide reads by time (alert rules, coverage sums, retention); carries
-- polled_s so coverage sums never touch the rows.
CREATE INDEX idx_device_minutes_bucket ON device_minutes(bucket, polled_s);

-- device_hours: hourly rollup of device_minutes (bucket = UTC hour start in
-- unix seconds). covered_s = SUM(polled_s); cpu_avg / cpu_max / cpu_p95 over
-- the quiet-window samples (p95 of the minute averages). Kept 730 d.
CREATE TABLE device_hours (
    device_id    TEXT    NOT NULL,
    bucket       INTEGER NOT NULL,
    covered_s    INTEGER,
    cpu_avg      REAL,
    cpu_max      INTEGER,
    cpu_p95      INTEGER,
    mem_free_min INTEGER,
    nms_cmds     INTEGER,
    nms_cost_ms  INTEGER,
    ros_version  TEXT,
    PRIMARY KEY (device_id, bucket)
) WITHOUT ROWID;
CREATE INDEX idx_device_hours_bucket ON device_hours(bucket, covered_s);

-- Per-port drop, error and link-down deltas, and the seconds of the bucket
-- the port's counters were observed for. NULL = the interface does not carry
-- the attribute (or the row predates this migration).
ALTER TABLE port_stats_1m ADD COLUMN rx_drop       INTEGER;
ALTER TABLE port_stats_1m ADD COLUMN tx_drop       INTEGER;
ALTER TABLE port_stats_1m ADD COLUMN rx_error      INTEGER;
ALTER TABLE port_stats_1m ADD COLUMN tx_error      INTEGER;
ALTER TABLE port_stats_1m ADD COLUMN tx_queue_drop INTEGER;
ALTER TABLE port_stats_1m ADD COLUMN link_downs    INTEGER;
ALTER TABLE port_stats_1m ADD COLUMN covered_s     INTEGER;

ALTER TABLE port_stats_1h ADD COLUMN rx_drop       INTEGER;
ALTER TABLE port_stats_1h ADD COLUMN tx_drop       INTEGER;
ALTER TABLE port_stats_1h ADD COLUMN rx_error      INTEGER;
ALTER TABLE port_stats_1h ADD COLUMN tx_error      INTEGER;
ALTER TABLE port_stats_1h ADD COLUMN tx_queue_drop INTEGER;
ALTER TABLE port_stats_1h ADD COLUMN link_downs    INTEGER;
ALTER TABLE port_stats_1h ADD COLUMN covered_s     INTEGER;

-- +goose Down
ALTER TABLE port_stats_1h DROP COLUMN covered_s;
ALTER TABLE port_stats_1h DROP COLUMN link_downs;
ALTER TABLE port_stats_1h DROP COLUMN tx_queue_drop;
ALTER TABLE port_stats_1h DROP COLUMN tx_error;
ALTER TABLE port_stats_1h DROP COLUMN rx_error;
ALTER TABLE port_stats_1h DROP COLUMN tx_drop;
ALTER TABLE port_stats_1h DROP COLUMN rx_drop;
ALTER TABLE port_stats_1m DROP COLUMN covered_s;
ALTER TABLE port_stats_1m DROP COLUMN link_downs;
ALTER TABLE port_stats_1m DROP COLUMN tx_queue_drop;
ALTER TABLE port_stats_1m DROP COLUMN tx_error;
ALTER TABLE port_stats_1m DROP COLUMN rx_error;
ALTER TABLE port_stats_1m DROP COLUMN tx_drop;
ALTER TABLE port_stats_1m DROP COLUMN rx_drop;
DROP INDEX IF EXISTS idx_device_hours_bucket;
DROP TABLE IF EXISTS device_hours;
DROP INDEX IF EXISTS idx_device_minutes_bucket;
DROP TABLE IF EXISTS device_minutes;
