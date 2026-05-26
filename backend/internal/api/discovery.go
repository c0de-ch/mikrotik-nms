package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

func (s *Server) handleDiscoverDevices(w http.ResponseWriter, r *http.Request) {
	durationSec := 10
	if v := r.URL.Query().Get("duration"); v != "" {
		if d, err := strconv.Atoi(v); err == nil && d >= 1 && d <= 30 {
			durationSec = d
		}
	}

	devices, err := routeros.ScanMNDP(time.Duration(durationSec) * time.Second)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "discovery failed: "+err.Error())
		return
	}
	if devices == nil {
		devices = []routeros.DiscoveredDevice{}
	}

	writeJSON(w, http.StatusOK, devices)
}

// DeepDiscoveredDevice is a single result of the deep scan: a device found
// either as an unmanaged neighbor of a managed device, via a subnet port-scan,
// or both.
type DeepDiscoveredDevice struct {
	Address   string `json:"address"`
	MAC       string `json:"mac"`
	Identity  string `json:"identity"`
	Platform  string `json:"platform"`
	Board     string `json:"board"`
	Version   string `json:"version"`
	Source    string `json:"source"` // "neighbor" | "port-scan" | "both"
	OpenPorts []int  `json:"open_ports"`
	SeenFrom  string `json:"seen_from"` // identity of the managed device that saw it as neighbor
}

// deepScanPorts are the RouterOS API + WinBox ports probed by the subnet sweep.
var deepScanPorts = []int{8728, 8729, 8291}

// handleDeepScan performs deep device discovery. It merges two sources:
//   - unmanaged neighbors (devices seen via /ip/neighbor by a managed device
//     but not themselves managed)
//   - an optional subnet port-scan of the supplied cidr
//
// Results are deduplicated by address; a device present in both sources is
// reported with Source "both", keeping the richer neighbor metadata plus the
// scanned open ports.
func (s *Server) handleDeepScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	merged := make(map[string]*DeepDiscoveredDevice)

	// 1. Unmanaged neighbors from the DB.
	neighbors, err := queries.ListUnmanagedNeighbors(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load unmanaged neighbors: "+err.Error())
		return
	}
	for _, n := range neighbors {
		merged[n.NeighborAddress] = &DeepDiscoveredDevice{
			Address:  n.NeighborAddress,
			MAC:      n.NeighborMAC,
			Identity: n.NeighborIdentity,
			Platform: n.NeighborPlatform,
			Board:    n.NeighborBoard,
			Version:  n.NeighborVersion,
			Source:   "neighbor",
			SeenFrom: n.SeenFromIdentity,
		}
	}

	// 2. Optional subnet port-scan.
	cidr := r.URL.Query().Get("cidr")
	if cidr != "" {
		scanned, err := routeros.ScanSubnet(ctx, cidr, deepScanPorts, 800*time.Millisecond, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "subnet scan failed: "+err.Error())
			return
		}
		for _, res := range scanned {
			if existing, ok := merged[res.Address]; ok {
				existing.Source = "both"
				existing.OpenPorts = res.OpenPorts
			} else {
				merged[res.Address] = &DeepDiscoveredDevice{
					Address:   res.Address,
					Source:    "port-scan",
					OpenPorts: res.OpenPorts,
				}
			}
		}
	}

	out := make([]DeepDiscoveredDevice, 0, len(merged))
	for _, d := range merged {
		out = append(out, *d)
	}

	writeJSON(w, http.StatusOK, out)
}
