package queries

import (
	"database/sql"
	"time"
)

type UpgradeJob struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	Reboot      bool       `json:"reboot"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type UpgradeJobDevice struct {
	ID          string     `json:"id"`
	JobID       string     `json:"job_id"`
	DeviceID    string     `json:"device_id"`
	Status      string     `json:"status"`
	Message     string     `json:"message"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

func CreateUpgradeJob(db *sql.DB, job *UpgradeJob) error {
	_, err := db.Exec(
		`INSERT INTO upgrade_jobs (id, status, reboot) VALUES (?, ?, ?)`,
		job.ID, job.Status, job.Reboot,
	)
	return err
}

func CreateUpgradeJobDevice(db *sql.DB, jd *UpgradeJobDevice) error {
	_, err := db.Exec(
		`INSERT INTO upgrade_job_devices (id, job_id, device_id, status) VALUES (?, ?, ?, ?)`,
		jd.ID, jd.JobID, jd.DeviceID, jd.Status,
	)
	return err
}

func UpdateUpgradeJobDeviceStatus(db *sql.DB, id, status, message string) error {
	var err error
	if status == "completed" || status == "failed" {
		_, err = db.Exec(
			`UPDATE upgrade_job_devices SET status=?, message=?, completed_at=CURRENT_TIMESTAMP WHERE id=?`,
			status, message, id,
		)
	} else {
		_, err = db.Exec(
			`UPDATE upgrade_job_devices SET status=?, message=?, started_at=COALESCE(started_at, CURRENT_TIMESTAMP) WHERE id=?`,
			status, message, id,
		)
	}
	return err
}

func UpdateUpgradeJobStatus(db *sql.DB, id, status string) error {
	if status == "completed" || status == "failed" {
		_, err := db.Exec(
			`UPDATE upgrade_jobs SET status=?, completed_at=CURRENT_TIMESTAMP WHERE id=?`,
			status, id,
		)
		return err
	}
	_, err := db.Exec(`UPDATE upgrade_jobs SET status=? WHERE id=?`, status, id)
	return err
}

func GetUpgradeJob(db *sql.DB, id string) (*UpgradeJob, error) {
	j := &UpgradeJob{}
	err := db.QueryRow(
		`SELECT id, status, reboot, created_at, completed_at FROM upgrade_jobs WHERE id=?`, id,
	).Scan(&j.ID, &j.Status, &j.Reboot, &j.CreatedAt, &j.CompletedAt)
	if err != nil {
		return nil, err
	}
	return j, nil
}

func ListUpgradeJobDevices(db *sql.DB, jobID string) ([]UpgradeJobDevice, error) {
	rows, err := db.Query(
		`SELECT id, job_id, device_id, status, message, started_at, completed_at
		 FROM upgrade_job_devices WHERE job_id=? ORDER BY device_id`, jobID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []UpgradeJobDevice
	for rows.Next() {
		var d UpgradeJobDevice
		if err := rows.Scan(&d.ID, &d.JobID, &d.DeviceID, &d.Status, &d.Message,
			&d.StartedAt, &d.CompletedAt); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}
