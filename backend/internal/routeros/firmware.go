package routeros

import (
	ros "github.com/go-routeros/routeros"
)

type FirmwareInfo struct {
	Channel          string
	InstalledVersion string
	LatestVersion    string
	UpdateAvailable  bool
}

type RouterboardInfo struct {
	CurrentFirmware string
	UpgradeFirmware string
}

func CheckFirmwareUpdate(client *ros.Client) (*FirmwareInfo, error) {
	// First trigger a check
	_, _ = RunCommand(client, "/system/package/update/check-for-updates")

	reply, err := RunCommand(client, "/system/package/update/print")
	if err != nil {
		return nil, err
	}

	if len(reply.Re) == 0 {
		return &FirmwareInfo{}, nil
	}

	m := GetSentenceMap(reply.Re[0])
	return &FirmwareInfo{
		Channel:          m["channel"],
		InstalledVersion: m["installed-version"],
		LatestVersion:    m["latest-version"],
		UpdateAvailable:  m["status"] == "New version is available",
	}, nil
}

func GetRouterboardInfo(client *ros.Client) (*RouterboardInfo, error) {
	reply, err := RunCommand(client, "/system/routerboard/print")
	if err != nil {
		return nil, err
	}

	if len(reply.Re) == 0 {
		return &RouterboardInfo{}, nil
	}

	m := GetSentenceMap(reply.Re[0])
	return &RouterboardInfo{
		CurrentFirmware: m["current-firmware"],
		UpgradeFirmware: m["upgrade-firmware"],
	}, nil
}

func TriggerUpgrade(client *ros.Client) error {
	_, err := RunCommand(client, "/system/package/update/install")
	return err
}

func TriggerReboot(client *ros.Client) error {
	_, err := RunCommand(client, "/system/reboot")
	return err
}
