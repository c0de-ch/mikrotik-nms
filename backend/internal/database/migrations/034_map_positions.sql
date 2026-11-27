-- +goose Up
-- map_positions: where an admin placed each node of the network map, shared
-- by every user. node_id is the /topology node id (a device id, "internet",
-- "gw:<ip>", "vpn:<device>:<iface>"); x/y are cytoscape model coordinates.
-- A node without a row is placed by the layout. Strictly additive (N-1
-- rule): a new table the previous release never reads. Times are unix
-- seconds.
CREATE TABLE map_positions (
    node_id    TEXT    PRIMARY KEY,
    x          REAL    NOT NULL,
    y          REAL    NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT    NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- +goose Down
DROP TABLE IF EXISTS map_positions;
