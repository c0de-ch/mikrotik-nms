-- +goose Up
-- Flow-export guardrails (P0-7). Strictly additive (N-1 rule): two new
-- tables and two settings rows; flow_exporters is untouched, so every
-- exporter accepted before this migration stays accepted.

-- flow_sender_policy: senders the collector must not decode, keyed by UDP
-- source IP (netip.Addr.Unmap().String(), like flow_exporters.address).
-- A row is a tombstone left by deleting an exporter (source 'deleted') or a
-- managed switch / access point / CPE / C1 device blocked on its first
-- datagram (source 'auto'). Datagrams from a blocked address are dropped
-- before decode; the sender is shown as "blocked: <reason>" and only an
-- explicit admin action removes the row. An address is never both here and
-- in flow_exporters (an exporter row wins). Times are unix seconds.
CREATE TABLE flow_sender_policy (
    address    TEXT NOT NULL PRIMARY KEY,
    policy     TEXT NOT NULL DEFAULT 'blocked' CHECK (policy IN ('blocked')),
    reason     TEXT NOT NULL DEFAULT '',
    source     TEXT NOT NULL DEFAULT 'deleted',           -- deleted | auto
    name       TEXT NOT NULL DEFAULT '',                  -- the exporter's or device's name when blocked
    kind       TEXT NOT NULL DEFAULT 'other',             -- routeros | opnsense | other
    device_id  TEXT REFERENCES devices(id) ON DELETE SET NULL,
    created_by TEXT NOT NULL DEFAULT '',                  -- admin username; '' = the collector
    created_at INTEGER NOT NULL DEFAULT 0,
    last_seen  INTEGER NOT NULL DEFAULT 0,                -- last dropped datagram (0 = none yet)
    datagrams  INTEGER NOT NULL DEFAULT 0                 -- datagrams dropped since blocked
) WITHOUT ROWID;

-- flow_export_checks: one row per managed device whose address sent flow
-- datagrams although its role says it must not export (switch, access point,
-- CPE, C1). first_seen/last_seen are the sightings; checked_at is the last
-- read of /ip/traffic-flow on the device (at most one per hour, 0 = never)
-- and result what it found: '' (not read yet) | exporting | not_exporting |
-- failed. The flow_export_on_switch alert is critical only on 'exporting'.
-- collector_port is the collector's listen port when the datagrams arrived
-- and targets the device's enabled traffic-flow targets as that read found
-- them (space-separated addr:port): together they name the one target the
-- rollback text removes.
CREATE TABLE flow_export_checks (
    device_id      TEXT NOT NULL PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    address        TEXT NOT NULL DEFAULT '',
    collector_port INTEGER NOT NULL DEFAULT 0,
    first_seen     INTEGER NOT NULL DEFAULT 0,
    last_seen      INTEGER NOT NULL DEFAULT 0,
    checked_at     INTEGER NOT NULL DEFAULT 0,
    result         TEXT NOT NULL DEFAULT '',
    detail         TEXT NOT NULL DEFAULT '',
    targets        TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- Auto-accept is by class and off by default: '' = nothing is accepted
-- automatically, 'router' = router-role C3 devices with a fresh internet or
-- VPN egress of their own.
INSERT INTO app_settings (key, value) VALUES ('flow_auto_accept_classes', '') ON CONFLICT(key) DO NOTHING;
-- The previous release reads flow_auto_accept_devices (default true) and
-- accepts every managed address: it must read false on this schema too.
INSERT INTO app_settings (key, value) VALUES ('flow_auto_accept_devices', 'false')
    ON CONFLICT(key) DO UPDATE SET value = 'false';

-- +goose Down
DELETE FROM app_settings WHERE key = 'flow_auto_accept_classes';
DROP TABLE IF EXISTS flow_export_checks;
DROP TABLE IF EXISTS flow_sender_policy;
