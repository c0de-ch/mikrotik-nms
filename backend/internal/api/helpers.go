package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// writeDialPolicyError maps a refused device dial to its HTTP status — 503
// while polling is off on this instance (MIKROTIK_NMS_POLLERS=off), 403 when
// the target lies outside MIKROTIK_NMS_ALLOWED_TARGET_CIDRS — and reports
// whether err was one of those. Anything else (nil included) is the caller's
// to handle, so handlers can write `if writeDialPolicyError(w, err) { return }`
// ahead of their usual error path.
func writeDialPolicyError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, routeros.ErrPollersOff):
		writeError(w, http.StatusServiceUnavailable,
			"polling is off on this instance (MIKROTIK_NMS_POLLERS=off) — devices are not contacted")
		return true
	case errors.Is(err, routeros.ErrTargetNotAllowed):
		writeError(w, http.StatusForbidden, err.Error())
		return true
	}
	return false
}

// writePollingPaused refuses an on-demand device action while an admin pause
// of the device's polling is in force — 409 "polling paused until <RFC 3339
// time>" — and reports whether it did, so handlers can write
// `if s.writePollingPaused(w, id) { return }` ahead of reaching the device.
// A pause means only the liveness ping reaches the device; run-now actions
// honour it like the scheduled pollers (resume the device to act on it). An
// action on several devices (firmware) is refused when any of them is
// paused, naming that one. Work already running is not interrupted.
func (s *Server) writePollingPaused(w http.ResponseWriter, deviceIDs ...string) bool {
	now := time.Now()
	for _, id := range deviceIDs {
		until, paused := poller.PausedUntil(s.db, id, now)
		if !paused {
			continue
		}
		msg := "polling paused until " + until.Format(time.RFC3339)
		if len(deviceIDs) > 1 {
			name := id
			if d, err := queries.GetDevice(s.db, id); err == nil {
				name = deviceDisplayName(*d)
			}
			msg = name + ": " + msg
		}
		writeError(w, http.StatusConflict, msg)
		return true
	}
	return false
}

// cadenceSource is the schedule staleness rules are judged by: the poller
// manager's when wired, else one built from the database and config (tests,
// tools) — the same class cadences and floors, without the manager's floor
// cache.
func (s *Server) cadenceSource() poller.CadenceSource {
	if s.cadence != nil {
		return s.cadence
	}
	return poller.NewCadence(s.db, s.cfg)
}

// pollersMode is the instance's MIKROTIK_NMS_POLLERS value ("local" or
// "off"); a Server built without a config (tests) reports off.
func (s *Server) pollersMode() string {
	if s.cfg == nil {
		return config.PollersOff
	}
	return s.cfg.PollersMode()
}
