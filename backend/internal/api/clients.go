package api

import (
	"log"
	"net/http"
	"strings"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

type networkClient struct {
	MAC       string `json:"mac_address"`
	IP        string `json:"ip_address"`
	HostName  string `json:"host_name"`
	Interface string `json:"interface"`
	Source    string `json:"source"`      // "arp", "dhcp", "wifi"
	DeviceID  string `json:"device_id"`   // which managed device reported this
	DeviceName string `json:"device_name"`
	// Wifi-specific fields
	AP        string `json:"ap,omitempty"`
	SSID      string `json:"ssid,omitempty"`
	Band      string `json:"band,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Frequency string `json:"frequency,omitempty"`
	Signal    string `json:"signal,omitempty"`
	TxRate    string `json:"tx_rate,omitempty"`
	RxRate    string `json:"rx_rate,omitempty"`
	Uptime    string `json:"uptime,omitempty"`
}

func (s *Server) handleScanClients(w http.ResponseWriter, r *http.Request) {
	devices, err := queries.ListDevices(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list devices")
		return
	}

	// Collect all clients, deduplicate by MAC
	clientMap := make(map[string]*networkClient) // key: uppercase MAC

	for _, dev := range devices {
		if dev.Status != "online" {
			continue
		}
		client := s.pool.Get(dev.ID)
		if client == nil {
			continue
		}
		devName := dev.Identity
		if devName == "" {
			devName = dev.Address
		}

		// ARP table
		func() {
			defer func() { recover() }()
			arps, err := routeros.GetARPTable(client)
			if err != nil {
				log.Printf("scan clients: arp %s: %v", devName, err)
				return
			}
			for _, a := range arps {
				if a.MAC == "" {
					continue
				}
				mac := strings.ToUpper(a.MAC)
				if existing, ok := clientMap[mac]; ok {
					if existing.IP == "" && a.Address != "" {
						existing.IP = a.Address
					}
				} else {
					clientMap[mac] = &networkClient{
						MAC:        a.MAC,
						IP:         a.Address,
						Interface:  a.Interface,
						Source:     "arp",
						DeviceID:   dev.ID,
						DeviceName: devName,
					}
				}
			}
		}()

		// DHCP leases
		func() {
			defer func() { recover() }()
			leases, err := routeros.GetDHCPLeases(client)
			if err != nil {
				log.Printf("scan clients: dhcp %s: %v", devName, err)
				return
			}
			for _, l := range leases {
				mac := strings.ToUpper(l.MAC)
				if mac == "" {
					mac = strings.ToUpper(l.ActiveMAC)
				}
				if mac == "" {
					continue
				}
				addr := l.ActiveAddr
				if addr == "" {
					addr = l.Address
				}
				if existing, ok := clientMap[mac]; ok {
					if existing.HostName == "" && l.HostName != "" {
						existing.HostName = l.HostName
					}
					if existing.IP == "" && addr != "" {
						existing.IP = addr
					}
					if l.HostName != "" {
						existing.Source = "dhcp"
					}
				} else {
					clientMap[mac] = &networkClient{
						MAC:        mac,
						IP:         addr,
						HostName:   l.HostName,
						Interface:  l.Server,
						Source:     "dhcp",
						DeviceID:   dev.ID,
						DeviceName: devName,
					}
				}
			}
		}()

		// WiFi / CAPsMAN registrations
		func() {
			defer func() { recover() }()
			regs, err := routeros.GetCAPsMANRegistrations(client)
			if err != nil {
				return // not all devices have wifi
			}
			for _, reg := range regs {
				mac := strings.ToUpper(reg.MAC)
				if mac == "" {
					continue
				}
				if existing, ok := clientMap[mac]; ok {
					// Enrich with wifi data
					existing.AP = reg.AP
					existing.SSID = reg.SSID
					existing.Band = reg.Band
					existing.Channel = reg.Channel
					existing.Frequency = reg.Frequency
					existing.Signal = reg.Signal
					existing.TxRate = reg.TxRate
					existing.RxRate = reg.RxRate
					existing.Uptime = reg.Uptime
					existing.Source = "wifi"
				} else {
					clientMap[mac] = &networkClient{
						MAC:        mac,
						Interface:  reg.Interface,
						Source:     "wifi",
						DeviceID:   dev.ID,
						DeviceName: devName,
						AP:         reg.AP,
						SSID:       reg.SSID,
						Band:       reg.Band,
						Channel:    reg.Channel,
						Frequency:  reg.Frequency,
						Signal:     reg.Signal,
						TxRate:     reg.TxRate,
						RxRate:     reg.RxRate,
						Uptime:     reg.Uptime,
					}
				}
			}
		}()
	}

	results := make([]networkClient, 0, len(clientMap))
	for _, c := range clientMap {
		results = append(results, *c)
	}

	writeJSON(w, http.StatusOK, results)
}
