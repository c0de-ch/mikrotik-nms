package api

import (
	"net/http"
	"time"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/poller"
)

// liveLimitsResponse tells the UI, before it subscribes, what a live stream
// of this device may do for the requesting user: the class caps, and whether
// a stream would be granted at all (role, pause, polling mode). The interface
// and lease ceilings depend on other viewers and are only decided on
// subscribe (subscribe.ack / subscribe.error).
type liveLimitsResponse struct {
	DeviceID string `json:"device_id"`
	poller.LiveLimits
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`  // refusal code when !allowed
	Message string `json:"message,omitempty"` // what to show instead
}

// handleDeviceLiveLimits — GET /devices/{id}/live-limits. Database only.
func (s *Server) handleDeviceLiveLimits(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r.PathValue("id"))
	if !ok {
		return
	}
	admin := false
	if u := auth.UserFromContext(r.Context()); u != nil {
		admin = u.Role == "admin"
	}
	interval := time.Duration(s.portSnapshot().IntervalSeconds) * time.Second
	pollingOn := s.pollersMode() == config.PollersLocal
	limits, code := poller.LiveAccessFor(d, admin, pollingOn, time.Now(), interval)
	resp := liveLimitsResponse{DeviceID: d.ID, LiveLimits: limits, Allowed: code == "", Reason: code}
	if code != "" {
		resp.Message = poller.LiveRefusalMessage(code, limits)
	}
	writeJSON(w, http.StatusOK, resp)
}
