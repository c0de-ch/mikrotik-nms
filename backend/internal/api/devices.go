package api

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
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
	// Every device carries its role (router, weak-router, switch, ap-cpe,
	// unknown) with the reason, derived from stored facts only.
	roles, devices, err := s.deviceRoles(time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list devices")
		return
	}
	if devices == nil {
		devices = []queries.Device{}
	}
	for i := range devices {
		attachRole(&devices[i], roles[devices[i].ID])
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
	// The role is a nicety here: a failure to derive it must not hide the
	// device.
	if roles, _, err := s.deviceRoles(time.Now()); err == nil {
		if role, ok := roles[device.ID]; ok {
			attachRole(device, role)
		}
	}
	writeJSON(w, http.StatusOK, withPolling(device))
}

// withPolling attaches how the pollers treat the device right now — class,
// profile, pause and each scheduled read's next run — for the device page.
func withPolling(d *queries.Device) *queries.Device {
	d.Polling = poller.Schedule().Polling(*d, time.Now())
	return d
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
	// No default credentials: a device is added with its own username and
	// password, typed by the admin.
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	password := req.Password

	// Test connection before adding, on a one-shot connection that goes
	// through the dial policy like every other device dial (polling off, or
	// an address outside the allowed subnets, is refused before any login).
	testClient, testErr := rosutil.DialOnce(req.Address, req.APIPort, req.Username, password, req.UseTLS, s.cfg.ROSTLSVerify)
	if testErr != nil {
		if writeDialPolicyError(w, testErr) {
			return
		}
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
		if errors.Is(err, queries.ErrSecretStorage) {
			// Never stored in plaintext instead: the device is not added.
			log.Printf("devices: add %s: %v", req.Address, err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeError(w, http.StatusConflict, fmt.Sprintf("device with address %s already exists", req.Address))
		return
	}
	s.invalidateTrafficTopo()

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

	// What the pooled session was dialled with: when any of it changes, the
	// session and the remembered dial parameters must go.
	before := [4]any{existing.Address, existing.APIPort, existing.UseTLS, existing.Username}

	if req.Address != "" {
		existing.Address = req.Address
	}
	if req.Identity != "" {
		existing.Identity = req.Identity
	}
	if req.Username != "" {
		existing.Username = req.Username
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

	// The password is stored only when the edit carries a new one; an edit
	// without it leaves the stored (encrypted) value untouched.
	if req.Password != "" {
		if err := queries.SetDevicePassword(s.db, id, req.Password); err != nil {
			log.Printf("devices: store credentials of %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "failed to store the device credentials: "+err.Error())
			return
		}
		existing.PasswordEnc = req.Password
	}
	if err := queries.UpdateDevice(s.db, existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update device")
		return
	}
	// A pooled session keeps using — and redialling with — the address and
	// credentials it was opened with. Drop it so the next poll connects with
	// the new ones.
	if s.pool != nil && (req.Password != "" || before != [4]any{existing.Address, existing.APIPort, existing.UseTLS, existing.Username}) {
		s.pool.Forget(id)
	}
	s.invalidateTrafficTopo()

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
	// The pool must not keep a session (and credentials) for a device that
	// is gone.
	if s.pool != nil {
		s.pool.Forget(id)
	}
	s.invalidateTrafficTopo()
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

// handleDeviceAddresses reads the device's configured IP addresses live
// (pooled client — /ip/address, /ipv6/address and /interface/vlan are fast
// prints), annotated with VLAN ids. The UI builds its per-VLAN source
// pickers (speed test src_address) from this.
func (s *Server) handleDeviceAddresses(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	dev, err := queries.GetDevice(s.db, id)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "device not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get device")
		return
	}
	if s.writePollingPaused(w, dev.ID) {
		return
	}
	if dev.Status != "online" {
		status := dev.Status
		if status == "unknown" {
			status = "not responding" // match the UI's label for the gray state
		}
		writeError(w, http.StatusConflict, "device is "+status)
		return
	}
	client := s.pool.GetLive(id)
	if client == nil {
		writeError(w, http.StatusConflict, "no API connection to device yet — try again shortly")
		return
	}
	addrs, err := rosutil.ListAddresses(client)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to read addresses from device: "+err.Error())
		return
	}
	if addrs == nil {
		addrs = []rosutil.DeviceAddress{}
	}
	writeJSON(w, http.StatusOK, addrs)
}
