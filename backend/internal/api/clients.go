package api

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/leasesource"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

type networkClient struct {
	MAC        string `json:"mac_address"`
	IP         string `json:"ip_address"`
	HostName   string `json:"host_name"`
	DNSName    string `json:"dns_name"`
	Interface  string `json:"interface"`
	Source     string `json:"source"`    // "arp", "dhcp", "wifi"
	DeviceID   string `json:"device_id"` // which managed device reported this
	DeviceName string `json:"device_name"`
	// Freshness: Active is false once the cache entry hasn't been refreshed
	// within the inactivity window (e.g. a device that left, or a reservation on
	// a now-disabled DHCP server). LastSeen is the last refresh (RFC3339).
	Active   bool   `json:"active"`
	LastSeen string `json:"last_seen,omitempty"`
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
				// Skip non-wireless entries
				if reg.SSID == "" && reg.Band == "" && reg.Signal == "" {
					iface := strings.ToLower(reg.Interface)
					if strings.Contains(iface, "ether") || strings.Contains(iface, "bridge") ||
						strings.Contains(iface, "vlan") || strings.Contains(iface, "pppoe") {
						continue
					}
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
					if existing.IP == "" && reg.LastIP != "" {
						existing.IP = reg.LastIP
					}
				} else {
					clientMap[mac] = &networkClient{
						MAC:        mac,
						IP:         reg.LastIP,
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
	// External DHCP (Kea / OPNsense, per app_settings) — the same sources the
	// background discovery poller uses. Without this a live scan only sees
	// MikroTik-visible clients and drops OPNsense/Kea-only clients (and their
	// DHCP hostnames), so the auto-scan would wipe them from the cached view.
	for _, l := range leasesource.FromSettings(s.db) {
		if existing, ok := clientMap[l.MAC]; ok {
			if existing.IP == "" && l.IP != "" {
				existing.IP = l.IP
			}
			if existing.HostName == "" && l.Hostname != "" {
				existing.HostName = l.Hostname
			}
		} else {
			clientMap[l.MAC] = &networkClient{MAC: l.MAC, IP: l.IP, HostName: l.Hostname, Source: "dhcp", DeviceName: l.Origin}
		}
	}

	// Resolve DNS names for all IPs
	ips := make([]string, 0, len(clientMap))
	for _, c := range clientMap {
		if c.IP != "" {
			ips = append(ips, c.IP)
		}
	}
	dnsNames := s.resolver.ResolveMany(ips)

	// Everything returned by a live scan was just seen, so it's active by
	// definition (and will refresh the cache's updated_at via UpsertMACLookup).
	nowStr := time.Now().UTC().Format(time.RFC3339)
	results := make([]networkClient, 0, len(clientMap))
	for _, c := range clientMap {
		if name, ok := dnsNames[c.IP]; ok && name != "" {
			c.DNSName = name
			// If no hostname from DHCP, use DNS name
			if c.HostName == "" {
				c.HostName = name
			}
		}
		c.Active = true
		c.LastSeen = nowStr
		results = append(results, *c)
	}

	// Apply limit
	if maxClients > 0 && len(results) > maxClients {
		results = results[:maxClients]
	}

	// Persist all results to mac_lookup cache (async, don't block response)
	go func() {
		for _, c := range results {
			_ = queries.UpsertMACLookup(s.db, &queries.MACLookup{
				MACAddress:    c.MAC,
				IPAddress:     c.IP,
				HostName:      c.HostName,
				DNSName:       c.DNSName,
				Source:        c.Source,
				DeviceName:    c.DeviceName,
				InterfaceName: c.Interface,
				DeviceID:      c.DeviceID,
				AP:            c.AP,
				SSID:          c.SSID,
				Band:          c.Band,
				Channel:       c.Channel,
				Frequency:     c.Frequency,
				Signal:        c.Signal,
				TxRate:        c.TxRate,
				RxRate:        c.RxRate,
				Uptime:        c.Uptime,
			})
		}
	}()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"clients":   results,
		"total":     len(clientMap),
		"limited":   maxClients > 0 && len(clientMap) > maxClients,
		"timed_out": ctx.Err() != nil,
	})
}

// defaultClientInactiveAfter is how long a cached client may go without being
// re-seen before it's reported inactive, when the admin hasn't configured
// client_inactive_after_seconds. Picked well above the client-discovery interval
// so a live client never flickers, but far below the ~30-day cache retention so
// a device that left (or a reservation on a disabled DHCP server) drops off the
// active list promptly instead of lingering for weeks.
const defaultClientInactiveAfter = 30 * time.Minute

func (s *Server) clientInactiveAfter() time.Duration {
	v, err := queries.GetSetting(s.db, "client_inactive_after_seconds")
	if err != nil {
		return defaultClientInactiveAfter
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultClientInactiveAfter
	}
	return time.Duration(n) * time.Second
}

func (s *Server) handleCachedClients(w http.ResponseWriter, r *http.Request) {
	entries, err := queries.GetAllMACLookupsSlice(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load cached clients")
		return
	}

	threshold := s.clientInactiveAfter()
	now := time.Now()
	active := 0
	clients := make([]networkClient, 0, len(entries))
	for _, e := range entries {
		isActive := now.Sub(e.UpdatedAt) < threshold
		if isActive {
			active++
		}
		clients = append(clients, networkClient{
			MAC:        e.MACAddress,
			IP:         e.IPAddress,
			HostName:   e.HostName,
			DNSName:    e.DNSName,
			Interface:  e.InterfaceName,
			Source:     e.Source,
			DeviceID:   e.DeviceID,
			DeviceName: e.DeviceName,
			AP:         e.AP,
			SSID:       e.SSID,
			Band:       e.Band,
			Channel:    e.Channel,
			Frequency:  e.Frequency,
			Signal:     e.Signal,
			TxRate:     e.TxRate,
			RxRate:     e.RxRate,
			Uptime:     e.Uptime,
			Active:     isActive,
			LastSeen:   e.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"clients": clients,
		"total":   len(clients),
		"active":  active,
		"cached":  true,
	})
}
