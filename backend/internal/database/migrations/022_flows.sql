-- +goose Up

-- flow_exporters: allow-list of NetFlow/IPFIX/sFlow senders, keyed by UDP
-- source IP (netip.Addr.Unmap().String()), plus persisted health written by
-- the flow collector (internal/flow) about every 60 s. Counters are cumulative
-- since created_at. All times are unix seconds (0 = never). AUTOINCREMENT: an
-- id is never reused, so orphaned fact rows of a deleted exporter can never
-- attach to a new one (flow.Sweep purges them).
CREATE TABLE flow_exporters (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT NOT NULL,
    address           TEXT NOT NULL UNIQUE,
    kind              TEXT NOT NULL DEFAULT 'other',      -- routeros | opnsense | other
    device_id         TEXT REFERENCES devices(id) ON DELETE SET NULL,
    enabled           INTEGER NOT NULL DEFAULT 1,
    auto              INTEGER NOT NULL DEFAULT 0,         -- 1 = auto-accepted managed device
    sampling_override INTEGER NOT NULL DEFAULT 0,         -- forced 1-in-N when nothing is announced; 0 = off
    nat_addresses     TEXT NOT NULL DEFAULT '',           -- CSV of IPs this exporter's NATed traffic appears as elsewhere
    last_seen         INTEGER NOT NULL DEFAULT 0,
    protocol          TEXT NOT NULL DEFAULT '',           -- netflow5 | netflow9 | ipfix | sflow5
    sampling_rate     INTEGER NOT NULL DEFAULT 1,         -- last effective 1-in-N
    datagrams         INTEGER NOT NULL DEFAULT 0,
    flows             INTEGER NOT NULL DEFAULT 0,
    bytes             INTEGER NOT NULL DEFAULT 0,
    decode_errors     INTEGER NOT NULL DEFAULT 0,
    template_misses   INTEGER NOT NULL DEFAULT 0,
    seq_lost          INTEGER NOT NULL DEFAULT 0,
    clock_skew_ms     INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT '',
    last_error_at     INTEGER NOT NULL DEFAULT 0,
    created_at        INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER))
);
CREATE INDEX idx_flow_exporters_device ON flow_exporters(device_id);

-- flow_exporter_ifaces: ifIndex -> name per exporter. source = device (RouterOS
-- .id sync), learned (seen in flows; hint = top ingress source prefixes) or
-- manual (admin; never overwritten by a sync). last_seen = last flushed minute
-- with bytes on this ifIndex (any source).
CREATE TABLE flow_exporter_ifaces (
    exporter_id INTEGER NOT NULL REFERENCES flow_exporters(id) ON DELETE CASCADE,
    if_index    INTEGER NOT NULL,
    name        TEXT NOT NULL DEFAULT '',
    type        TEXT NOT NULL DEFAULT '',
    vlan_id     INTEGER NOT NULL DEFAULT 0,
    parent      TEXT NOT NULL DEFAULT '',                 -- VLAN parent interface (RouterOS /interface/vlan interface=)
    role        TEXT NOT NULL DEFAULT '',                 -- '' | lan | wan | vpn | other (admin)
    source      TEXT NOT NULL DEFAULT 'learned',          -- device | learned | manual
    hint        TEXT NOT NULL DEFAULT '',
    first_seen  INTEGER NOT NULL DEFAULT 0,
    last_seen   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (exporter_id, if_index)
) WITHOUT ROWID;

-- flow_points: observation points = one exporter + a set of its ifIndexes
-- (canonical ascending decimal CSV, e.g. "10,13"). facing up = the ifaces face
-- the Internet (download = in_if in set); down = they face hosts (download =
-- out_if in set). port_side relates (device_id, iface), the managed port the
-- point represents: same = the exporter's own iface, peer = the far end of the
-- wire. exclude_attach: JSON [{"device_id":"…","iface":"…"}] - rows whose local
-- endpoint's MAC is learned on one of these ports are dropped. Rows are never
-- summed across exporters.
CREATE TABLE flow_points (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    exporter_id    INTEGER NOT NULL REFERENCES flow_exporters(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    kind           TEXT NOT NULL,                         -- native | derived
    if_indexes     TEXT NOT NULL,
    facing         TEXT NOT NULL DEFAULT 'down',          -- up | down
    port_side      TEXT NOT NULL DEFAULT '',              -- '' | same | peer
    device_id      TEXT REFERENCES devices(id) ON DELETE SET NULL,
    iface          TEXT NOT NULL DEFAULT '',
    local_internal INTEGER NOT NULL DEFAULT 0,
    exclude_attach TEXT NOT NULL DEFAULT '[]',
    note           TEXT NOT NULL DEFAULT '',
    auto           INTEGER NOT NULL DEFAULT 1,
    enabled        INTEGER NOT NULL DEFAULT 1,
    created_at     INTEGER NOT NULL DEFAULT (CAST(strftime('%s','now') AS INTEGER))
);
CREATE UNIQUE INDEX idx_flow_points_native ON flow_points(exporter_id, if_indexes) WHERE kind = 'native';
CREATE UNIQUE INDEX idx_flow_points_derived ON flow_points(exporter_id, device_id, iface) WHERE kind = 'derived';
CREATE INDEX idx_flow_points_port ON flow_points(device_id, iface);

-- flow_templates: persisted v9/IPFIX (options) templates so a restart decodes
-- at once (OPNsense re-announces only every 600 s). fields = scope_count u16
-- BE, then per field: type u16 (bit 15 = enterprise), length u16, pen u32
-- (all BE). kind 0 = data template, 1 = options template.
CREATE TABLE flow_templates (
    exporter_id INTEGER NOT NULL REFERENCES flow_exporters(id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    obs_domain  INTEGER NOT NULL,
    template_id INTEGER NOT NULL,
    kind        INTEGER NOT NULL,
    fields      BLOB NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (exporter_id, version, obs_domain, template_id)
) WITHOUT ROWID;

-- flow_prefixes: address plan derived from managed devices' /ip(v6)/address
-- (class internal | vpn | transit), refreshed every 15 min by the collector.
CREATE TABLE flow_prefixes (
    prefix     TEXT NOT NULL PRIMARY KEY,                 -- canonical CIDR (netip.Prefix.Masked().String())
    class      TEXT NOT NULL,
    device_id  TEXT NOT NULL DEFAULT '',
    iface      TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
) WITHOUT ROWID;

-- flow_1m: per-exporter, per-minute top-N conversations per (in_if, out_if)
-- plus one exact "other" row per pair (src = dst = x''). bucket = unix seconds
-- of the UTC minute start. Addresses are the NAT-normalised inside view, 4 or
-- 16 bytes. bytes/packets are sampling-scaled estimates. No FK (size); orphan
-- rows of deleted exporters are purged in chunks by flow.Sweep.
CREATE TABLE flow_1m (
    exporter_id INTEGER NOT NULL,
    bucket      INTEGER NOT NULL,
    in_if       INTEGER NOT NULL,
    out_if      INTEGER NOT NULL,
    src         BLOB NOT NULL,
    dst         BLOB NOT NULL,
    proto       INTEGER NOT NULL,
    port        INTEGER NOT NULL,
    bytes       INTEGER NOT NULL,
    packets     INTEGER NOT NULL,
    flows       INTEGER NOT NULL,
    PRIMARY KEY (exporter_id, bucket, in_if, out_if, src, dst, proto, port)
) WITHOUT ROWID;

-- flow_1h: hourly rollup of flow_1m (bucket = hour start), top-N per pair + other.
CREATE TABLE flow_1h (
    exporter_id INTEGER NOT NULL,
    bucket      INTEGER NOT NULL,
    in_if       INTEGER NOT NULL,
    out_if      INTEGER NOT NULL,
    src         BLOB NOT NULL,
    dst         BLOB NOT NULL,
    proto       INTEGER NOT NULL,
    port        INTEGER NOT NULL,
    bytes       INTEGER NOT NULL,
    packets     INTEGER NOT NULL,
    flows       INTEGER NOT NULL,
    PRIMARY KEY (exporter_id, bucket, in_if, out_if, src, dst, proto, port)
) WITHOUT ROWID;

-- flow_*_meta: one row per (exporter, bucket) the exporter was alive for
-- (datagrams received), even with zero records. Presence = covered, absence = gap.
CREATE TABLE flow_1m_meta (
    exporter_id INTEGER NOT NULL,
    bucket      INTEGER NOT NULL,
    datagrams   INTEGER NOT NULL,
    records     INTEGER NOT NULL,
    bytes       INTEGER NOT NULL,                         -- total before top-N folding
    sampling    INTEGER NOT NULL,                         -- max 1-in-N in the bucket
    overflow    INTEGER NOT NULL,
    late        INTEGER NOT NULL,
    PRIMARY KEY (exporter_id, bucket)
) WITHOUT ROWID;
CREATE TABLE flow_1h_meta (
    exporter_id INTEGER NOT NULL,
    bucket      INTEGER NOT NULL,
    datagrams   INTEGER NOT NULL,
    records     INTEGER NOT NULL,
    bytes       INTEGER NOT NULL,
    sampling    INTEGER NOT NULL,
    overflow    INTEGER NOT NULL,
    late        INTEGER NOT NULL,
    minutes     INTEGER NOT NULL,                         -- covered minutes in the hour
    PRIMARY KEY (exporter_id, bucket)
) WITHOUT ROWID;

-- Runtime-tunable flow collector knobs (Settings → Flow collector).
INSERT INTO app_settings (key, value) VALUES ('flow_top_n', '50') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_top_n_hourly', '100') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_max_rows_per_minute', '1500') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_1m_days', '3') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_1h_days', '90') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_rate_limit_pps', '2000') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_auto_accept_devices', 'true') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_internal_prefixes', '') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_external_prefixes', '') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_resolve_ptr', 'false') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('flow_advertise_address', '') ON CONFLICT(key) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS flow_1h_meta;
DROP TABLE IF EXISTS flow_1m_meta;
DROP TABLE IF EXISTS flow_1h;
DROP TABLE IF EXISTS flow_1m;
DROP TABLE IF EXISTS flow_prefixes;
DROP TABLE IF EXISTS flow_templates;
DROP TABLE IF EXISTS flow_points;
DROP TABLE IF EXISTS flow_exporter_ifaces;
DROP TABLE IF EXISTS flow_exporters;
DELETE FROM app_settings WHERE key IN ('flow_top_n','flow_top_n_hourly','flow_max_rows_per_minute','flow_1m_days',
  'flow_1h_days','flow_rate_limit_pps','flow_auto_accept_devices','flow_internal_prefixes','flow_external_prefixes',
  'flow_resolve_ptr','flow_advertise_address');
