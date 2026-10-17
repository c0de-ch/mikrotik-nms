package routeros

import (
	"fmt"
	"strconv"

	ros "github.com/go-routeros/routeros/v3"
)

// ARPEntry represents an entry from /ip/arp.
type ARPEntry struct {
	Address   string `json:"address"`
	MAC       string `json:"mac_address"`
	Interface string `json:"interface"`
	Dynamic   bool   `json:"dynamic"`
	Complete  bool   `json:"complete"`
}

func GetARPTable(run Runner) ([]ARPEntry, error) {
	reply, err := run.Run("/ip/arp/print")
	if err != nil {
		return nil, err
	}
	var entries []ARPEntry
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		entries = append(entries, ARPEntry{
			Address:   m["address"],
			MAC:       m["mac-address"],
			Interface: m["interface"],
			Dynamic:   m["dynamic"] == "true",
			Complete:  m["complete"] == "true",
		})
	}
	return entries, nil
}

// DHCPLease represents a DHCP server lease.
type DHCPLease struct {
	Address    string `json:"address"`
	MAC        string `json:"mac_address"`
	HostName   string `json:"host_name"`
	Server     string `json:"server"`
	Status     string `json:"status"`
	ActiveAddr string `json:"active_address"`
	ActiveMAC  string `json:"active_mac"`
	Comment    string `json:"comment"`
	Dynamic    bool   `json:"dynamic"`
}

func GetDHCPLeases(run Runner) ([]DHCPLease, error) {
	reply, err := run.Run("/ip/dhcp-server/lease/print")
	if err != nil {
		return nil, err
	}
	var leases []DHCPLease
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		leases = append(leases, DHCPLease{
			Address:    m["address"],
			MAC:        m["mac-address"],
			HostName:   m["host-name"],
			Server:     m["server"],
			Status:     m["status"],
			ActiveAddr: m["active-address"],
			ActiveMAC:  m["active-mac-address"],
			Comment:    m["comment"],
			Dynamic:    m["dynamic"] == "true",
		})
	}
	// A disabled DHCP server keeps its lease/reservation rows in the table, but
	// those aren't live assignments — the server isn't handing them out. Drop
	// them so a switch whose dhcp server is turned off doesn't surface a ghost
	// "client" for every leftover static reservation.
	return activeLeases(leases, disabledDHCPServers(run)), nil
}

// disabledDHCPServers returns the set of administratively-disabled DHCP server
// names. Best-effort: any error yields an empty set, so leases are shown rather
// than wrongly hidden on a transient query failure (fail-open).
func disabledDHCPServers(run Runner) map[string]bool {
	reply, err := run.Run("/ip/dhcp-server/print")
	if err != nil {
		return nil
	}
	out := make(map[string]bool)
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		if m["disabled"] == "true" {
			if name := m["name"]; name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// activeLeases drops leases that belong to a disabled DHCP server. Pure, for
// testing. A lease whose server is empty/unknown is kept (we only hide leases we
// can positively attribute to a disabled server).
func activeLeases(leases []DHCPLease, disabledServers map[string]bool) []DHCPLease {
	if len(disabledServers) == 0 {
		return leases
	}
	out := make([]DHCPLease, 0, len(leases))
	for _, l := range leases {
		if disabledServers[l.Server] {
			continue
		}
		out = append(out, l)
	}
	return out
}

// WifiRegistration represents a wireless client registration (CAPsMAN or wifi).
type WifiRegistration struct {
	Interface  string `json:"interface"`
	MAC        string `json:"mac_address"`
	AP         string `json:"ap"`
	SSID       string `json:"ssid"`
	Band       string `json:"band"`
	Channel    string `json:"channel"`
	Frequency  string `json:"frequency"`
	Signal     string `json:"signal"`
	TxRate     string `json:"tx_rate"`
	RxRate     string `json:"rx_rate"`
	Uptime     string `json:"uptime"`
	Bytes      string `json:"bytes"`
	PacketRate string `json:"packet_rate"`
	LastIP     string `json:"last_ip"`
	Source     string `json:"source"` // "wifi-v2", "capsman", "wireless"
}

// Registration-table menus a device may have, in the order a device that has
// several is read. Which ones exist is probed once (poller.CapabilityCache);
// printing a menu whose package is missing costs the device a trap.
const (
	RegMenuWifi     = "wifi"     // /interface/wifi (RouterOS 7 wifi, CAPsMAN v2)
	RegMenuCAPsMAN  = "capsman"  // /caps-man (legacy CAPsMAN controller)
	RegMenuWireless = "wireless" // /interface/wireless (legacy local wireless)
)

// Menu probe commands, for the capability cache.
const (
	WifiProbeCommand       = "/interface/wifi/print"
	CAPsMANProbeCommand    = "/caps-man/interface/print"
	WirelessProbeCommand   = "/interface/wireless/print"
	DHCPServerProbeCommand = "/ip/dhcp-server/print"
	BridgeProbeCommand     = "/interface/bridge/print"
)

// GetRegistrations reads one registration-table menu (RegMenu*). Callers know
// from the capability probe which menus the device has, so this never sends
// a print that will trap.
func GetRegistrations(run Runner, menu string) ([]WifiRegistration, error) {
	switch menu {
	case RegMenuWifi:
		return getWifiRegistrations(run)
	case RegMenuCAPsMAN:
		return getLegacyCAPsMANRegistrations(run)
	case RegMenuWireless:
		return getLocalWirelessRegistrations(run)
	}
	return nil, fmt.Errorf("unknown registration menu %q", menu)
}

// GetCAPsMANRegistrations gets wireless clients without knowing which menus
// the device has: it tries wifi, then legacy CAPsMAN, then local wireless,
// and a missing package costs a trap per attempt. On-demand callers only
// (the Clients page); the pollers read the menus the capability probe found.
func GetCAPsMANRegistrations(client *ros.Client) ([]WifiRegistration, error) {
	run := ClientRunner{Client: client}
	// Try RouterOS v7 wifi first
	regs, err := getWifiRegistrations(run)
	if err == nil && len(regs) > 0 {
		return regs, nil
	}
	// Fallback to legacy CAPsMAN
	regs, err = getLegacyCAPsMANRegistrations(run)
	if err == nil {
		return regs, nil
	}
	// Fallback to local wireless registration-table
	return getLocalWirelessRegistrations(run)
}

func getWifiRegistrations(run Runner) ([]WifiRegistration, error) {
	reply, err := run.Run("/interface/wifi/registration-table/print")
	if err != nil {
		return nil, err
	}
	var regs []WifiRegistration
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		// AP field: try "ap", "ap-name", "radio-name"; interface often contains the CAP radio name
		ap := m["ap"]
		if ap == "" {
			ap = m["ap-name"]
		}
		if ap == "" {
			ap = m["radio-name"]
		}
		regs = append(regs, WifiRegistration{
			Interface: m["interface"],
			MAC:       m["mac-address"],
			AP:        ap,
			SSID:      m["ssid"],
			Band:      m["band"],
			Channel:   m["channel"],
			Frequency: m["frequency"],
			Signal:    formatSignal(m["signal"]),
			TxRate:    m["tx-rate"],
			RxRate:    m["rx-rate"],
			LastIP:    m["last-ip"],
			Source:    "wifi-v2",
			Uptime:    m["uptime"],
			Bytes:     m["bytes"],
		})
	}
	return regs, nil
}

func getLegacyCAPsMANRegistrations(run Runner) ([]WifiRegistration, error) {
	reply, err := run.Run("/caps-man/registration-table/print")
	if err != nil {
		return nil, err
	}
	var regs []WifiRegistration
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		regs = append(regs, WifiRegistration{
			Interface: m["interface"],
			MAC:       m["mac-address"],
			AP:        m["ap"],
			SSID:      m["ssid"],
			Band:      m["band"],
			Signal:    formatSignal(m["rx-signal"]),
			TxRate:    m["tx-rate"],
			RxRate:    m["rx-rate"],
			LastIP:    m["last-ip"],
			Uptime:    m["uptime"],
			Bytes:     m["bytes"],
			Source:    "capsman",
		})
	}
	return regs, nil
}

func getLocalWirelessRegistrations(run Runner) ([]WifiRegistration, error) {
	reply, err := run.Run("/interface/wireless/registration-table/print")
	if err != nil {
		return nil, err
	}
	var regs []WifiRegistration
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		regs = append(regs, WifiRegistration{
			Interface: m["interface"],
			MAC:       m["mac-address"],
			Signal:    formatSignal(m["signal-strength"]),
			TxRate:    m["tx-rate"],
			RxRate:    m["rx-rate"],
			Uptime:    m["uptime"],
			Bytes:     m["bytes"],
			Source:    "wireless",
		})
	}
	return regs, nil
}

func formatSignal(s string) string {
	if s == "" {
		return ""
	}
	// Try to parse as int for dBm display
	if v, err := strconv.Atoi(s); err == nil {
		return strconv.Itoa(v) + " dBm"
	}
	return s
}
