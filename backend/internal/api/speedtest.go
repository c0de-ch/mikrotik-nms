package api

import (
	"database/sql"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
)

// enrichedSpeedTest is a SpeedTest plus display-only context: the measuring
// device's name and the newest sample.
type enrichedSpeedTest struct {
	queries.SpeedTest
	DeviceName string               `json:"device_name"`
	LastSample *queries.SpeedSample `json:"last_sample"`
}

// validSpeedTestURL accepts the http(s) URLs RouterOS /tool/fetch can
// meaningfully measure a download from.
func validSpeedTestURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// enrichSpeedTests decorates tests with device names and their latest sample.
func (s *Server) enrichSpeedTests(tests []queries.SpeedTest) []enrichedSpeedTest {
	latest, _ := queries.GetLatestSpeedSamples(s.db)
	deviceNames := s.deviceNameMap()

	out := make([]enrichedSpeedTest, 0, len(tests))
	for _, t := range tests {
		out = append(out, enrichedSpeedTest{
			SpeedTest:  t,
			DeviceName: deviceNames[t.DeviceID],
			LastSample: latest[t.ID],
		})
	}
	return out
}

func (s *Server) enrichSpeedTest(t *queries.SpeedTest) enrichedSpeedTest {
	return s.enrichSpeedTests([]queries.SpeedTest{*t})[0]
}

func (s *Server) handleListSpeedTests(w http.ResponseWriter, r *http.Request) {
	tests, err := queries.ListSpeedTests(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list speed tests")
		return
	}
	writeJSON(w, http.StatusOK, s.enrichSpeedTests(tests))
}

func (s *Server) handleCreateSpeedTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID   string `json:"device_id"`
		URL        string `json:"url"`
		SrcAddress string `json:"src_address"`
		Label      string `json:"label"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	t := &queries.SpeedTest{
		ID:         uuid.NewString(),
		DeviceID:   strings.TrimSpace(req.DeviceID),
		URL:        strings.TrimSpace(req.URL),
		SrcAddress: strings.TrimSpace(req.SrcAddress),
		Label:      strings.TrimSpace(req.Label),
		Enabled:    true,
	}
	if t.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "device_id is required")
		return
	}
	if !validSpeedTestURL(t.URL) {
		writeError(w, http.StatusBadRequest, "url must be a valid http(s) URL")
		return
	}
	if t.SrcAddress != "" && net.ParseIP(t.SrcAddress) == nil {
		writeError(w, http.StatusBadRequest, "src_address must be a valid IP address")
		return
	}

	if err := queries.CreateSpeedTest(s.db, t); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create speed test")
		return
	}
	// Re-read so created_at (DB default) is populated.
	created, err := queries.GetSpeedTest(s.db, t.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load created speed test")
		return
	}
	writeJSON(w, http.StatusCreated, s.enrichSpeedTest(created))
}

func (s *Server) handleUpdateSpeedTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := queries.GetSpeedTest(s.db, id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "speed test not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get speed test")
		return
	}

	var req struct {
		DeviceID   *string `json:"device_id"`
		URL        *string `json:"url"`
		SrcAddress *string `json:"src_address"`
		Label      *string `json:"label"`
		Enabled    *bool   `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.DeviceID != nil {
		t.DeviceID = strings.TrimSpace(*req.DeviceID)
	}
	if req.URL != nil {
		t.URL = strings.TrimSpace(*req.URL)
	}
	if req.SrcAddress != nil {
		t.SrcAddress = strings.TrimSpace(*req.SrcAddress)
	}
	if req.Label != nil {
		t.Label = strings.TrimSpace(*req.Label)
	}
	if req.Enabled != nil {
		t.Enabled = *req.Enabled
	}
	if t.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "device_id is required")
		return
	}
	if !validSpeedTestURL(t.URL) {
		writeError(w, http.StatusBadRequest, "url must be a valid http(s) URL")
		return
	}
	if t.SrcAddress != "" && net.ParseIP(t.SrcAddress) == nil {
		writeError(w, http.StatusBadRequest, "src_address must be a valid IP address")
		return
	}

	if err := queries.UpdateSpeedTest(s.db, t); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update speed test")
		return
	}
	writeJSON(w, http.StatusOK, s.enrichSpeedTest(t))
}

func (s *Server) handleDeleteSpeedTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := queries.DeleteSpeedTest(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "speed test not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete speed test")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleRunSpeedTest starts an async measurement and returns 202 immediately —
// a download can take 60-120s, far past the HTTP write timeout, and runs on a
// DEDICATED connection (poller.RunSpeedTest dials with the device's
// credentials) so it never holds a pooled client's mutex. The sample is
// persisted and broadcast on "connectivity.speed". The run guard shared with
// the scheduled poller yields 409 for concurrent runs of the same test, and
// the global API run-slot cap yields 409 when too many run-nows are in flight.
func (s *Server) handleRunSpeedTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := queries.GetSpeedTest(s.db, id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "speed test not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get speed test")
		return
	}

	dev, err := queries.GetDevice(s.db, t.DeviceID)
	if err != nil {
		writeError(w, http.StatusConflict, "device no longer exists")
		return
	}
	if dev.Status != "online" {
		writeError(w, http.StatusConflict, "device is "+dev.Status)
		return
	}

	key := "speedtest:" + t.ID
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
				log.Printf("api: panic in speed test run %s: %v", t.ID, r)
			}
		}()

		sample := poller.RunSpeedTest(s.db, *t, verifyTLS)
		// Persist FIRST (the WS publish drops for slow clients), then broadcast.
		if err := queries.InsertSpeedSample(s.db, sample); err != nil {
			log.Printf("api: insert speed sample for test %s: %v", t.ID, err)
			return
		}
		s.hub.Publish("connectivity.speed", sample)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

func (s *Server) handleGetSpeedSamples(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := queries.GetSpeedTest(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "speed test not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get speed test")
		return
	}

	from, to, limit := parseTimeRange(r, 7*24*time.Hour, 500, 5000)
	samples, err := queries.GetSpeedSamples(s.db, id, from, to, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get speed samples")
		return
	}
	if samples == nil {
		samples = []queries.SpeedSample{}
	}
	writeJSON(w, http.StatusOK, samples)
}
