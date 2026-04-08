-- +goose Up

-- Track WHERE each wifi_history row came from so the UI can show
-- provenance on join/leave/roam events.
--
--   source values: 'log'      = parsed from controller wireless log
--                  'snapshot' = inferred from registration-table polling
--                  'absence'  = absence safety net (client missing for N polls)
--                  ''         = legacy / unknown (rows written before this migration)
--
--   reason is the disconnect reason captured from the log line, e.g.
--   'connection lost' or 'not responding'. Empty for non-leave events
--   and for absence-fallback leaves.

ALTER TABLE wifi_history ADD COLUMN source TEXT NOT NULL DEFAULT '';
ALTER TABLE wifi_history ADD COLUMN reason TEXT NOT NULL DEFAULT '';

-- +goose Down

-- SQLite supports DROP COLUMN since 3.35; modernc.org/sqlite is well past
-- that. The downgrade just removes the columns again.
ALTER TABLE wifi_history DROP COLUMN source;
ALTER TABLE wifi_history DROP COLUMN reason;
