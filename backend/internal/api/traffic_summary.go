package api

import (
	"net/http"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

type deviceTrafficSummary struct {
	DeviceID string `json:"device_id"`
	RxBps    int64  `json:"rx_bps"`
	TxBps    int64  `json:"tx_bps"`
}

// handleGetTrafficSummary returns a one-shot aggregate traffic snapshot for all online devices.
func (s *Server) handleGetTrafficSummary(w http.ResponseWriter, r *http.Request) {
	devices, err := queries.ListDevices(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list devices")
		return
	}

	var results []deviceTrafficSummary
	for _, dev := range devices {
		if dev.Status != "online" {
			continue
		}
		client := s.pool.Get(dev.ID)
		if client == nil {
			continue
		}

		// Get traffic on the first bridge or main interface
		ifaces, _ := queries.ListInterfacesByDevice(s.db, dev.ID)
		ifaceName := ""
		// Prefer bridge, then ether1
		for _, i := range ifaces {
			if i.Type == "bridge" || i.Name == "bridge" || i.Name == "bridge1" {
				ifaceName = i.Name
				break
			}
		}
		if ifaceName == "" {
			for _, i := range ifaces {
				if i.Name == "ether1" {
					ifaceName = i.Name
					break
				}
			}
		}
		if ifaceName == "" && len(ifaces) > 0 {
			ifaceName = ifaces[0].Name
		}
		if ifaceName == "" {
			continue
		}

		func() {
			defer func() { recover() }()
			data, err := routeros.GetTrafficSnapshot(client, ifaceName)
			if err != nil {
				return
			}
			results = append(results, deviceTrafficSummary{
				DeviceID: dev.ID,
				RxBps:    data.RxBitsPerSec,
				TxBps:    data.TxBitsPerSec,
			})
		}()
	}

	if results == nil {
		results = []deviceTrafficSummary{}
	}
	writeJSON(w, http.StatusOK, results)
}
