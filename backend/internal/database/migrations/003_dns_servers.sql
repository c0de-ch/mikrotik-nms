-- +goose Up

CREATE TABLE dns_servers (
    id      TEXT PRIMARY KEY,
    name    TEXT NOT NULL DEFAULT '',
    address TEXT NOT NULL,
    port    INTEGER NOT NULL DEFAULT 53,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down

DROP TABLE IF EXISTS dns_servers;
