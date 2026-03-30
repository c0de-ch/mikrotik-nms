-- +goose Up

CREATE TABLE upgrade_jobs (
    id              TEXT PRIMARY KEY,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    reboot          INTEGER NOT NULL DEFAULT 1,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at    DATETIME
);

CREATE TABLE upgrade_job_devices (
    id              TEXT PRIMARY KEY,
    job_id          TEXT NOT NULL REFERENCES upgrade_jobs(id) ON DELETE CASCADE,
    device_id       TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'downloading', 'installing', 'rebooting', 'verifying', 'completed', 'failed')),
    message         TEXT DEFAULT '',
    started_at      DATETIME,
    completed_at    DATETIME,

    UNIQUE(job_id, device_id)
);
CREATE INDEX idx_upgrade_job_devices_job ON upgrade_job_devices(job_id);

-- +goose Down

DROP TABLE IF EXISTS upgrade_job_devices;
DROP TABLE IF EXISTS upgrade_jobs;
