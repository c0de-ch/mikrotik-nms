package api

import (
	"net/http"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

type enrichedBridgeVLAN struct {
	queries.BridgeVLAN
	DeviceName string `json:"device_name"`
}

func (s *Server) handleListVLANs(w http.ResponseWriter, r *http.Request) {
	vlans, err := queries.ListBridgeVLANs(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list vlans")
		return
	}

	deviceNames := s.deviceNameMap()
	out := make([]enrichedBridgeVLAN, 0, len(vlans))
	for _, v := range vlans {
		out = append(out, enrichedBridgeVLAN{
			BridgeVLAN: v,
			DeviceName: deviceNames[v.DeviceID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListVLANLabels(w http.ResponseWriter, r *http.Request) {
	labels, err := queries.ListVLANLabels(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list vlan labels")
		return
	}
	if labels == nil {
		labels = []queries.VLANLabel{}
	}
	writeJSON(w, http.StatusOK, labels)
}

func (s *Server) handleUpdateVLANLabel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VLANID  int    `json:"vlan_id"`
		Name    string `json:"name"`
		Purpose string `json:"purpose"`
		Color   string `json:"color"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.VLANID < 0 {
		writeError(w, http.StatusBadRequest, "vlan_id is required")
		return
	}

	label := &queries.VLANLabel{
		VLANID:  body.VLANID,
		Name:    body.Name,
		Purpose: body.Purpose,
		Color:   body.Color,
	}
	if err := queries.UpsertVLANLabel(s.db, label); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update vlan label")
		return
	}
	writeJSON(w, http.StatusOK, label)
}
