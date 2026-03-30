package api

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

type networkClient struct {
	MAC       string `json:"mac_address"`
	IP        string `json:"ip_address"`
	HostName  string `json:"host_name"`
	DNSName   string `json:"dns_name"`
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

// handleDebugWifiRaw returns raw fields from the wifi registration table for debugging.
func (s *Server) handleDebugWifiRaw(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "device_id required")
		return
	}
	client := s.pool.Get(deviceID)
	if client == nil {
		writeError(w, http.StatusNotFound, "device not connected")
		return
	}

	results := make(map[string]interface{})

	// Try all three sources
	for _, cmd := range []string{
		"/interface/wifi/registration-table/print",
		"/caps-man/registration-table/print",
		"/interface/wireless/registration-table/print",
	} {
		func() {
			defer func() { recover() }()
			raw, err := routeros.RawCommand(client, cmd)
			if err != nil {
				results[cmd] = map[string]string{"error": err.Error()}
				return
			}
			results[cmd] = raw
		}()
	}

	writeJSON(w, http.StatusOK, results)
}

func (s *Server) handleScanClients(w http.ResponseWriter, r *http.Request) {
	// Parse query params
	limitParam := r.URL.Query().Get("limit")
	maxClients := 0 // 0 = unlimited
	if limitParam != "" {
		if v, err := strconv.Atoi(limitParam); err == nil && v > 0 {
			maxClients = v
		}
	}

	timeoutParam := r.URL.Query().Get("timeout")
	scanTimeout := 30 * time.Second // default 30s
	if timeoutParam != "" {
		if v, err := strconv.Atoi(timeoutParam); err == nil && v >= 5 && v <= 120 {
			scanTimeout = time.Duration(v) * time.Second
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), scanTimeout)
	defer cancel()

	devices, err := queries.ListDevices(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list devices")
		return
	}

	// Collect all clients, deduplicate by MAC
	clientMap := make(map[string]*networkClient) // key: uppercase MAC

	for _, dev := range devices {
		// Check timeout
		select {
		case <-ctx.Done():
			goto done
		default:
		}

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
				// Resolve AP name: use AP field, or extract from interface name
				apName := reg.AP
				if apName == "" && reg.Interface != "" {
					// Interface is often "capName/radioName" or just the radio identity
					apName = reg.Interface
					if idx := strings.Index(apName, "/"); idx > 0 {
						apName = apName[:idx]
					}
				}
				if existing, ok := clientMap[mac]; ok {
					// Enrich with wifi data
					existing.AP = apName
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
						AP:         apName,
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

done:
	// Resolve DNS names for all IPs
	ips := make([]string, 0, len(clientMap))
	for _, c := range clientMap {
		if c.IP != "" {
			ips = append(ips, c.IP)
		}
	}
	dnsNames := s.resolver.ResolveMany(ips)

	results := make([]networkClient, 0, len(clientMap))
	for _, c := range clientMap {
		if name, ok := dnsNames[c.IP]; ok && name != "" {
			c.DNSName = name
			// If no hostname from DHCP, use DNS name
			if c.HostName == "" {
				c.HostName = name
			}
		}
		results = append(results, *c)
	}

	// Apply limit
	if maxClients > 0 && len(results) > maxClients {
		results = results[:maxClients]
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"clients":   results,
		"total":     len(clientMap),
		"limited":   maxClients > 0 && len(clientMap) > maxClients,
		"timed_out": ctx.Err() != nil,
	})
}
