package api

import (
	"database/sql"
	"net/http"

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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
