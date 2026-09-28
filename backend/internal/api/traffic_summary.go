package api

import (
	"net/http"

	"github.com/mikrotik-nms/backend/internal/poller"
)

type deviceTrafficSummary struct {
	DeviceID string `json:"device_id"`
	RxBps    int64  `json:"rx_bps"`
	TxBps    int64  `json:"tx_bps"`
}

// handleGetTrafficSummary returns one rx/tx figure per device, served from the
// port-stats collector's snapshot (no device calls). Each device reports its
// main interface: the first bridge, else ether1, else its first interface.
func (s *Server) handleGetTrafficSummary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, summarizeSnapshot(s.portSnapshot()))
}

// summarizeSnapshot picks each device's main interface from a snapshot whose
// ports are sorted by device, then interface name. Devices keep snapshot order.
func summarizeSnapshot(snap poller.PortSnapshot) []deviceTrafficSummary {
	results := []deviceTrafficSummary{}
	for i := 0; i < len(snap.Ports); {
		j := i
		for j < len(snap.Ports) && snap.Ports[j].DeviceID == snap.Ports[i].DeviceID {
			j++
		}
		p := mainInterface(snap.Ports[i:j])
		results = append(results, deviceTrafficSummary{DeviceID: p.DeviceID, RxBps: p.RxBps, TxBps: p.TxBps})
		i = j
	}
	return results
}

// mainInterface keeps the old summary semantics: prefer a bridge, then ether1,
// then the first interface by name. ports is non-empty and sorted by name.
func mainInterface(ports []poller.PortRate) poller.PortRate {
	for _, p := range ports {
		if p.Type == "bridge" || p.Iface == "bridge" || p.Iface == "bridge1" {
			return p
		}
	}
	for _, p := range ports {
		if p.Iface == "ether1" {
			return p
		}
	}
	return ports[0]
}
