package routeros

import (
	"strconv"

	ros "github.com/go-routeros/routeros"
)

// ARPEntry represents an entry from /ip/arp.
type ARPEntry struct {
	Address   string `json:"address"`
	MAC       string `json:"mac_address"`
	Interface string `json:"interface"`
	Dynamic   bool   `json:"dynamic"`
	Complete  bool   `json:"complete"`
}

func GetARPTable(client *ros.Client) ([]ARPEntry, error) {
	reply, err := RunCommand(client, "/ip/arp/print")
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

func GetDHCPLeases(client *ros.Client) ([]DHCPLease, error) {
	reply, err := RunCommand(client, "/ip/dhcp-server/lease/print")
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
	return leases, nil
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
}

// GetCAPsMANRegistrations gets wireless clients from CAPsMAN (RouterOS v7 /interface/wifi).
func GetCAPsMANRegistrations(client *ros.Client) ([]WifiRegistration, error) {
	// Try RouterOS v7 wifi first
	regs, err := getWifiRegistrations(client)
	if err == nil && len(regs) > 0 {
		return regs, nil
	}
	// Fallback to legacy CAPsMAN
	regs, err = getLegacyCAPsMANRegistrations(client)
	if err == nil {
		return regs, nil
	}
	// Fallback to local wireless registration-table
	return getLocalWirelessRegistrations(client)
}

func getWifiRegistrations(client *ros.Client) ([]WifiRegistration, error) {
	reply, err := RunCommand(client, "/interface/wifi/registration-table/print")
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
			Uptime:    m["uptime"],
			Bytes:     m["bytes"],
		})
	}
	return regs, nil
}

func getLegacyCAPsMANRegistrations(client *ros.Client) ([]WifiRegistration, error) {
	reply, err := RunCommand(client, "/caps-man/registration-table/print")
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
			Uptime:    m["uptime"],
			Bytes:     m["bytes"],
		})
	}
	return regs, nil
}

func getLocalWirelessRegistrations(client *ros.Client) ([]WifiRegistration, error) {
	reply, err := RunCommand(client, "/interface/wireless/registration-table/print")
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
