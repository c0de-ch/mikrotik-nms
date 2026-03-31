-- +goose Up

CREATE TABLE wifi_history (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    mac_address     TEXT NOT NULL,
    ip_address      TEXT DEFAULT '',
    host_name       TEXT DEFAULT '',
    ap_name         TEXT NOT NULL DEFAULT '',
    ssid            TEXT DEFAULT '',
    band            TEXT DEFAULT '',
    channel         TEXT DEFAULT '',
    signal          TEXT DEFAULT '',
    tx_rate         TEXT DEFAULT '',
    rx_rate         TEXT DEFAULT '',
    event           TEXT NOT NULL DEFAULT 'seen',  -- 'seen', 'roam', 'join', 'leave'
    controller_id   TEXT DEFAULT '',
    recorded_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_wifi_history_mac ON wifi_history(mac_address, recorded_at);
CREATE INDEX idx_wifi_history_ap ON wifi_history(ap_name, recorded_at);
CREATE INDEX idx_wifi_history_time ON wifi_history(recorded_at);

-- +goose Down

DROP TABLE IF EXISTS wifi_history;
