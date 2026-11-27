-- +goose Up
-- The hygiene audit and the diagnose pass (P0-10), and the CPU watch after a
-- flow exporter is accepted (G6). Strictly additive (N-1 rule): three new
-- tables and nullable columns; the previous release reads and writes
-- device_findings and bridge_port_status by column name and never sees them.
-- Times are unix seconds.

-- device_audit: a device's configuration as the audit read it — the answers
-- of its read-only, proplisted prints, without counters or sessions and
-- never with a secret (no community names, passwords, keys or scripts are
-- asked for). A row is written only when the hash differs from the device's
-- newest row, so the table is the history of configuration changes the audit
-- saw. findings is what the rules made of that snapshot (JSON).
CREATE TABLE device_audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id   TEXT    NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    taken_at    INTEGER NOT NULL,
    hash        TEXT    NOT NULL,
    ros_version TEXT    NOT NULL DEFAULT '',
    snapshot    TEXT    NOT NULL,
    findings    TEXT    NOT NULL DEFAULT '[]'
);
CREATE INDEX idx_device_audit_device ON device_audit(device_id, taken_at);

-- device_audit_runs: one row per pass — a scheduled or requested audit, or a
-- diagnose pass. It is the memory of the budgets (one audit and one diagnose
-- pass per device per hour, across restarts), of which alert a diagnose pass
-- answered, and of what it found: the alert rules attach the newest diagnose
-- pass of a device as its "likely cause".
--   kind         audit | diagnose
--   reason       scheduled | admin | cpu_high | flow_export_on_switch |
--                flow_export_cpu_high
--   alert_id     the alert an automatic diagnose pass ran for (0 = none)
--   status       running | complete | partial (a read failed) | aborted
--   finished_at  0 while running
--   reads        commands sent; failed_reads those that failed
--   hash         the snapshot hash of a complete audit ('' otherwise)
--   runtime      counters the next pass compares against (JSON)
--   findings     what this pass found (JSON)
--   checks       what this pass saw of each usual device-side cause of a high
--                CPU — absent, present or not read — finding or not (JSON):
--                the sentence of a pass that found nothing is built from it
CREATE TABLE device_audit_runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id    TEXT    NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind         TEXT    NOT NULL CHECK (kind IN ('audit', 'diagnose')),
    reason       TEXT    NOT NULL DEFAULT '',
    alert_id     INTEGER NOT NULL DEFAULT 0,
    requested_by TEXT    NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL,
    finished_at  INTEGER NOT NULL DEFAULT 0,
    status       TEXT    NOT NULL DEFAULT 'running',
    reads        INTEGER NOT NULL DEFAULT 0,
    failed_reads INTEGER NOT NULL DEFAULT 0,
    hash         TEXT    NOT NULL DEFAULT '',
    detail       TEXT    NOT NULL DEFAULT '',
    runtime      TEXT    NOT NULL DEFAULT '',
    findings     TEXT    NOT NULL DEFAULT '[]',
    checks       TEXT    NOT NULL DEFAULT '[]'
);
CREATE INDEX idx_device_audit_runs_device ON device_audit_runs(device_id, kind, started_at);

-- Audit findings live in device_findings with what an operator needs to act:
-- a title, a plain explanation and the suggested fix (JSON list of steps:
-- text, commands, read_only, window). source = 'audit' marks the rows a pass
-- owns — it removes them once it finds the setting gone. NULL everywhere for
-- the findings of the login watch, the TLS pin check and the flow guard.
ALTER TABLE device_findings ADD COLUMN title TEXT;
ALTER TABLE device_findings ADD COLUMN explanation TEXT;
ALTER TABLE device_findings ADD COLUMN fix TEXT;
ALTER TABLE device_findings ADD COLUMN source TEXT;
CREATE INDEX idx_device_findings_source ON device_findings(source, severity);

-- The bridge port print already returns hw (hardware offload requested) and
-- horizon; the offload rules need them next to hw_offload. NULL = not
-- reported, or a row written before this migration.
ALTER TABLE bridge_port_status ADD COLUMN hw INTEGER;
ALTER TABLE bridge_port_status ADD COLUMN horizon TEXT;

-- flow_export_watches: the CPU watch that starts when a flow exporter of a
-- managed device is accepted (G6). One row per device; accepting again
-- restarts it. baseline is the device's average quiet-window CPU over the
-- 24 h before (NULL = no data). The flow_export_cpu_high rule reads the
-- device's minutes of the 30 min after accepted_at against it.
-- collector_port is the collector's listen port then, for the rollback text.
CREATE TABLE flow_export_watches (
    device_id      TEXT    NOT NULL PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    exporter_id    INTEGER NOT NULL DEFAULT 0,
    address        TEXT    NOT NULL DEFAULT '',
    collector_port INTEGER NOT NULL DEFAULT 0,
    accepted_at    INTEGER NOT NULL,
    baseline       REAL
) WITHOUT ROWID;

-- +goose Down
DROP TABLE IF EXISTS flow_export_watches;
ALTER TABLE bridge_port_status DROP COLUMN horizon;
ALTER TABLE bridge_port_status DROP COLUMN hw;
DROP INDEX IF EXISTS idx_device_findings_source;
ALTER TABLE device_findings DROP COLUMN source;
ALTER TABLE device_findings DROP COLUMN fix;
ALTER TABLE device_findings DROP COLUMN explanation;
ALTER TABLE device_findings DROP COLUMN title;
DROP INDEX IF EXISTS idx_device_audit_runs_device;
DROP TABLE IF EXISTS device_audit_runs;
DROP INDEX IF EXISTS idx_device_audit_device;
DROP TABLE IF EXISTS device_audit;
