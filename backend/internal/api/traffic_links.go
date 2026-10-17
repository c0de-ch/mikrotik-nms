package api

import (
	"net/http"
	"time"

	"github.com/mikrotik-nms/backend/internal/poller"
)

// liveReadings is implemented by the port-stats collector: the snapshot with
// when each device's counters were last read (the snapshot itself carries
// one fleet cycle time), both taken under one lock. Test fakes may leave it
// out.
type liveReadings interface {
	SnapshotWithReadTimes() (poller.PortSnapshot, map[string]time.Time)
}

// liveSnapshot returns the per-device read times and the snapshot, taken
// together, so no read landing in between can make a time look fresher
// than the rates served. Times are nil when the source does not provide
// them.
func (s *Server) liveSnapshot() (map[string]time.Time, poller.PortSnapshot) {
	if src, ok := s.portStats.(liveReadings); ok {
		snap, times := src.SnapshotWithReadTimes()
		return times, snap
	}
	return nil, s.portSnapshot()
}

// handleGetTrafficLinks returns per-link throughput for the network map's
// first paint, derived from the port-stats snapshot (no device calls, so an
// open map costs a weak switch nothing). The continuous feed is the
// "topology.traffic" WS topic, built the same way once per counter cycle.
func (s *Server) handleGetTrafficLinks(w http.ResponseWriter, r *http.Request) {
	times, snap := s.liveSnapshot()
	payload, err := poller.LoadLinkTraffic(s.db, s.cadenceSource(), snap, times, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load links")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
