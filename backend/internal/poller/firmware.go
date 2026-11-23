package poller

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/firmware"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// Timings of an upgrade job (variables for tests).
var (
	// upgradeConnectWindow is how long a job keeps retrying its first dial
	// of a device (it may still be rebooting from a RouterBOARD upgrade).
	upgradeConnectWindow = 3 * time.Minute
	// upgradeRetryEvery is the gap between dial attempts.
	upgradeRetryEvery = 10 * time.Second
	// rebootGrace is how long the job waits before looking for a rebooting
	// device, rebootTimeout how long it may take to come back.
	rebootGrace   = 15 * time.Second
	rebootTimeout = 5 * time.Minute
	// packageCommandTimeout bounds download and install: both fetch the
	// packages from MikroTik's servers before they answer.
	packageCommandTimeout = 10 * time.Minute
)

// UpgradeExecutor runs a firmware upgrade job asynchronously. Every device
// session of the job is a one-shot connection (routeros.DialOnceWith) logged
// in with the credentials the admin typed for the job — never the pooled
// poll session and never the stored poll credential — and the credentials
// are zeroed when the job ends.
type UpgradeExecutor struct {
	db        *sql.DB
	pool      *routeros.Pool
	hub       *ws.Hub
	creds     *routeros.Credentials
	verifyTLS bool
	// refusedOn is the first device that refused the job's login: the
	// remaining devices are skipped rather than sent the same wrong
	// password (each failure is a line in their log, and may lock out).
	refusedOn string
}

// NewUpgradeExecutor takes ownership of creds: Execute zeroes them.
func NewUpgradeExecutor(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, creds *routeros.Credentials, verifyTLS bool) *UpgradeExecutor {
	return &UpgradeExecutor{db: db, pool: pool, hub: hub, creds: creds, verifyTLS: verifyTLS}
}

// Execute runs an upgrade job: upgrades each device sequentially, broadcasting progress.
func (ue *UpgradeExecutor) Execute(jobID string) {
	defer ue.creds.Zero()
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

	for _, jd := range devices {
		if ue.refusedOn != "" {
			ue.skipDevice(jobID, jd)
			continue
		}
		ue.upgradeDevice(jobID, jd, reboot)
	}
	// Partial success is still "completed"; each device row has its outcome.
	_ = queries.UpdateUpgradeJobStatus(ue.db, jobID, "completed")

	ue.hub.Publish(fmt.Sprintf("upgrade.progress.%s", jobID), map[string]interface{}{
		"job_id": jobID,
		"status": "completed",
	})

	log.Printf("firmware: upgrade job %s finished", jobID)
}

// LoginRefusedMessage is the per-device outcome of a job whose credentials
// the device turned down.
func LoginRefusedMessage(identity string) string {
	return fmt.Sprintf("login refused by %s — check the username and password typed for this job", identity)
}

func (ue *UpgradeExecutor) skipDevice(jobID string, jd queries.UpgradeJobDevice) {
	msg := "skipped: " + LoginRefusedMessage(ue.refusedOn)
	_ = queries.UpdateUpgradeJobDeviceStatus(ue.db, jd.ID, "failed", msg)
	ue.hub.Publish(fmt.Sprintf("upgrade.progress.%s", jobID), map[string]interface{}{
		"job_id": jobID, "device_id": jd.DeviceID, "status": "failed", "message": msg,
	})
}

// dial opens a one-shot session to dev with the job's credentials.
func (ue *UpgradeExecutor) dial(dev *queries.Device) (*ros.Client, error) {
	return routeros.DialOnceWith(dev.Address, dev.APIPort, ue.creds, dev.UseTLS, ue.verifyTLS)
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

	dev, err := queries.GetDevice(ue.db, jd.DeviceID)
	if err != nil {
		publish("failed", "device not found")
		return false
	}

	publish("downloading", fmt.Sprintf("Checking update on %s", dev.Identity))

	// The device can be briefly unreachable when the job starts — e.g. still
	// rebooting from a RouterBOARD firmware upgrade applied moments earlier.
	// Retry the first connection for a bounded window; a refused login is
	// final (retrying a wrong password only fills the device's log).
	client, err := ue.dial(dev)
	for deadline := time.Now().Add(upgradeConnectWindow); err != nil && !routeros.IsLoginRefused(err) &&
		!errors.Is(err, routeros.ErrTargetNotAllowed) && !errors.Is(err, routeros.ErrPollersOff) &&
		time.Now().Before(deadline); {
		publish("downloading", fmt.Sprintf("Waiting for %s to respond...", dev.Identity))
		time.Sleep(upgradeRetryEvery)
		client, err = ue.dial(dev)
	}
	if err != nil {
		if routeros.IsLoginRefused(err) {
			ue.refusedOn = dev.Identity
			publish("failed", LoginRefusedMessage(dev.Identity))
		} else {
			publish("failed", fmt.Sprintf("connection failed: %v", err))
		}
		return false
	}
	closed := false
	closeClient := func() {
		if !closed {
			closed = true
			_ = client.Close()
		}
	}
	defer closeClient()

	// check-for-updates is not read-only; it runs here, inside the admin's
	// job and on its session, never with the poll credential.
	fw, err := routeros.CheckForUpdates(routeros.OnceRunner{Client: client})
	if err != nil {
		publish("failed", fmt.Sprintf("update check failed: %v", err))
		return false
	}
	if fw.LatestVersion == "" {
		msg := "the device could not check for updates"
		if fw.Status != "" {
			msg += ": " + fw.Status
		}
		publish("failed", msg)
		return false
	}
	if !fw.UpdateAvailable {
		publish("completed", fmt.Sprintf("already up to date (%s)", fw.InstalledVersion))
		return true
	}

	pkgRun := routeros.OnceRunner{Client: client, Timeout: packageCommandTimeout}
	if !reboot {
		// download fetches the packages and stops there: RouterOS installs
		// them on the next reboot. install would reboot the device now.
		publish("downloading", fmt.Sprintf("Downloading %s → %s", fw.InstalledVersion, fw.LatestVersion))
		if err := routeros.DownloadUpdate(pkgRun); err != nil {
			publish("failed", fmt.Sprintf("download failed: %v", err))
			return false
		}
		publish("completed", fmt.Sprintf("%s downloaded — %s installs it on its next reboot", fw.LatestVersion, dev.Identity))
		return true
	}

	publish("installing", fmt.Sprintf("Installing %s → %s", fw.InstalledVersion, fw.LatestVersion))
	if err := routeros.InstallUpdate(pkgRun); err != nil {
		// The device reboots to install, which usually ends the session.
		log.Printf("firmware: install on %s: %v (expected when the device reboots)", dev.Identity, err)
	}
	closeClient()

	publish("rebooting", "Device is rebooting...")
	ue.pool.Close(dev.ID) // the pooled poll session died with the reboot

	if err := ue.waitForReboot(dev); err != nil {
		if routeros.IsLoginRefused(err) {
			publish("failed", LoginRefusedMessage(dev.Identity))
		} else {
			publish("failed", fmt.Sprintf("reboot timeout: %v", err))
		}
		return false
	}

	publish("verifying", "Verifying new version...")
	verify, err := ue.dial(dev)
	if err != nil {
		publish("failed", fmt.Sprintf("post-reboot connection failed: %v", err))
		return false
	}
	defer verify.Close()
	res, err := routeros.GetSystemResource(verify)
	if err != nil {
		publish("failed", fmt.Sprintf("post-reboot check failed: %v", err))
		return false
	}
	_ = queries.UpdateDeviceInfo(ue.db, dev.ID, res.Platform, res.Board, res.Version, "", res.Architecture)
	if firmware.UpdateAvailable(res.Version, fw.LatestVersion) {
		publish("failed", fmt.Sprintf("%s came back on %s, expected %s", dev.Identity, res.Version, fw.LatestVersion))
		return false
	}
	publish("completed", fmt.Sprintf("Upgraded to %s", res.Version))
	return true
}

// waitForReboot waits for dev to answer a login again after a reboot.
func (ue *UpgradeExecutor) waitForReboot(dev *queries.Device) error {
	time.Sleep(rebootGrace)
	deadline := time.Now().Add(rebootTimeout)
	for {
		client, err := ue.dial(dev)
		if err == nil {
			_ = client.Close()
			return nil
		}
		if routeros.IsLoginRefused(err) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s did not come back within %v", dev.Identity, rebootTimeout)
		}
		time.Sleep(upgradeRetryEvery)
	}
}
