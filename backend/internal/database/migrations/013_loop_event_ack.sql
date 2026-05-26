-- +goose Up

-- Acknowledgement state for loop_events. An acknowledged event no longer counts
-- as an active alert (it's excluded from CountRecentLoopEvents) and can be hidden
-- in the UI.
ALTER TABLE loop_events ADD COLUMN acknowledged INTEGER NOT NULL DEFAULT 0;
ALTER TABLE loop_events ADD COLUMN acknowledged_at DATETIME;

-- +goose Down

-- no-op: SQLite cannot easily drop a column without a full table rebuild, and
-- the rest of the schema (loop_events table) is dropped by 009's Down anyway.
-- Leaving the columns in place on a down-migration is harmless.
