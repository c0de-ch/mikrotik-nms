package api

import (
	"net/http"
	"strconv"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func (s *Server) handleMACLookup(w http.ResponseWriter, r *http.Request) {
	lookups, err := queries.GetAllMACLookups(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get MAC lookups")
		return
	}
	writeJSON(w, http.StatusOK, lookups)
}

func (s *Server) handleWifiCurrent(w http.ResponseWriter, r *http.Request) {
	clients, err := queries.GetWifiClientsCurrentAP(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get wifi clients")
		return
	}
	if clients == nil {
		clients = []queries.WifiHistoryEntry{}
	}
	writeJSON(w, http.StatusOK, clients)
}

func (s *Server) handleWifiHistory(w http.ResponseWriter, r *http.Request) {
	mac := r.URL.Query().Get("mac")
	ap := r.URL.Query().Get("ap")
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if l, err := strconv.Atoi(v); err == nil && l > 0 && l <= 5000 {
			limit = l
		}
	}

	var entries []queries.WifiHistoryEntry
	var err error

	if mac != "" {
		entries, err = queries.GetWifiHistoryByMAC(s.db, mac, limit)
	} else if ap != "" {
		entries, err = queries.GetWifiHistoryByAP(s.db, ap, limit)
	} else {
		entries, err = queries.GetWifiHistoryRecent(s.db, limit)
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get wifi history")
		return
	}
	if entries == nil {
		entries = []queries.WifiHistoryEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}
