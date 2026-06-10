package api

import (
	"database/sql"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// enrichedPingTarget is a PingTarget plus display-only context: the probing
// device's name, the client hostname (client kind), and the newest sample.
type enrichedPingTarget struct {
	queries.PingTarget
	DeviceName string              `json:"device_name"`
	HostName   string              `json:"host_name"`
	LastSample *queries.PingSample `json:"last_sample"`
}

// clientTimelineResponse correlates everything the NMS knows about one client
// over a time window. All arrays are newest-first and never null.
type clientTimelineResponse struct {
	Pings         []queries.PingSample         `json:"pings"`
	Signals       []queries.ClientSignalSample `json:"signals"`
	WifiEvents    []enrichedWifiEntry          `json:"wifi_events"`
	NetworkEvents []enrichedLoopEvent          `json:"network_events"`
}

// normalizeMAC parses a MAC in any form net.ParseMAC accepts (colon, dash or
// Cisco dot notation) and returns the canonical uppercase colon-separated form
// every other table (mac_lookup, wifi_history, client_signal_samples) is keyed
// on. Returns "" when the input is not a valid 48-bit MAC.
func normalizeMAC(mac string) string {
	hw, err := net.ParseMAC(strings.TrimSpace(mac))
	if err != nil || len(hw) != 6 {
		return ""
	}
	return strings.ToUpper(hw.String())
}

// validProbeAddress reports whether s is acceptable as a probe address: an IP
// literal (v4 or v6 — anything net.ParseIP accepts) or a plausible hostname
// (letters, digits, dots, hyphens, underscores). The hostname charset
// implicitly rejects whitespace and control characters, which would otherwise
// be smuggled into the RouterOS command sentence.
func validProbeAddress(s string) bool {
	if s == "" {
		return false
	}
	if net.ParseIP(s) != nil {
		return true
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// enrichPingTargets decorates targets with device names, client hostnames and
// their latest sample.
func (s *Server) enrichPingTargets(targets []queries.PingTarget) []enrichedPingTarget {
	latest, _ := queries.GetLatestPingSamples(s.db)
	deviceNames := s.deviceNameMap()
	macLookups, _ := queries.GetAllMACLookups(s.db)

	out := make([]enrichedPingTarget, 0, len(targets))
	for _, t := range targets {
		e := enrichedPingTarget{
			PingTarget: t,
			DeviceName: deviceNames[t.DeviceID],
			LastSample: latest[t.ID],
		}
		if t.Kind == "client" {
			if lookup, ok := macLookups[t.MACAddress]; ok {
				e.HostName = lookup.HostName
				if e.HostName == "" {
					e.HostName = lookup.DNSName
				}
			}
		}
		out = append(out, e)
	}
	return out
}

func (s *Server) enrichPingTarget(t *queries.PingTarget) enrichedPingTarget {
	return s.enrichPingTargets([]queries.PingTarget{*t})[0]
}

func (s *Server) handleListPingTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := queries.ListPingTargets(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list ping targets")
		return
	}
	writeJSON(w, http.StatusOK, s.enrichPingTargets(targets))
}

func (s *Server) handleCreatePingTarget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind         string `json:"kind"`
		Address      string `json:"address"`
		MACAddress   string `json:"mac_address"`
		Label        string `json:"label"`
		DeviceID     string `json:"device_id"`
		SrcAddress   string `json:"src_address"`
		SrcInterface string `json:"src_interface"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	t := &queries.PingTarget{
		ID:           uuid.NewString(),
		Kind:         req.Kind,
		Address:      strings.TrimSpace(req.Address),
		Label:        strings.TrimSpace(req.Label),
		DeviceID:     strings.TrimSpace(req.DeviceID),
		SrcAddress:   strings.TrimSpace(req.SrcAddress),
		SrcInterface: strings.TrimSpace(req.SrcInterface),
		Enabled:      true,
	}
	if t.SrcAddress != "" && net.ParseIP(t.SrcAddress) == nil {
		writeError(w, http.StatusBadRequest, "src_address must be a valid IP address")
		return
	}

	switch req.Kind {
	case "internet":
		if t.Address == "" {
			writeError(w, http.StatusBadRequest, "address is required for internet targets")
			return
		}
		if !validProbeAddress(t.Address) {
			writeError(w, http.StatusBadRequest, "address must be an IP address (v4 or v6) or a hostname")
			return
		}
		if t.DeviceID == "" {
			writeError(w, http.StatusBadRequest, "device_id is required for internet targets")
			return
		}
	case "client":
		mac := normalizeMAC(req.MACAddress)
		if mac == "" {
			writeError(w, http.StatusBadRequest, "a valid mac_address is required for client targets")
			return
		}
		t.MACAddress = mac
		// Default the label to the client's current hostname, if known.
		if t.Label == "" {
			if lookup, err := queries.GetMACLookup(s.db, mac); err == nil {
				t.Label = lookup.HostName
				if t.Label == "" {
					t.Label = lookup.DNSName
				}
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "kind must be \"internet\" or \"client\"")
		return
	}

	if err := queries.CreatePingTarget(s.db, t); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create ping target")
		return
	}
	// Re-read so created_at (DB default) is populated.
	created, err := queries.GetPingTarget(s.db, t.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load created ping target")
		return
	}
	writeJSON(w, http.StatusCreated, s.enrichPingTarget(created))
}

func (s *Server) handleUpdatePingTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := queries.GetPingTarget(s.db, id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "ping target not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get ping target")
		return
	}

	var req struct {
		Address      *string `json:"address"`
		Label        *string `json:"label"`
		DeviceID     *string `json:"device_id"`
		SrcAddress   *string `json:"src_address"`
		SrcInterface *string `json:"src_interface"`
		Enabled      *bool   `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Address != nil {
		t.Address = strings.TrimSpace(*req.Address)
	}
	if req.Label != nil {
		t.Label = strings.TrimSpace(*req.Label)
	}
	if req.DeviceID != nil {
		t.DeviceID = strings.TrimSpace(*req.DeviceID)
	}
	if req.SrcAddress != nil {
		t.SrcAddress = strings.TrimSpace(*req.SrcAddress)
	}
	if req.SrcInterface != nil {
		t.SrcInterface = strings.TrimSpace(*req.SrcInterface)
	}
	if req.Enabled != nil {
		t.Enabled = *req.Enabled
	}
	if t.SrcAddress != "" && net.ParseIP(t.SrcAddress) == nil {
		writeError(w, http.StatusBadRequest, "src_address must be a valid IP address")
		return
	}
	if t.Kind == "internet" && t.Address == "" {
		writeError(w, http.StatusBadRequest, "address is required for internet targets")
		return
	}
	// Validate any non-empty address (client targets' cached addresses are
	// resolved IPs, so this only ever rejects hand-entered garbage).
	if t.Address != "" && !validProbeAddress(t.Address) {
		writeError(w, http.StatusBadRequest, "address must be an IP address (v4 or v6) or a hostname")
		return
	}
	// Mirror the create-time invariant: an internet target without a probing
	// device can never run and would only accumulate error samples.
	if t.Kind == "internet" && t.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "device_id is required for internet targets")
		return
	}

	if err := queries.UpdatePingTarget(s.db, t); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update ping target")
		return
	}
	writeJSON(w, http.StatusOK, s.enrichPingTarget(t))
}

func (s *Server) handleDeletePingTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := queries.DeletePingTarget(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "ping target not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete ping target")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// connectivityPingCount mirrors the poller's runtime-tunable burst size for the
// synchronous run-now endpoint.
func (s *Server) connectivityPingCount() int {
	count := 5
	if v, err := queries.GetSetting(s.db, "connectivity_ping_count"); err == nil {
		if n, e := strconv.Atoi(strings.TrimSpace(v)); e == nil {
			count = n
		}
	}
	if count < 1 {
		count = 1
	}
	if count > 10 {
		count = 10
	}
	return count
}

// handleRunPingTarget runs one probe synchronously, persists + broadcasts the
// sample, and returns it. The burst itself is ~2-3s worst case (count<=10 at
// 0.2s spacing); the per-client mutex can add a wait if the poller is mid-burst
// on the same device, but even queued behind one full burst the total stays
// well inside the server's 15s write timeout. Pre-probe failures (unresolvable
// client, offline device, no API connection) return 409 instead of recording a
// sample.
func (s *Server) handleRunPingTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := queries.GetPingTarget(s.db, id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "ping target not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get ping target")
		return
	}

	var deviceID, address string
	if t.Kind == "client" {
		devID, addr, _, reason := queries.ResolveClientProbe(s.db, t)
		if addr != "" && addr != t.Address {
			_ = queries.UpdatePingTargetAddress(s.db, t.ID, addr)
		}
		if reason != "" {
			writeError(w, http.StatusConflict, "cannot probe client: "+reason)
			return
		}
		deviceID, address = devID, addr
	} else {
		if t.DeviceID == "" {
			writeError(w, http.StatusConflict, "no probing device configured")
			return
		}
		dev, err := queries.GetDevice(s.db, t.DeviceID)
		if err != nil {
			writeError(w, http.StatusConflict, "probing device no longer exists")
			return
		}
		if dev.Status != "online" {
			writeError(w, http.StatusConflict, "probing device is "+dev.Status)
			return
		}
		deviceID, address = t.DeviceID, t.Address
	}

	client := s.pool.Get(deviceID)
	if client == nil {
		writeError(w, http.StatusConflict, "no API connection to probing device yet — try again shortly")
		return
	}

	sample := &queries.PingSample{
		TargetID: t.ID,
		DeviceID: deviceID,
		Address:  address,
	}
	res, err := routeros.RunPing(client, routeros.PingOptions{
		Address:    address,
		Count:      s.connectivityPingCount(),
		SrcAddress: t.SrcAddress,
		Interface:  t.SrcInterface,
	})
	if err != nil {
		// A trap (e.g. invalid address) is a real probe outcome: record it.
		sample.Error = err.Error()
	} else {
		sample.Sent = res.Sent
		sample.Received = res.Received
		sample.LossPct = res.LossPct
		sample.RTTMinMs = res.RTTMinMs
		sample.RTTAvgMs = res.RTTAvgMs
		sample.RTTMaxMs = res.RTTMaxMs
		sample.JitterMs = res.JitterMs
	}

	if err := queries.InsertPingSample(s.db, sample); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save ping sample")
		return
	}
	s.hub.Publish("connectivity.sample", sample)
	writeJSON(w, http.StatusOK, sample)
}

// parseTimeRange reads from/to/limit query params with a default lookback
// window ending now. All bounds are normalized to UTC: samples are stored with
// UTC timestamps and SQLite compares DATETIME TEXT lexicographically, so a
// local-zone bound on a non-UTC host would silently shift the window by the
// UTC offset (hiding the newest hours of data west of UTC).
func parseTimeRange(r *http.Request, lookback time.Duration, defaultLimit, maxLimit int) (from, to time.Time, limit int) {
	now := time.Now().UTC()
	from = now.Add(-lookback)
	to = now
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t.UTC()
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t.UTC()
		}
	}
	limit = defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if l, err := strconv.Atoi(v); err == nil && l > 0 && l <= maxLimit {
			limit = l
		}
	}
	return from, to, limit
}

func (s *Server) handleGetPingSamples(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := queries.GetPingTarget(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "ping target not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get ping target")
		return
	}

	from, to, limit := parseTimeRange(r, 24*time.Hour, 3000, 10000)
	samples, err := queries.GetPingSamples(s.db, id, from, to, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get ping samples")
		return
	}
	if samples == nil {
		samples = []queries.PingSample{}
	}
	writeJSON(w, http.StatusOK, samples)
}

// tracerouteRunTimeout bounds one /tool/traceroute run on its dedicated
// connection (20 hops × count=1 against an unresponsive path stays under it).
const tracerouteRunTimeout = 45 * time.Second

// handleRunTraceroute starts an async traceroute for an internet target and
// returns 202 immediately — up to ~45s on a DEDICATED connection (DialOnce)
// would blow the HTTP write timeout and, on a pooled client, hold its mutex
// past CommandTimeout. The result is persisted and broadcast on
// "connectivity.traceroute". The run guard shared with the auto-traceroute
// poller yields 409 for concurrent runs against the same target, and the
// global API run-slot cap yields 409 when too many run-nows are in flight.
func (s *Server) handleRunTraceroute(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := queries.GetPingTarget(s.db, id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "ping target not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get ping target")
		return
	}
	if t.Kind == "client" {
		writeError(w, http.StatusBadRequest, "traceroute is only available for internet targets")
		return
	}
	if t.DeviceID == "" {
		writeError(w, http.StatusConflict, "no probing device configured")
		return
	}
	dev, err := queries.GetDevice(s.db, t.DeviceID)
	if err != nil {
		writeError(w, http.StatusConflict, "probing device no longer exists")
		return
	}
	if dev.Status != "online" {
		writeError(w, http.StatusConflict, "probing device is "+dev.Status)
		return
	}

	key := "traceroute:" + t.ID
	if !poller.TryBeginRun(key) {
		writeError(w, http.StatusConflict, "a run for this test/target is already in progress")
		return
	}
	if !poller.TryAcquireAPIRunSlot() {
		poller.EndRun(key)
		writeError(w, http.StatusConflict, "too many runs in flight — try again shortly")
		return
	}

	verifyTLS := s.cfg.ROSTLSVerify
	go func() {
		defer poller.EndRun(key)
		defer poller.ReleaseAPIRunSlot()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("api: panic in traceroute for target %s: %v", t.ID, r)
			}
		}()

		run := &queries.TracerouteRun{TargetID: t.ID, Address: t.Address}
		client, err := routeros.DialOnce(dev.Address, dev.APIPort, dev.Username, dev.PasswordEnc, dev.UseTLS, verifyTLS)
		if err != nil {
			run.Error = err.Error()
		} else {
			hops, err := routeros.Traceroute(client, t.Address, t.SrcAddress, t.SrcInterface, tracerouteRunTimeout)
			client.Close()
			if err != nil {
				run.Error = err.Error()
			} else {
				run.Hops = hops
			}
		}

		// Persist FIRST (the WS publish drops for slow clients), then broadcast.
		if err := queries.InsertTracerouteRun(s.db, run); err != nil {
			log.Printf("api: insert traceroute run for target %s: %v", t.ID, err)
			return
		}
		s.hub.Publish("connectivity.traceroute", run)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) handleListTracerouteRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := queries.GetPingTarget(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "ping target not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get ping target")
		return
	}

	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if l, err := strconv.Atoi(v); err == nil && l > 0 && l <= 50 {
			limit = l
		}
	}
	runs, err := queries.GetTracerouteRuns(s.db, id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get traceroute runs")
		return
	}
	if runs == nil {
		runs = []queries.TracerouteRun{}
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) handleClientTimeline(w http.ResponseWriter, r *http.Request) {
	// The frontend percent-encodes the MAC (AA%3ABB%3A...) and chi keeps path
	// values in their escaped form, so unescape before validating. PathUnescape
	// is a no-op for already-literal colons (curl-style requests).
	rawMAC := r.PathValue("mac")
	if u, err := url.PathUnescape(rawMAC); err == nil {
		rawMAC = u
	}
	mac := normalizeMAC(rawMAC)
	if mac == "" {
		writeError(w, http.StatusBadRequest, "invalid MAC address")
		return
	}

	from, to, _ := parseTimeRange(r, 24*time.Hour, 0, 0)

	resp := clientTimelineResponse{
		Pings:         []queries.PingSample{},
		Signals:       []queries.ClientSignalSample{},
		WifiEvents:    []enrichedWifiEntry{},
		NetworkEvents: []enrichedLoopEvent{},
	}

	// Ping samples: merge across all targets watching this MAC.
	if targets, err := queries.GetPingTargetsByMAC(s.db, mac); err == nil {
		for _, t := range targets {
			if samples, err := queries.GetPingSamples(s.db, t.ID, from, to, 10000); err == nil {
				resp.Pings = append(resp.Pings, samples...)
			}
		}
		if len(targets) > 1 {
			sort.Slice(resp.Pings, func(i, j int) bool {
				return resp.Pings[i].RecordedAt.After(resp.Pings[j].RecordedAt)
			})
		}
	}

	if signals, err := queries.GetClientSignalSamples(s.db, mac, from, to, 10000); err == nil && signals != nil {
		resp.Signals = signals
	}

	if entries, err := queries.GetWifiHistoryByMACRange(s.db, mac, from, to, 1000); err == nil && len(entries) > 0 {
		resp.WifiEvents = s.enrichWifiEntries(entries)
	}

	// Network-health events for the device the client hangs off (per mac_lookup).
	if lookup, err := queries.GetMACLookup(s.db, mac); err == nil && lookup.DeviceID != "" {
		if events, err := queries.GetLoopEventsByDeviceRange(s.db, lookup.DeviceID, from, to, 1000); err == nil && len(events) > 0 {
			deviceNames := s.deviceNameMap()
			for _, e := range events {
				resp.NetworkEvents = append(resp.NetworkEvents, enrichedLoopEvent{
					LoopEvent:  e,
					DeviceName: deviceNames[e.DeviceID],
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
