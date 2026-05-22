-- +goose Up

-- Extend interface_state with the extra runtime fields needed to surface
-- WHY a port is disabled (admin / loop-protect / slave) and to render a
-- human label for it.
ALTER TABLE interface_state ADD COLUMN loop_protect_status TEXT NOT NULL DEFAULT '';
ALTER TABLE interface_state ADD COLUMN slave               INTEGER NOT NULL DEFAULT 0;
ALTER TABLE interface_state ADD COLUMN comment             TEXT NOT NULL DEFAULT '';

-- A new event kind piggy-backs on loop_events:
--   port_loop_protect — RouterOS loop-protect tripped (critical)
-- No schema change to loop_events is needed; the existing port_interface
-- column already carries the interface name.

-- +goose Down

ALTER TABLE interface_state DROP COLUMN loop_protect_status;
ALTER TABLE interface_state DROP COLUMN slave;
ALTER TABLE interface_state DROP COLUMN comment;
