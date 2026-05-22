package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

func (s *Server) handleListFirmware(w http.ResponseWriter, r *http.Request) {
	statuses, err := queries.ListFirmwareStatus(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list firmware status")
		return
	}
	if statuses == nil {
		statuses = []queries.FirmwareStatus{}
	}
	writeJSON(w, http.StatusOK, statuses)
}

func (s *Server) handleCheckFirmware(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "firmware check triggered"})
}

func (s *Server) handleUpgradeFirmware(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceIDs []string `json:"device_ids"`
		Reboot    bool     `json:"reboot"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.DeviceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "device_ids required")
		return
	}

	// Create upgrade job
	jobID := uuid.NewString()
	job := &queries.UpgradeJob{
		ID:     jobID,
		Status: "pending",
		Reboot: req.Reboot,
	}
	if err := queries.CreateUpgradeJob(s.db, job); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create upgrade job")
		return
	}

	// Create per-device entries
	for _, deviceID := range req.DeviceIDs {
		jd := &queries.UpgradeJobDevice{
			ID:       uuid.NewString(),
			JobID:    jobID,
			DeviceID: deviceID,
			Status:   "pending",
		}
		if err := queries.CreateUpgradeJobDevice(s.db, jd); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create job device entry")
			return
		}
	}

	// Launch async execution
	executor := poller.NewUpgradeExecutor(s.db, routeros.NewPool(), s.hub)
	go executor.Execute(jobID)

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"job_id":     jobID,
		"status":     "pending",
		"device_ids": req.DeviceIDs,
		"reboot":     req.Reboot,
	})
}

func (s *Server) handleGetUpgradeJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobId")
	job, err := queries.GetUpgradeJob(s.db, jobID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "job not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get job")
		return
	}

	devices, err := queries.ListUpgradeJobDevices(s.db, jobID)
	if err != nil {
		devices = []queries.UpgradeJobDevice{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"job":     job,
		"devices": devices,
	})
}

func (s *Server) handleSetChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceIDs []string `json:"device_ids"`
		Channel   string   `json:"channel"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Channel != "stable" && req.Channel != "long-term" && req.Channel != "testing" && req.Channel != "development" {
		writeError(w, http.StatusBadRequest, "channel must be stable, long-term, testing, or development")
		return
	}
	if len(req.DeviceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "device_ids required")
		return
	}

	var changed int
	var errors []string
	for _, deviceID := range req.DeviceIDs {
		client := s.pool.Get(deviceID)
		if client == nil {
			errors = append(errors, deviceID+": not connected")
			continue
		}
		if err := routeros.SetChannel(client, req.Channel); err != nil {
			errors = append(errors, deviceID+": "+err.Error())
			continue
		}
		changed++
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"changed": changed,
		"errors":  errors,
	})
}

func (s *Server) handleUpgradeRouterboard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceIDs []string `json:"device_ids"`
		Reboot    bool     `json:"reboot"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.DeviceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "device_ids required")
		return
	}

	var upgraded int
	var errors []string
	for _, deviceID := range req.DeviceIDs {
		client := s.pool.Get(deviceID)
		if client == nil {
			errors = append(errors, deviceID+": not connected")
			continue
		}
		if err := routeros.UpgradeRouterboard(client); err != nil {
			errors = append(errors, deviceID+": "+err.Error())
			continue
		}
		upgraded++
		if req.Reboot {
			_ = routeros.TriggerReboot(client)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"upgraded": upgraded,
		"reboot":   req.Reboot,
		"errors":   errors,
	})
}

// instanceID is regenerated every time the backend starts. Clients can use
// it to detect a redeploy by polling /health and comparing the value they
// saw on first connect — useful for forcing a browser refresh after a
// frontend rebuild changes the JS chunk hashes.
var instanceID = func() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a timestamp if the OS RNG is unavailable for some
		// reason — uniqueness across restarts is the only invariant we
		// actually rely on.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}()

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":      "ok",
		"instance_id": instanceID,
	})
}
