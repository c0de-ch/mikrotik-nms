-- +goose Up

-- P0-1: STP runtime state now comes from one proplisted
-- /interface/bridge/port/print (RouterOS >= 7.24) instead of one monitor per
-- port, and the per-port topology-changes counter is the only TCN source
-- (the bridge monitor has no such field). NULL in the new columns means
-- "not reported by this device's RouterOS", which the detectors treat as
-- unknown rather than 0. Additive only: the previous release ignores them.
ALTER TABLE bridge_port_status ADD COLUMN topology_changes INTEGER;
ALTER TABLE bridge_port_status ADD COLUMN forwarding INTEGER;
ALTER TABLE bridge_port_status ADD COLUMN learning INTEGER;
ALTER TABLE bridge_port_status ADD COLUMN inactive INTEGER;
ALTER TABLE bridge_port_status ADD COLUMN disabled INTEGER;
ALTER TABLE bridge_port_status ADD COLUMN hw_offload INTEGER;
ALTER TABLE bridge_port_status ADD COLUMN hw_offload_group TEXT;
ALTER TABLE bridge_port_status ADD COLUMN pvid INTEGER;
ALTER TABLE bridge_port_status ADD COLUMN port_state TEXT;   -- raw status: in-bridge | inactive
ALTER TABLE bridge_port_status ADD COLUMN edge_config TEXT;  -- configured edge: auto | yes | no | yes-discover | no-discover

-- How the device's port STP state was read: print | monitor-batched |
-- monitor-per-port | none (see routeros.BridgePath*).
ALTER TABLE bridge_status ADD COLUMN stp_source TEXT NOT NULL DEFAULT '';

-- +goose Down

-- no-op: dropping columns needs a table rebuild in SQLite, and the previous
-- release runs unchanged with the extra columns in place (N-1 compatible).
