-- +goose Up

-- interface_relations: per-device interface stacking, dumped by the port-stats
-- collector's hosts loop every port_hosts_interval. kind 'bond': interface_name
-- is a member (slave) of the bond named in parent (/interface/bonding slaves);
-- kind 'vlan' / 'pppoe': interface_name is a VLAN / PPPoE client running over
-- parent (/interface/vlan, /interface/pppoe-client interface=). The traffic
-- role graph folds bond members into their bond (the FDB learns on the bond)
-- and marks the physical port under a VLAN/PPPoE WAN as facing upstream.
-- Each device's rows are replaced wholesale on every successful dump.
CREATE TABLE interface_relations (
    device_id      TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    interface_name TEXT NOT NULL,
    kind           TEXT NOT NULL,
    parent         TEXT NOT NULL,
    updated_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (device_id, interface_name, kind)
) WITHOUT ROWID;

-- +goose Down

DROP TABLE IF EXISTS interface_relations;
