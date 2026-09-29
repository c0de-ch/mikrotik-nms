package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// purgeRequest selects which history tables to purge and how far back.
// OlderThanDays=0 (or omitted) means "delete all rows from the chosen tables".
type purgeRequest struct {
	WiFi          bool `json:"wifi"`            // wifi_history
	Clients       bool `json:"clients"`         // client_history
	NetworkHealth bool `json:"network_health"`  // loop_events
	Traffic       bool `json:"traffic"`         // traffic_samples + port_stats_1m + port_stats_1h
	Flows         bool `json:"flows"`           // flow_1m + flow_1h (+ their meta rows)
	OlderThanDays int  `json:"older_than_days"` // 0 = everything
}

type purgeResponse struct {
	Deleted map[string]int64 `json:"deleted"`
}

// handlePurgeHistory is an admin-only endpoint that wipes historical tables
// according to the request. Current-state tables (mac_lookup, interface_state,
// bridge_status, bridge_port_status, app_settings, devices, users) are NEVER
// touched — they get re-populated by polling and would be confusing/wrong to
// delete from here.
func (s *Server) handlePurgeHistory(w http.ResponseWriter, r *http.Request) {
	var req purgeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !req.WiFi && !req.Clients && !req.NetworkHealth && !req.Traffic && !req.Flows {
		writeError(w, http.StatusBadRequest, "no targets selected")
		return
	}
	if req.OlderThanDays < 0 {
		writeError(w, http.StatusBadRequest, "older_than_days must be >= 0")
		return
	}
	// A full traffic / flow purge deletes millions of rows in bounded
	// chunks and can outlast the server's 15 s write timeout: extend this
	// response's deadline (ErrNotSupported under test recorders is fine).
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))

	// targets maps the API toggle to the underlying SQL table + the column
	// used to filter by age. All four tables have a column whose default is
	// CURRENT_TIMESTAMP, but the names differ — keep them explicit.
	targets := []struct {
		enabled bool
		table   string
		column  string
	}{
		{req.WiFi, "wifi_history", "recorded_at"},
		{req.Clients, "client_history", "recorded_at"},
		{req.NetworkHealth, "loop_events", "recorded_at"},
		{req.Traffic, "traffic_samples", "collected_at"},
	}

	deleted := make(map[string]int64, 6)
	for _, t := range targets {
		if !t.enabled {
			continue
		}
		n, err := purgeTable(s.db, t.table, t.column, req.OlderThanDays)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("purge %s: %v", t.table, err))
			return
		}
		deleted[t.table] = n
	}
	if req.Traffic {
		// The per-port traffic analytics history (1-minute buckets and their
		// hourly rollup). Deleted in bounded chunks: a full purge can be
		// millions of rows, and one DELETE would lock out every other writer.
		for _, table := range []queries.PortStatsTable{queries.PortStats1m, queries.PortStats1h} {
			n, err := queries.DeleteOldPortStats(s.db, table, purgeCutoff(req.OlderThanDays))
			if err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("purge %s: %v", table, err))
				return
			}
			deleted[string(table)] = n
		}
	}
	if req.Flows {
		// Flow export history (per-minute conversations and the hourly
		// rollup, each with its covered-bucket meta rows; the counts include
		// the meta rows). Chunked like port stats. Exporter / point
		// configuration is kept.
		for _, table := range []queries.FlowTable{queries.Flows1m, queries.Flows1h} {
			n, err := queries.DeleteOldFlows(s.db, table, purgeCutoff(req.OlderThanDays))
			if err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("purge %s: %v", table, err))
				return
			}
			deleted[string(table)] = n
		}
		s.flowCache.clear()
	}

	writeJSON(w, http.StatusOK, purgeResponse{Deleted: deleted})
}

// purgeCutoff is the bucket cutoff for the port-stats and flow tables: everything
// older than olderThanDays, or every row when it is 0.
func purgeCutoff(olderThanDays int) time.Time {
	if olderThanDays == 0 {
		return time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	}
	return time.Now().UTC().AddDate(0, 0, -olderThanDays)
}

func purgeTable(db *sql.DB, table, column string, olderThanDays int) (int64, error) {
	// Table/column are not user-controlled (they come from a fixed allowlist
	// above), so it's safe to interpolate. No user data hits this SQL string.
	var res sql.Result
	var err error
	if olderThanDays == 0 {
		res, err = db.Exec("DELETE FROM " + table)
	} else {
		cutoff := time.Now().UTC().AddDate(0, 0, -olderThanDays)
		res, err = db.Exec("DELETE FROM "+table+" WHERE "+column+" < ?", cutoff)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
