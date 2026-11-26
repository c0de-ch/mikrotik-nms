-- +goose Up
-- Minimal alerting: open and past alerts, silences, and where notifications
-- go. Strictly additive (N-1 rule). Times are INTEGER unix seconds, like the
-- flow fact tables.
--
-- An alert is one firing of a rule for a key (usually a device id). While it
-- is not resolved there is at most one row per (rule, key): a condition that
-- keeps holding updates last_at instead of adding rows.
CREATE TABLE alerts (
    id               INTEGER PRIMARY KEY,
    rule             TEXT NOT NULL,                 -- device_unreachable | parallel_api_poller | …
    key              TEXT NOT NULL,                 -- what the rule fired for
    site_id          TEXT,                          -- reserved for multi-site (NULL = the one site)
    device_id        TEXT,
    severity         TEXT NOT NULL,                 -- info | warning | critical
    state            TEXT NOT NULL CHECK (state IN ('firing', 'acked', 'resolved')),
    first_at         INTEGER NOT NULL,
    last_at          INTEGER NOT NULL,              -- last evaluation that still saw the condition
    last_notified_at INTEGER,                       -- NULL = no notification delivered yet
    resolved_at      INTEGER,
    acked_by         TEXT,
    acked_at         INTEGER,
    confirmed        INTEGER NOT NULL DEFAULT 1,    -- 0 = raised on unconfirmed evidence
    payload          TEXT NOT NULL DEFAULT '{}'     -- JSON: summary and rule-specific facts
);
CREATE UNIQUE INDEX alerts_open ON alerts(rule, key) WHERE state <> 'resolved';
CREATE INDEX alerts_recent ON alerts(last_at);

-- A silence stops notifications (never the alert itself) until it expires.
CREATE TABLE alert_silences (
    id         INTEGER PRIMARY KEY,
    scope      TEXT NOT NULL CHECK (scope IN ('site', 'device', 'rule')),
    target     TEXT NOT NULL,                       -- device id, rule name, or site id ('' = the one site)
    until      INTEGER NOT NULL,
    reason     TEXT,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX alert_silences_until ON alert_silences(until);

-- Where notifications are delivered. target_enc is an e-mail address or a
-- webhook URL — a webhook URL is a bearer secret — encrypted like device
-- passwords (enc:v1) and never returned in full by the API.
CREATE TABLE notification_channels (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL CHECK (kind IN ('smtp', 'webhook')),
    target_enc   TEXT NOT NULL,
    min_severity TEXT NOT NULL DEFAULT 'warning',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS notification_channels;
DROP INDEX IF EXISTS alert_silences_until;
DROP TABLE IF EXISTS alert_silences;
DROP INDEX IF EXISTS alerts_recent;
DROP INDEX IF EXISTS alerts_open;
DROP TABLE IF EXISTS alerts;
