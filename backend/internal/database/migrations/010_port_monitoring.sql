-- +goose Up

-- interface_state: per-device per-interface runtime snapshot, refreshed by the
-- network-health poller every cycle. Used by the UI for "current state"
-- rendering of every monitored port. The audit trail of state transitions
-- (port_disabled / port_link_down / port_link_flap) lives in loop_events,
-- alongside the L2 loop / STP events.
--
-- This table is intentionally separate from the existing `interfaces` table:
--   - `interfaces` is updated by the health poller (every health_interval) and
--     stores the slow-moving inventory (name/type/MAC/MTU/comment).
--   - `interface_state` is updated by the network-health poller (every
--     network_health_interval) and stores the fast-moving runtime fields
--     (running, disabled, last-link-up/down timestamps, recent flap counter).
CREATE TABLE interface_state (
    id                 TEXT PRIMARY KEY,             -- device_id:iface_name
    device_id          TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    interface_name     TEXT NOT NULL,
    interface_type     TEXT NOT NULL DEFAULT '',
    running            INTEGER NOT NULL DEFAULT 0,
    disabled           INTEGER NOT NULL DEFAULT 0,
    last_link_up       TEXT NOT NULL DEFAULT '',     -- raw RouterOS string
    last_link_down     TEXT NOT NULL DEFAULT '',
    flap_count_window  INTEGER NOT NULL DEFAULT 0,   -- transitions in current window
    last_polled        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(device_id, interface_name)
);
CREATE INDEX idx_interface_state_device ON interface_state(device_id);

-- The existing loop_events.severity CHECK is ('warn', 'critical'). Port events
-- piggy-back on this table — kinds added by the port poller:
--   port_disabled    — admin disabled an interface (warn)
--   port_link_down   — running port lost link (warn)
--   port_link_flap   — N or more transitions within window (critical)
--
-- No schema change to loop_events is needed; the port_interface column already
-- exists and is used by the existing event types.

-- +goose Down

DROP TABLE IF EXISTS interface_state;
