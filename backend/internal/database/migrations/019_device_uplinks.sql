-- +goose Up
-- Egress paths discovered per device: active IPv4 default routes and running
-- VPN tunnel interfaces. Refreshed by the topology poller each cycle; the
-- topology builder synthesizes Internet / gateway / VPN nodes from these rows.
CREATE TABLE device_uplinks (
    id          TEXT PRIMARY KEY,          -- device_id + ":" + kind + ":" + interface + ":" + gateway_ip
    device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,             -- 'default-route' | 'vpn'
    interface   TEXT NOT NULL DEFAULT '',  -- egress interface (may be '' when unknown)
    iface_type  TEXT NOT NULL DEFAULT '',  -- RouterOS type of that interface (wg, lte, ether, …)
    gateway_ip  TEXT NOT NULL DEFAULT '',  -- next-hop for default routes, '' for vpn ifaces
    last_seen   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_device_uplinks_device ON device_uplinks(device_id);

-- +goose Down
DROP TABLE device_uplinks;
