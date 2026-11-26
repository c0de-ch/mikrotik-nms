package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/macvendor"
	"github.com/mikrotik-nms/backend/internal/poller"
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
	// Vendor / Randomized are derived from the MAC's OUI (fallback identity when
	// there's no DHCP/DNS hostname).
	Vendor     string `json:"vendor,omitempty"`
	Randomized bool   `json:"randomized,omitempty"`
}

// handleDebugWifiRaw returns raw fields from the wifi registration table for debugging.
func (s *Server) handleDebugWifiRaw(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "device_id required")
		return
	}
	if s.writePollingPaused(w, deviceID) {
		return
	}
	client := s.pool.GetLive(deviceID)
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

// clientRefresher is the part of *poller.ClientRefresher the handlers use;
// tests substitute a fake. nil = not wired: the cache is served, a refresh
// is not available.
type clientRefresher interface {
	Status() poller.ClientRefreshStatus
	Start() (poller.ClientRefreshStatus, error)
}

// clientRefreshView is the refresh state as the client list and the refresh
// endpoint report it.
type clientRefreshView struct {
	poller.ClientRefreshStatus
	// Available is false on an instance that cannot refresh at all (polling
	// off): the UI then shows the cache without a Refresh control.
	Available bool `json:"available"`
}

func (s *Server) clientRefreshView() clientRefreshView {
	if s.clients == nil {
		return clientRefreshView{ClientRefreshStatus: poller.ClientRefreshStatus{
			MinIntervalSeconds: int(poller.ClientRefreshMinInterval / time.Second),
		}}
	}
	return clientRefreshView{ClientRefreshStatus: s.clients.Status(), Available: routeros.PollingAllowed() == nil}
}

// handleRefreshClients starts the on-demand refresh of the client cache —
// the one way a page view can make the fleet read its ARP, lease and
// registration tables ahead of schedule. It answers at once: 202 with the
// refresh state (the list endpoint reports when it is done), 409 while one
// is running, 429 with Retry-After inside the minimum interval, 503 on an
// instance that polls nothing.
func (s *Server) handleRefreshClients(w http.ResponseWriter, r *http.Request) {
	if s.clients == nil {
		writeError(w, http.StatusServiceUnavailable, "client refresh is not available on this instance")
		return
	}
	st, err := s.clients.Start()
	view := clientRefreshView{ClientRefreshStatus: st, Available: true}
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"refresh": view})
	case errors.Is(err, poller.ErrClientRefreshRunning):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "refresh": view})
	case errors.Is(err, poller.ErrClientRefreshTooSoon):
		if st.NextAllowedAt != nil {
			if wait := int(time.Until(*st.NextAllowedAt).Seconds()) + 1; wait > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(wait))
			}
		}
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error(), "refresh": view})
	case writeDialPolicyError(w, err):
	default:
		writeError(w, http.StatusInternalServerError, "failed to start the client refresh")
	}
}

// defaultClientInactiveAfter is the least time a cached client may go
// without being re-seen before it is reported inactive, when the admin
// hasn't configured client_inactive_after_seconds: far below the ~30-day
// cache retention, so a device that left (or a reservation on a disabled
// DHCP server) drops off the active list promptly instead of lingering for
// weeks.
const defaultClientInactiveAfter = 30 * time.Minute

// clientReadMargin is added to two reads of the slowest ARP reader: a read
// that runs a little late (jitter, a busy device) must not flip its clients
// to inactive.
const clientReadMargin = 10 * time.Minute

// clientInactiveAfter is how long a cached client may go unseen before the
// list reports it inactive: the client_inactive_after_seconds setting, else
// two reads of the fleet's slowest ARP reader plus a margin (weak devices
// are read every 30 minutes, so a fixed half hour would make their clients
// flicker), never under defaultClientInactiveAfter.
func (s *Server) clientInactiveAfter(now time.Time) time.Duration {
	if v, err := queries.GetSetting(s.db, "client_inactive_after_seconds"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return max(defaultClientInactiveAfter, 2*s.slowestClientRead(now)+clientReadMargin)
}

// slowestClientRead is the longest ARP cadence among the devices being
// polled right now (0 when there is none).
func (s *Server) slowestClientRead(now time.Time) time.Duration {
	devices, err := queries.ListDevices(s.db)
	if err != nil {
		return 0
	}
	cs := s.cadenceSource()
	var slowest time.Duration
	for _, d := range devices {
		if d.Status != "online" || cs.Paused(d, now) {
			continue
		}
		slowest = max(slowest, cs.Interval(d, devclass.TierARP, now))
	}
	return slowest
}

// handleListClients serves the client list from the cache (mac_lookup) the
// scheduled pollers keep — no device is contacted — with the age of its
// newest entry and the state of the on-demand refresh. It answers both
// GET /clients and GET /clients/cached: /clients used to scan the whole
// fleet on every call, so its scan fields (limited, timed_out) are still
// present for a browser tab running the previous frontend.
func (s *Server) handleListClients(w http.ResponseWriter, r *http.Request) {
	entries, err := queries.GetAllMACLookupsSlice(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load cached clients")
		return
	}

	now := time.Now()
	threshold := s.clientInactiveAfter(now)
	active := 0
	var newest time.Time
	clients := make([]networkClient, 0, len(entries))
	for _, e := range entries {
		isActive := now.Sub(e.UpdatedAt) < threshold
		if isActive {
			active++
		}
		if e.UpdatedAt.After(newest) {
			newest = e.UpdatedAt
		}
		nc := networkClient{
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
		}
		nc.Vendor, nc.Randomized = macvendor.Describe(e.MACAddress)
		clients = append(clients, nc)
	}

	resp := map[string]interface{}{
		"clients":   clients,
		"total":     len(clients),
		"active":    active,
		"cached":    true,
		"limited":   false,
		"timed_out": false,
		"refresh":   s.clientRefreshView(),
	}
	// The cache is as old as its newest entry: every scheduled read and
	// every refresh re-stamps the clients it saw.
	if !newest.IsZero() {
		resp["updated_at"] = newest.UTC().Format(time.RFC3339)
		resp["age_seconds"] = max(0, int(now.Sub(newest).Seconds()))
	}
	writeJSON(w, http.StatusOK, resp)
}
