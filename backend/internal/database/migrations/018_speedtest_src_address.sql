-- +goose Up
-- Optional source address for speed tests: /tool/fetch supports src-address
-- (verified on RouterOS 7.23; it has no interface/vrf parameter), so a test
-- can egress via a specific VLAN by sourcing from the device's IP on it.
ALTER TABLE speed_tests ADD COLUMN src_address TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE speed_tests DROP COLUMN src_address;
