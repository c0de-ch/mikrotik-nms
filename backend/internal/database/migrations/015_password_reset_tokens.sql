-- +goose Up

CREATE TABLE password_reset_tokens (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,            -- sha256(token) hex, never the raw token
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at  DATETIME NOT NULL,
    consumed_at DATETIME                          -- NULL until used; single-use guard
);

CREATE INDEX idx_pwreset_user    ON password_reset_tokens(user_id, consumed_at);
CREATE INDEX idx_pwreset_expires ON password_reset_tokens(expires_at);

-- Session-invalidation counter: bumped on password reset. Embedded in access +
-- refresh JWT claims; refresh is rejected when the claim's tv != users.token_version.
ALTER TABLE users ADD COLUMN token_version INTEGER NOT NULL DEFAULT 0;

-- Informational/admin toggles for the Settings UI. Authoritative SMTP config is
-- env-based; smtp_configured is computed at request time (not stored here).
INSERT INTO app_settings (key, value) VALUES ('smtp_from_address', '') ON CONFLICT(key) DO NOTHING;
INSERT INTO app_settings (key, value) VALUES ('pwreset_enabled', 'true') ON CONFLICT(key) DO NOTHING;

-- +goose Down

DROP TABLE IF EXISTS password_reset_tokens;
-- SQLite cannot easily drop a column; users.token_version is left in place
-- (harmless, defaults to 0). app_settings rows are left as-is.
