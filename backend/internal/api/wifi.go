package api

import (
	"net/http"
	"strconv"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/macvendor"
)

type enrichedWifiEntry struct {
	queries.WifiHistoryEntry
	ControllerName string `json:"controller_name"`
}

func (s *Server) enrichWifiEntries(entries []queries.WifiHistoryEntry) []enrichedWifiEntry {
	// Build device ID → name lookup
	devices, _ := queries.ListDevices(s.db)
	deviceNames := make(map[string]string)
	for _, d := range devices {
		name := d.Identity
		if name == "" {
			name = d.Address
		}
		deviceNames[d.ID] = name
	}

	// Fill empty IP/hostname from mac_lookup (ARP/DHCP data)
	macLookups, _ := queries.GetAllMACLookups(s.db)

	result := make([]enrichedWifiEntry, len(entries))
	for i, e := range entries {
		if lookup, ok := macLookups[e.MACAddress]; ok {
			if e.IPAddress == "" {
				e.IPAddress = lookup.IPAddress
			}
			if e.HostName == "" {
				e.HostName = lookup.HostName
				if e.HostName == "" {
					e.HostName = lookup.DNSName
				}
			}
		}
		result[i] = enrichedWifiEntry{
			WifiHistoryEntry: e,
			ControllerName:   deviceNames[e.ControllerID],
		}
	}
	return result
}

func (s *Server) handleMACLookup(w http.ResponseWriter, r *http.Request) {
	lookups, err := queries.GetAllMACLookups(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get MAC lookups")
		return
	}
	// Derive the hardware vendor / randomized flag from each OUI so the WiFi and
	// Clients pages can fall back to a vendor name when there's no DHCP/DNS name.
	for _, m := range lookups {
		m.Vendor, m.Randomized = macvendor.Describe(m.MACAddress)
	}
	writeJSON(w, http.StatusOK, lookups)
}

func (s *Server) handleWifiCurrent(w http.ResponseWriter, r *http.Request) {
	clients, err := queries.GetWifiClientsCurrentAP(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get wifi clients")
		return
	}
	if clients == nil {
		clients = []queries.WifiHistoryEntry{}
	}
	writeJSON(w, http.StatusOK, s.enrichWifiEntries(clients))
}

func (s *Server) handleWifiHistory(w http.ResponseWriter, r *http.Request) {
	mac := r.URL.Query().Get("mac")
	ap := r.URL.Query().Get("ap")
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if l, err := strconv.Atoi(v); err == nil && l > 0 && l <= 5000 {
			limit = l
		}
	}

	var entries []queries.WifiHistoryEntry
	var err error

	if mac != "" {
		entries, err = queries.GetWifiHistoryByMAC(s.db, mac, limit)
	} else if ap != "" {
		entries, err = queries.GetWifiHistoryByAP(s.db, ap, limit)
	} else {
		entries, err = queries.GetWifiHistoryRecent(s.db, limit)
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get wifi history")
		return
	}
	if entries == nil {
		entries = []queries.WifiHistoryEntry{}
	}
	writeJSON(w, http.StatusOK, s.enrichWifiEntries(entries))
}
