-- +goose Up

-- bridge_vlans: one row per (device, bridge, vlan-ids) entry from
-- /interface/bridge/vlan. Snapshot of the bridge VLAN table, refreshed on each
-- network-health poll cycle. Records both the statically configured tagged /
-- untagged port lists and the runtime (current) lists.
CREATE TABLE bridge_vlans (
    id                TEXT PRIMARY KEY,  -- device_id + ":" + bridge + ":" + vlan_ids
    device_id         TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    bridge_name       TEXT NOT NULL DEFAULT '',
    vlan_ids          TEXT NOT NULL DEFAULT '',  -- e.g. "28" or "28,78"
    tagged            TEXT NOT NULL DEFAULT '',
    untagged          TEXT NOT NULL DEFAULT '',
    current_tagged    TEXT NOT NULL DEFAULT '',
    current_untagged  TEXT NOT NULL DEFAULT '',
    comment           TEXT NOT NULL DEFAULT '',
    last_polled       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(device_id, bridge_name, vlan_ids)
);
CREATE INDEX idx_bridge_vlans_device ON bridge_vlans(device_id);

-- vlan_labels: user-editable metadata for a VLAN ID across the whole fleet.
CREATE TABLE vlan_labels (
    vlan_id     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL DEFAULT '',
    purpose     TEXT NOT NULL DEFAULT '',
    color       TEXT NOT NULL DEFAULT '',
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down

DROP TABLE IF EXISTS vlan_labels;
DROP TABLE IF EXISTS bridge_vlans;
