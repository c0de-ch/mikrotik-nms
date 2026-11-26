package routeros

import (
	"errors"
	"time"

	ros "github.com/go-routeros/routeros/v3"
)

// FirmwareInfo is what /system/package/update/print reports. The scheduled
// pollers only ever read Channel and InstalledVersion (ReadPackageUpdate):
// availability is computed centrally from MikroTik's release feed
// (internal/firmware), never by making a device run check-for-updates.
type FirmwareInfo struct {
	Channel          string
	InstalledVersion string
	LatestVersion    string
	Status           string
	UpdateAvailable  bool
}

type RouterboardInfo struct {
	CurrentFirmware string
	UpgradeFirmware string
}

// ReadPackageUpdate is the read-only inventory read of the update channel and
// the installed version: one proplisted print, no check-for-updates.
func ReadPackageUpdate(run Runner) (*FirmwareInfo, error) {
	reply, err := run.Run("/system/package/update/print", "=.proplist=channel,installed-version")
	if err != nil {
		return nil, err
	}
	if reply == nil || len(reply.Re) == 0 {
		return &FirmwareInfo{}, nil
	}
	m := reply.Re[0].Map
	return &FirmwareInfo{Channel: m["channel"], InstalledVersion: m["installed-version"]}, nil
}

// ErrNoRouterboard is ReadRouterboard's answer on a device without
// RouterBOARD firmware (CHR, x86): the menu does not exist there.
var ErrNoRouterboard = errors.New("no routerboard firmware on this device")

// ReadRouterboard is the read-only print of the RouterBOARD firmware
// versions. A device without the menu answers ErrNoRouterboard.
func ReadRouterboard(run Runner) (*RouterboardInfo, error) {
	reply, err := run.Run("/system/routerboard/print", "=.proplist=current-firmware,upgrade-firmware")
	if err != nil {
		if IsNoSuchCommand(err) {
			return nil, ErrNoRouterboard
		}
		return nil, err
	}
	if reply == nil || len(reply.Re) == 0 {
		return nil, ErrNoRouterboard
	}
	m := reply.Re[0].Map
	if m["current-firmware"] == "" && m["upgrade-firmware"] == "" {
		return nil, ErrNoRouterboard
	}
	return &RouterboardInfo{CurrentFirmware: m["current-firmware"], UpgradeFirmware: m["upgrade-firmware"]}, nil
}

// UpdateCheckPoll is how long CheckForUpdates waits between prints while the
// device fetches its update information (a variable for tests).
var UpdateCheckPoll = 2 * time.Second

// CheckForUpdates makes the device ask MikroTik's servers for the newest
// version of its channel and returns the result. It is not read-only, so it
// runs only inside an admin's firmware job, on that job's one-shot session
// and credentials — never with the poll credential.
func CheckForUpdates(run Runner) (*FirmwareInfo, error) {
	// Some versions answer the async command with an error while the check
	// still runs; the prints below tell what the device found.
	_, _ = run.Run("/system/package/update/check-for-updates")
	var info *FirmwareInfo
	for range 5 {
		time.Sleep(UpdateCheckPoll)
		reply, err := run.Run("/system/package/update/print",
			"=.proplist=channel,installed-version,latest-version,status")
		if err != nil {
			return nil, err
		}
		if reply == nil || len(reply.Re) == 0 {
			continue
		}
		m := reply.Re[0].Map
		info = &FirmwareInfo{
			Channel:          m["channel"],
			InstalledVersion: m["installed-version"],
			LatestVersion:    m["latest-version"],
			Status:           m["status"],
			UpdateAvailable:  m["status"] == "New version is available",
		}
		if info.LatestVersion != "" {
			return info, nil
		}
		if m["status"] == "checking..." || m["status"] == "" {
			continue
		}
		break
	}
	if info == nil {
		return &FirmwareInfo{}, nil
	}
	return info, nil
}

// InstallUpdate downloads and installs the update. RouterOS reboots the
// device to install it, so the session usually ends with an error.
func InstallUpdate(run Runner) error {
	_, err := run.Run("/system/package/update/install")
	return err
}

// DownloadUpdate downloads the update without rebooting: RouterOS installs
// it on the next reboot, whoever triggers it.
func DownloadUpdate(run Runner) error {
	_, err := run.Run("/system/package/update/download")
	return err
}

// SetChannel changes the update channel (stable, long-term, testing, development).
func SetChannel(run Runner, channel string) error {
	_, err := run.Run("/system/package/update/set", "=channel="+channel)
	return err
}

// UpgradeRouterboard upgrades the RouterBoard firmware to match the installed
// RouterOS version (applied on the next reboot).
func UpgradeRouterboard(run Runner) error {
	_, err := run.Run("/system/routerboard/upgrade")
	return err
}

// Reboot reboots the device.
func Reboot(run Runner) error {
	_, err := run.Run("/system/reboot")
	return err
}

// OnceRunner runs commands on a dedicated one-shot client (DialOnce) with its
// own timeout: OnceCommandTimeout unless Timeout says otherwise (firmware
// downloads and installs take minutes).
type OnceRunner struct {
	Client  *ros.Client
	Timeout time.Duration
}

// Run executes the command with the runner's timeout.
func (o OnceRunner) Run(command string, args ...string) (*ros.Reply, error) {
	t := o.Timeout
	if t <= 0 {
		t = OnceCommandTimeout
	}
	return RunCommandWithTimeout(o.Client, t, command, args...)
}
