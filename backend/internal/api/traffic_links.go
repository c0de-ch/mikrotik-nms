package api

import (
	"net/http"
	"time"

	"github.com/mikrotik-nms/backend/internal/poller"
)

// deviceReadTimes is implemented by the port-stats collector: when each
// device's counters were last read (the snapshot itself carries one fleet
// cycle time). Test fakes may leave it out.
type deviceReadTimes interface {
	DeviceSampleTimes() map[string]time.Time
}

// liveSnapshot returns the per-device read times and then the snapshot — in
// that order, so a cycle finishing in between can only make a time look
// older than its rates, never fresher. Times are nil when the source does
// not provide them.
func (s *Server) liveSnapshot() (map[string]time.Time, poller.PortSnapshot) {
	var times map[string]time.Time
	if src, ok := s.portStats.(deviceReadTimes); ok {
		times = src.DeviceSampleTimes()
	}
	return times, s.portSnapshot()
}

// handleGetTrafficLinks returns per-link throughput for the network map's
// first paint, derived from the port-stats snapshot (no device calls, so an
// open map costs a weak switch nothing). The continuous feed is the
// "topology.traffic" WS topic, built the same way once per counter cycle.
func (s *Server) handleGetTrafficLinks(w http.ResponseWriter, r *http.Request) {
	times, snap := s.liveSnapshot()
	payload, err := poller.LoadLinkTraffic(s.db, snap, times, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load links")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
