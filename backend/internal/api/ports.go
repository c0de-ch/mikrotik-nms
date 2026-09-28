package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/topology"
)

type portInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Running  bool   `json:"running"`
	Disabled bool   `json:"disabled"`
	Comment  string `json:"comment"`
	RxBps    int64  `json:"rx_bps"`
	TxBps    int64  `json:"tx_bps"`
}

// handleGetDevicePorts returns each physical port with its running/disabled
// state and a one-shot live rx/tx sample (batched into a single monitor-traffic
// call). Powers the switch port-grid on the map.
func (s *Server) handleGetDevicePorts(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	dev, err := queries.GetDevice(s.db, id)
	if err != nil || dev == nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	ifaces, err := queries.ListInterfacesByDevice(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list interfaces")
		return
	}

	ports := make([]portInfo, 0, len(ifaces))
	names := make([]string, 0, len(ifaces))
	for _, i := range ifaces {
		// Faceplate ports only (ethernet, SFP, QSFP, combo); virtual
		// interfaces (bridge, vlan, vpn, wireless, …) are dropped.
		if !topology.IsPhysicalPort(i.Name, i.Type) {
			continue
		}
		ports = append(ports, portInfo{
			Name: i.Name, Type: i.Type, Running: i.Running, Disabled: i.Disabled, Comment: i.Comment,
		})
		names = append(names, i.Name)
	}

	if dev.Status == "online" {
		if client := s.pool.GetLive(id); client != nil {
			if tr, err := routeros.GetPortTraffic(client, names); err == nil {
				for idx := range ports {
					if t, ok := tr[ports[idx].Name]; ok {
						ports[idx].RxBps = t.RxBitsPerSec
						ports[idx].TxBps = t.TxBitsPerSec
					}
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, ports)
}
