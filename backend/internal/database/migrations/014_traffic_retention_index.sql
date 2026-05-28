-- +goose Up

-- The retention sweep deletes by collected_at alone:
--   DELETE FROM traffic_samples WHERE collected_at < ?
-- The existing idx_traffic_device_time index leads with device_id, so it cannot
-- accelerate that range scan. This dedicated index lets the hourly purge use an
-- index range instead of a full table scan on the largest table.
CREATE INDEX IF NOT EXISTS idx_traffic_collected_at ON traffic_samples(collected_at);

-- +goose Down

DROP INDEX IF EXISTS idx_traffic_collected_at;
