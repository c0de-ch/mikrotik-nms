package api

import (
	"crypto/tls"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	ros "github.com/go-routeros/routeros/v3"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	rosutil "github.com/mikrotik-nms/backend/internal/routeros"
)

type createDeviceRequest struct {
	Address  string `json:"address"`
	Identity string `json:"identity"`
	Username string `json:"username"`
	Password string `json:"password"`
	UseTLS   bool   `json:"use_tls"`
	APIPort  int    `json:"api_port"`
	Tags     string `json:"tags"`
	Notes    string `json:"notes"`
}

type updateDeviceRequest struct {
	Address  string `json:"address"`
	Identity string `json:"identity"`
	Username string `json:"username"`
	Password string `json:"password"`
	UseTLS   bool   `json:"use_tls"`
	APIPort  int    `json:"api_port"`
	Tags     string `json:"tags"`
	Notes    string `json:"notes"`
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := queries.ListDevices(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list devices")
		return
	}
	if devices == nil {
		devices = []queries.Device{}
	}
	writeJSON(w, http.StatusOK, devices)
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	device, err := queries.GetDevice(s.db, id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "device not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get device")
		return
	}
	writeJSON(w, http.StatusOK, device)
}

func (s *Server) handleCreateDevice(w http.ResponseWriter, r *http.Request) {
	var req createDeviceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Address == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}
	if req.APIPort == 0 {
		req.APIPort = s.cfg.DefaultROSPort
	}
	if req.Username == "" {
		req.Username = s.cfg.DefaultROSUser
	}
	password := req.Password
	if password == "" {
		password = s.cfg.DefaultROSPass
	}

	// Test connection before adding
	addr := fmt.Sprintf("%s:%d", req.Address, req.APIPort)
	var testClient *ros.Client
	var testErr error
	if req.UseTLS {
		testClient, testErr = ros.DialTLS(addr, req.Username, password, &tls.Config{InsecureSkipVerify: !s.cfg.ROSTLSVerify}) //nolint:gosec // opt-in verification via MIKROTIK_NMS_ROS_TLS_VERIFY
	} else {
		testClient, testErr = ros.Dial(addr, req.Username, password)
	}
	if testErr != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot connect to %s: %v", req.Address, testErr))
		return
	}

	// Get device identity from the device itself
	res, err := rosutil.GetSystemResource(testClient)
	if err == nil && req.Identity == "" {
		// Try to get identity
		if reply, err2 := rosutil.RunCommand(testClient, "/system/identity/print"); err2 == nil && len(reply.Re) > 0 {
			if name, ok := reply.Re[0].Map["name"]; ok {
				req.Identity = name
			}
		}
	}
	testClient.Close()

	device := &queries.Device{
		ID:          uuid.NewString(),
		Address:     req.Address,
		Identity:    req.Identity,
		Username:    req.Username,
		PasswordEnc: password,
		UseTLS:      req.UseTLS,
		APIPort:     req.APIPort,
		Tags:        req.Tags,
		Notes:       req.Notes,
		Status:      "online",
	}
	if device.Tags == "" {
		device.Tags = "[]"
	}
	// Pre-fill device info if we got it
	if res != nil {
		device.Platform = res.Platform
		device.Board = res.Board
		device.ROSVersion = res.Version
		device.Architecture = res.Architecture
	}

	if err := queries.CreateDevice(s.db, device); err != nil {
		writeError(w, http.StatusConflict, fmt.Sprintf("device with address %s already exists", req.Address))
		return
	}

	writeJSON(w, http.StatusCreated, device)
}

func (s *Server) handleUpdateDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	existing, err := queries.GetDevice(s.db, id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "device not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get device")
		return
	}

	var req updateDeviceRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Address != "" {
		existing.Address = req.Address
	}
	if req.Identity != "" {
		existing.Identity = req.Identity
	}
	if req.Username != "" {
		existing.Username = req.Username
	}
	if req.Password != "" {
		existing.PasswordEnc = req.Password // encrypted at rest by queries.UpdateDevice
	}
	existing.UseTLS = req.UseTLS
	if req.APIPort > 0 {
		existing.APIPort = req.APIPort
	}
	if req.Tags != "" {
		existing.Tags = req.Tags
	}
	if req.Notes != "" {
		existing.Notes = req.Notes
	}

	if err := queries.UpdateDevice(s.db, existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update device")
		return
	}

	writeJSON(w, http.StatusOK, existing)
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := queries.DeleteDevice(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "device not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete device")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListInterfaces(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ifaces, err := queries.ListInterfacesByDevice(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list interfaces")
		return
	}
	if ifaces == nil {
		ifaces = []queries.Interface{}
	}
	writeJSON(w, http.StatusOK, ifaces)
}

func (s *Server) handleListNeighbors(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	neighbors, err := queries.ListNeighborsByDevice(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list neighbors")
		return
	}
	if neighbors == nil {
		neighbors = []queries.Neighbor{}
	}
	writeJSON(w, http.StatusOK, neighbors)
}
