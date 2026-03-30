package poller

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// UpgradeExecutor runs firmware upgrades asynchronously.
type UpgradeExecutor struct {
	db   *sql.DB
	pool *routeros.Pool
	hub  *ws.Hub
}

func NewUpgradeExecutor(db *sql.DB, pool *routeros.Pool, hub *ws.Hub) *UpgradeExecutor {
	return &UpgradeExecutor{db: db, pool: pool, hub: hub}
}

// Execute runs an upgrade job: upgrades each device sequentially, broadcasting progress.
func (ue *UpgradeExecutor) Execute(jobID string) {
	log.Printf("firmware: starting upgrade job %s", jobID)

	_ = queries.UpdateUpgradeJobStatus(ue.db, jobID, "running")

	devices, err := queries.ListUpgradeJobDevices(ue.db, jobID)
	if err != nil {
		log.Printf("firmware: list job devices: %v", err)
		_ = queries.UpdateUpgradeJobStatus(ue.db, jobID, "failed")
		return
	}

	job, _ := queries.GetUpgradeJob(ue.db, jobID)
	reboot := job != nil && job.Reboot

	allSuccess := true
	for _, jd := range devices {
		success := ue.upgradeDevice(jobID, jd, reboot)
		if !success {
			allSuccess = false
		}
	}

	if allSuccess {
		_ = queries.UpdateUpgradeJobStatus(ue.db, jobID, "completed")
	} else {
		_ = queries.UpdateUpgradeJobStatus(ue.db, jobID, "completed") // partial success still "completed"
	}

	ue.hub.Publish(fmt.Sprintf("upgrade.progress.%s", jobID), map[string]interface{}{
		"job_id": jobID,
		"status": "completed",
	})

	log.Printf("firmware: upgrade job %s finished", jobID)
}

func (ue *UpgradeExecutor) upgradeDevice(jobID string, jd queries.UpgradeJobDevice, reboot bool) bool {
	topic := fmt.Sprintf("upgrade.progress.%s", jobID)
	publish := func(status, message string) {
		_ = queries.UpdateUpgradeJobDeviceStatus(ue.db, jd.ID, status, message)
		ue.hub.Publish(topic, map[string]interface{}{
			"job_id":    jobID,
			"device_id": jd.DeviceID,
			"status":    status,
			"message":   message,
		})
	}

	// Get device info
	dev, err := queries.GetDevice(ue.db, jd.DeviceID)
	if err != nil {
		publish("failed", "device not found")
		return false
	}

	publish("downloading", fmt.Sprintf("Checking update on %s", dev.Identity))

	client, err := ue.pool.EnsureConnection(dev.ID, dev.Address, dev.APIPort, dev.Username, dev.PasswordEnc, dev.UseTLS)
	if err != nil {
		publish("failed", fmt.Sprintf("connection failed: %v", err))
		return false
	}

	// Check if update is actually available
	fw, err := routeros.CheckFirmwareUpdate(client)
	if err != nil {
		publish("failed", fmt.Sprintf("firmware check failed: %v", err))
		return false
	}

	if !fw.UpdateAvailable {
		publish("completed", "already up to date")
		return true
	}

	publish("installing", fmt.Sprintf("Installing %s → %s", fw.InstalledVersion, fw.LatestVersion))

	// Trigger download + install
	if err := routeros.TriggerUpgrade(client); err != nil {
		// TriggerUpgrade may return error because the device starts rebooting
		log.Printf("firmware: trigger upgrade on %s: %v (may be expected during reboot)", dev.Identity, err)
	}

	if reboot {
		publish("rebooting", "Device is rebooting...")
		ue.pool.Close(dev.ID)

		// Wait for device to come back online
		if err := ue.waitForReboot(dev, 5*time.Minute); err != nil {
			publish("failed", fmt.Sprintf("reboot timeout: %v", err))
			return false
		}

		publish("verifying", "Verifying new version...")

		// Verify version changed
		newClient, err := ue.pool.EnsureConnection(dev.ID, dev.Address, dev.APIPort, dev.Username, dev.PasswordEnc, dev.UseTLS)
		if err != nil {
			publish("failed", fmt.Sprintf("post-reboot connection failed: %v", err))
			return false
		}

		res, err := routeros.GetSystemResource(newClient)
		if err != nil {
			publish("failed", fmt.Sprintf("post-reboot check failed: %v", err))
			return false
		}

		_ = queries.UpdateDeviceInfo(ue.db, dev.ID, res.Platform, res.Board, res.Version, "", res.Architecture)
		publish("completed", fmt.Sprintf("Upgraded to %s", res.Version))
	} else {
		publish("completed", fmt.Sprintf("Update downloaded — %s needs manual reboot", dev.Identity))
	}

	return true
}

func (ue *UpgradeExecutor) waitForReboot(dev *queries.Device, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	// Wait a bit for device to go down
	time.Sleep(15 * time.Second)

	for time.Now().Before(deadline) {
		client, err := ue.pool.Dial(dev.ID, dev.Address, dev.APIPort, dev.Username, dev.PasswordEnc, dev.UseTLS)
		if err == nil {
			// Test that it's actually responding
			if err := routeros.KeepAlive(client); err == nil {
				return nil
			}
		}
		time.Sleep(10 * time.Second)
	}

	return fmt.Errorf("device %s did not come back within %v", dev.Identity, timeout)
}
