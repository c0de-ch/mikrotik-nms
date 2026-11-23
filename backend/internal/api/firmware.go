package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/firmware"
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

// firmwareChecker is the release-feed checker: the injected one (tests) or
// one over the database. Fetches are serialised process-wide either way.
func (s *Server) firmwareChecker() *poller.FirmwareChecker {
	if s.firmware != nil {
		return s.firmware
	}
	return poller.NewFirmwareChecker(s.db, s.hub)
}

// handleFirmwareReleases lists the newest version per channel and when the
// feed was fetched.
func (s *Server) handleFirmwareReleases(w http.ResponseWriter, r *http.Request) {
	rels, err := queries.ListFirmwareReleases(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list firmware releases")
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

// handleCheckFirmware ("Check All") refreshes MikroTik's release feed and
// recomputes every device's comparison. It sends the devices nothing: their
// channel and installed version come from the inventory read. The feed is
// fetched at most once per poller.FirmwareFetchMinInterval; within that a
// check only recomputes (200), otherwise the fetch runs in the background
// (202).
func (s *Server) handleCheckFirmware(w http.ResponseWriter, r *http.Request) {
	if writeDialPolicyError(w, routeros.PollingAllowed()) {
		return
	}
	c := s.firmwareChecker()
	if last, ok := c.LastFetched(); ok && time.Since(last) < poller.FirmwareFetchMinInterval {
		if err := c.Recompute(); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to recompute firmware status")
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":     "recent",
			"fetched_at": last.UTC().Format(time.RFC3339),
			"next_fetch": last.Add(poller.FirmwareFetchMinInterval).UTC().Format(time.RFC3339),
		})
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := c.Refresh(ctx, 0); err != nil {
			log.Printf("firmware: release feed: %v", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "fetching"})
}

// jobCredentials validates the RouterOS login typed for a firmware action.
// The caller owns the result and must Zero it (an upgrade job does so when
// it ends). The credentials are never stored, logged or echoed.
func jobCredentials(w http.ResponseWriter, username, password string) (*routeros.Credentials, bool) {
	if strings.TrimSpace(username) == "" || password == "" {
		writeError(w, http.StatusBadRequest, "username and password of a RouterOS user allowed to upgrade the devices are required")
		return nil, false
	}
	if len(username) > 256 || len(password) > 1024 {
		writeError(w, http.StatusBadRequest, "username or password too long")
		return nil, false
	}
	return routeros.NewCredentials(username, password), true
}

func (s *Server) verifyTLS() bool {
	return s.cfg != nil && s.cfg.ROSTLSVerify
}

func (s *Server) handleUpgradeFirmware(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceIDs []string `json:"device_ids"`
		Reboot    bool     `json:"reboot"`
		Username  string   `json:"username"`
		Password  string   `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.DeviceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "device_ids required")
		return
	}
	creds, ok := jobCredentials(w, req.Username, req.Password)
	if !ok {
		return
	}
	req.Password = ""
	launched := false
	defer func() {
		if !launched {
			creds.Zero()
		}
	}()
	// A job that could never reach a device must not be created at all —
	// nor one that would upgrade a device whose polling is paused (a job
	// already running when the pause starts is not interrupted).
	if writeDialPolicyError(w, routeros.PollingAllowed()) {
		return
	}
	if s.writePollingPaused(w, req.DeviceIDs...) {
		return
	}

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

	// The job's sessions are one-shot connections logged in with the typed
	// credentials, which the executor zeroes when the job ends.
	executor := poller.NewUpgradeExecutor(s.db, s.pool, s.hub, creds, s.verifyTLS())
	launched = true
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

// firmwareAction runs act on a one-shot session per device, logged in with
// the typed credentials, then re-reads the device's firmware facts on the
// same session so the page shows the change at once. The first refused
// login stops the run: the remaining devices are not sent the same wrong
// password. It returns how many devices act succeeded on and the errors.
func (s *Server) firmwareAction(deviceIDs []string, creds *routeros.Credentials, act func(*queries.Device, routeros.Runner) error) (int, []string) {
	var done int
	errs := []string{}
	var refusedOn string
	checker := s.firmwareChecker()
	for _, id := range deviceIDs {
		dev, err := queries.GetDevice(s.db, id)
		if err != nil {
			errs = append(errs, id+": device not found")
			continue
		}
		name := deviceDisplayName(*dev)
		if refusedOn != "" {
			errs = append(errs, name+": skipped: "+poller.LoginRefusedMessage(refusedOn))
			continue
		}
		client, err := routeros.DialOnceWith(dev.Address, dev.APIPort, creds, dev.UseTLS, s.verifyTLS())
		if err != nil {
			if routeros.IsLoginRefused(err) {
				refusedOn = name
				errs = append(errs, name+": "+poller.LoginRefusedMessage(name))
			} else {
				errs = append(errs, name+": "+err.Error())
			}
			continue
		}
		run := routeros.OnceRunner{Client: client}
		if err := act(dev, run); err != nil {
			errs = append(errs, name+": "+err.Error())
			_ = client.Close()
			continue
		}
		done++
		if pkg, rb, err := poller.ReadFirmware(run); err == nil {
			checker.RecordDevice(dev.ID, pkg, rb)
		}
		_ = client.Close()
	}
	return done, errs
}

func (s *Server) handleSetChannel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceIDs []string `json:"device_ids"`
		Channel   string   `json:"channel"`
		Username  string   `json:"username"`
		Password  string   `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !firmware.ValidChannel(req.Channel) {
		writeError(w, http.StatusBadRequest, "channel must be stable, long-term, testing, or development")
		return
	}
	if len(req.DeviceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "device_ids required")
		return
	}
	creds, ok := jobCredentials(w, req.Username, req.Password)
	if !ok {
		return
	}
	req.Password = ""
	defer creds.Zero()
	if writeDialPolicyError(w, routeros.PollingAllowed()) {
		return
	}
	if s.writePollingPaused(w, req.DeviceIDs...) {
		return
	}

	changed, errs := s.firmwareAction(req.DeviceIDs, creds, func(_ *queries.Device, run routeros.Runner) error {
		return routeros.SetChannel(run, req.Channel)
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"changed": changed,
		"errors":  errs,
	})
}

func (s *Server) handleUpgradeRouterboard(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceIDs []string `json:"device_ids"`
		Reboot    bool     `json:"reboot"`
		Username  string   `json:"username"`
		Password  string   `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.DeviceIDs) == 0 {
		writeError(w, http.StatusBadRequest, "device_ids required")
		return
	}
	creds, ok := jobCredentials(w, req.Username, req.Password)
	if !ok {
		return
	}
	req.Password = ""
	defer creds.Zero()
	if writeDialPolicyError(w, routeros.PollingAllowed()) {
		return
	}
	if s.writePollingPaused(w, req.DeviceIDs...) {
		return
	}

	upgraded, errs := s.firmwareAction(req.DeviceIDs, creds, func(dev *queries.Device, run routeros.Runner) error {
		if err := routeros.UpgradeRouterboard(run); err != nil {
			return err
		}
		if req.Reboot {
			// The reboot usually drops the session before it answers; the
			// pooled poll session dies with it.
			_ = routeros.Reboot(run)
			s.pool.Close(dev.ID)
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"upgraded": upgraded,
		"reboot":   req.Reboot,
		"errors":   errs,
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

// handleHealth is unauthenticated, so it carries only what every visitor may
// know: liveness, the instance id the version watchdog compares, and whether
// this instance polls at all (the UI shows a banner when it does not). The
// allow-list of subnets is on the authenticated /system endpoint.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":      "ok",
		"instance_id": instanceID,
		"pollers":     s.pollersMode(),
	})
}
