package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// Per-device polling controls: an admin can pause every poller but liveness
// for a while, switch a device to the light (C1) cadences, or override its
// CPU class with a gentler one. Each change is written to audit_log.

// maxPauseHours bounds a pause; a device that needs longer is one to remove.
const maxPauseHours = 24 * 7

type pausePollingRequest struct {
	Hours int `json:"hours"`
}

// audit records an operator action on a device; failures are logged, never
// surfaced, so an audit write can't block the action it describes.
func (s *Server) audit(r *http.Request, action, deviceID, detail string) {
	e := &queries.AuditEntry{Action: action, DeviceID: deviceID, Detail: detail}
	if u := auth.UserFromContext(r.Context()); u != nil {
		e.UserID, e.Username = u.ID, u.Username
	}
	if _, err := queries.InsertAuditLog(s.db, e); err != nil {
		log.Printf("audit: %s on %s: %v", action, deviceID, err)
	}
}

func (s *Server) loadDevice(w http.ResponseWriter, id string) (*queries.Device, bool) {
	d, err := queries.GetDevice(s.db, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "device not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to get device")
		}
		return nil, false
	}
	return d, true
}

// handlePausePolling — POST /devices/{id}/polling/pause {"hours": 1|24}
func (s *Server) handlePausePolling(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.loadDevice(w, id); !ok {
		return
	}
	var req pausePollingRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Hours < 1 || req.Hours > maxPauseHours {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("hours must be between 1 and %d", maxPauseHours))
		return
	}
	until := time.Now().Add(time.Duration(req.Hours) * time.Hour).Truncate(time.Second)
	if err := queries.PauseDevicePolling(s.db, id, until); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to pause polling")
		return
	}
	s.audit(r, "polling_paused", id, fmt.Sprintf("%d h, until %s", req.Hours, until.UTC().Format(time.RFC3339)))
	d, ok := s.loadDevice(w, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, withPolling(d))
}

// handleResumePolling — POST /devices/{id}/polling/resume
func (s *Server) handleResumePolling(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, ok := s.loadDevice(w, id)
	if !ok {
		return
	}
	if d.PollingPausedUntil != nil {
		if err := queries.ResumeDevicePolling(s.db, id); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to resume polling")
			return
		}
		s.audit(r, "polling_resumed", id, "")
		if d, ok = s.loadDevice(w, id); !ok {
			return
		}
	}
	writeJSON(w, http.StatusOK, withPolling(d))
}

// handleUpdatePolling — PUT /devices/{id}/polling
// {"profile": "auto"|"light", "class_override": "C1"|"C2"|"C3"|null}
// Omitted keys are left alone; class_override null or "" clears the override.
// The request is validated as a whole before anything is written: a 400 for
// one key never leaves the other one changed.
func (s *Server) handleUpdatePolling(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	d, ok := s.loadDevice(w, id)
	if !ok {
		return
	}
	var raw map[string]json.RawMessage
	if err := decodeJSON(r, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	var profile *devclass.Profile
	if v, present := raw["profile"]; present {
		var p string
		if err := json.Unmarshal(v, &p); err != nil {
			writeError(w, http.StatusBadRequest, "profile must be a string")
			return
		}
		parsed, ok := devclass.ParseProfile(p)
		if !ok {
			writeError(w, http.StatusBadRequest, `profile must be "auto" or "light"`)
			return
		}
		profile = &parsed
	}

	var override *devclass.Class // "" = clear
	if v, present := raw["class_override"]; present {
		var c devclass.Class
		if string(v) != "null" {
			var str string
			if err := json.Unmarshal(v, &str); err != nil {
				writeError(w, http.StatusBadRequest, "class_override must be a string or null")
				return
			}
			if str != "" {
				parsed, ok := devclass.ParseClass(str)
				if !ok || parsed == devclass.C0Unknown {
					writeError(w, http.StatusBadRequest, `class_override must be "C1", "C2", "C3" or null`)
					return
				}
				c = parsed
			}
		}
		if c != "" {
			probed := d.ProbedClass()
			if _, clamped := devclass.Effective(probed, c); clamped {
				writeError(w, http.StatusBadRequest, fmt.Sprintf(
					"class override %s is more capable than the probed class %s; an override may only make polling gentler",
					c, probed.Budgeted()))
				return
			}
		}
		override = &c
	}

	if profile != nil && *profile != d.Profile() {
		if err := queries.SetDevicePollProfile(s.db, id, *profile); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update polling profile")
			return
		}
		s.audit(r, "poll_profile_changed", id, fmt.Sprintf("%s → %s", d.Profile(), *profile))
	}
	if override != nil && string(*override) != d.ClassOverride {
		if err := queries.SetDeviceClassOverride(s.db, id, *override); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to update class override")
			return
		}
		s.audit(r, "class_override_changed", id, describeOverrideChange(d.ClassOverride, string(*override)))
	}

	if d, ok = s.loadDevice(w, id); !ok {
		return
	}
	writeJSON(w, http.StatusOK, withPolling(d))
}

func describeOverrideChange(from, to string) string {
	if from == "" {
		from = "none"
	}
	if to == "" {
		return from + " → cleared"
	}
	return from + " → " + to
}

// handleDeviceAudit — GET /devices/{id}/audit?limit=50
func (s *Server) handleDeviceAudit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.loadDevice(w, id); !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	entries, err := queries.ListAuditLog(s.db, id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list audit log")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
