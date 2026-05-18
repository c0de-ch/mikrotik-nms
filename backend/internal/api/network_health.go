package api

import (
	"net/http"
	"strconv"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

type bridgeWithPorts struct {
	queries.BridgeStatus
	DeviceName string                      `json:"device_name"`
	Ports      []queries.BridgePortStatus  `json:"ports"`
}

type networkHealthResponse struct {
	Bridges    []bridgeWithPorts        `json:"bridges"`
	Events     []enrichedLoopEvent      `json:"events"`
	PortStates []enrichedInterfaceState `json:"port_states"`
}

type enrichedInterfaceState struct {
	queries.InterfaceState
	DeviceName string `json:"device_name"`
}

type enrichedLoopEvent struct {
	queries.LoopEvent
	DeviceName string `json:"device_name"`
}

func (s *Server) handleNetworkHealth(w http.ResponseWriter, r *http.Request) {
	bridges, err := queries.ListBridgeStatus(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list bridges")
		return
	}
	ports, err := queries.ListBridgePortStatus(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list ports")
		return
	}
	events, err := queries.ListLoopEvents(s.db, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list loop events")
		return
	}
	portStates, err := queries.ListInterfaceStates(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list interface states")
		return
	}

	deviceNames := s.deviceNameMap()

	portsByBridge := make(map[string][]queries.BridgePortStatus)
	for _, p := range ports {
		key := p.DeviceID + "|" + p.BridgeName
		portsByBridge[key] = append(portsByBridge[key], p)
	}

	enrichedBridges := make([]bridgeWithPorts, 0, len(bridges))
	for _, b := range bridges {
		enrichedBridges = append(enrichedBridges, bridgeWithPorts{
			BridgeStatus: b,
			DeviceName:   deviceNames[b.DeviceID],
			Ports:        portsByBridge[b.DeviceID+"|"+b.BridgeName],
		})
	}

	enrichedEvents := make([]enrichedLoopEvent, 0, len(events))
	for _, e := range events {
		enrichedEvents = append(enrichedEvents, enrichedLoopEvent{
			LoopEvent:  e,
			DeviceName: deviceNames[e.DeviceID],
		})
	}

	enrichedPorts := make([]enrichedInterfaceState, 0, len(portStates))
	for _, ps := range portStates {
		enrichedPorts = append(enrichedPorts, enrichedInterfaceState{
			InterfaceState: ps,
			DeviceName:     deviceNames[ps.DeviceID],
		})
	}

	writeJSON(w, http.StatusOK, networkHealthResponse{
		Bridges:    enrichedBridges,
		Events:     enrichedEvents,
		PortStates: enrichedPorts,
	})
}

func (s *Server) handleNetworkHealthEvents(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if l, err := strconv.Atoi(v); err == nil && l > 0 && l <= 5000 {
			limit = l
		}
	}
	events, err := queries.ListLoopEvents(s.db, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list loop events")
		return
	}

	deviceNames := s.deviceNameMap()
	out := make([]enrichedLoopEvent, 0, len(events))
	for _, e := range events {
		out = append(out, enrichedLoopEvent{LoopEvent: e, DeviceName: deviceNames[e.DeviceID]})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deviceNameMap() map[string]string {
	devices, _ := queries.ListDevices(s.db)
	names := make(map[string]string, len(devices))
	for _, d := range devices {
		name := d.Identity
		if name == "" {
			name = d.Address
		}
		names[d.ID] = name
	}
	return names
}
