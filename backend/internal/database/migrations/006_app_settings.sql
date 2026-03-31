-- +goose Up

CREATE TABLE app_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

-- Default intervals (in seconds)
INSERT INTO app_settings (key, value) VALUES ('health_interval', '30');
INSERT INTO app_settings (key, value) VALUES ('topology_interval', '60');
INSERT INTO app_settings (key, value) VALUES ('firmware_interval', '21600');
INSERT INTO app_settings (key, value) VALUES ('wifi_interval', '30');
INSERT INTO app_settings (key, value) VALUES ('client_discovery_interval', '900');
INSERT INTO app_settings (key, value) VALUES ('retention_days', '7');
INSERT INTO app_settings (key, value) VALUES ('dark_mode', 'false');

-- +goose Down

DROP TABLE IF EXISTS app_settings;
