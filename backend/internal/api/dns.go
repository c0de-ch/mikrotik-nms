package api

import (
	"database/sql"
	"net/http"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func (s *Server) handleListDNSServers(w http.ResponseWriter, r *http.Request) {
	servers, err := queries.ListDNSServers(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list DNS servers")
		return
	}
	if servers == nil {
		servers = []queries.DNSServer{}
	}
	writeJSON(w, http.StatusOK, servers)
}

func (s *Server) handleCreateDNSServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Port    int    `json:"port"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Address == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}
	if req.Port == 0 {
		req.Port = 53
	}

	srv := &queries.DNSServer{
		ID:      uuid.NewString(),
		Name:    req.Name,
		Address: req.Address,
		Port:    req.Port,
		Enabled: true,
	}

	if err := queries.CreateDNSServer(s.db, srv); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create DNS server")
		return
	}

	writeJSON(w, http.StatusCreated, srv)
}

func (s *Server) handleUpdateDNSServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name    string `json:"name"`
		Address string `json:"address"`
		Port    int    `json:"port"`
		Enabled *bool  `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	srv := &queries.DNSServer{ID: id, Name: req.Name, Address: req.Address, Port: req.Port}
	if req.Enabled != nil {
		srv.Enabled = *req.Enabled
	} else {
		srv.Enabled = true
	}
	if srv.Port == 0 {
		srv.Port = 53
	}

	if err := queries.UpdateDNSServer(s.db, srv); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update DNS server")
		return
	}

	writeJSON(w, http.StatusOK, srv)
}

func (s *Server) handleDeleteDNSServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := queries.DeleteDNSServer(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "DNS server not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete DNS server")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleResolveDNS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IPs []string `json:"ips"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	results := s.resolver.ResolveMany(req.IPs)
	writeJSON(w, http.StatusOK, results)
}
