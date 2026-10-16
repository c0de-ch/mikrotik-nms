package api

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
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

// devicePortsResponse is one device's faceplate with the snapshot's rates.
// sampled_at is null while the snapshot has none of the device's interfaces
// (offline, or not polled yet): the rates are then zero, not measured.
type devicePortsResponse struct {
	DeviceID string `json:"device_id"`
	poller.SnapshotMeta
	Ports []portInfo `json:"ports"`
}

// handleGetDevicePorts returns each physical port with its running/disabled
// state and its rx/tx rate from the port-stats snapshot — never a device
// call, so the map's port grid costs a weak switch nothing however many tabs
// show it. The rates are as fresh as the counter cycle (SnapshotMeta).
func (s *Server) handleGetDevicePorts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := queries.GetDevice(s.db, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "device not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to get device")
		}
		return
	}
	ifaces, err := queries.ListInterfacesByDevice(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list interfaces")
		return
	}
	snap := s.portSnapshot()
	writeJSON(w, http.StatusOK, devicePorts(id, ifaces, snap, time.Now()))
}

// devicePorts joins the interface inventory with the snapshot. The snapshot
// wins for running/disabled/comment (it is seconds old; the inventory is
// refreshed hourly), and a faceplate port the inventory has not caught up
// with yet is listed from the snapshot alone, after the inventory's ports.
func devicePorts(deviceID string, ifaces []queries.Interface, snap poller.PortSnapshot, now time.Time) devicePortsResponse {
	live := make(map[string]poller.PortRate)
	for _, p := range snap.Ports {
		if p.DeviceID == deviceID {
			live[p.Iface] = p
		}
	}

	resp := devicePortsResponse{DeviceID: deviceID, SnapshotMeta: poller.NewSnapshotMeta(snap, now), Ports: []portInfo{}}
	if len(live) == 0 {
		resp.SampledAt, resp.AgeSeconds = nil, nil
	}

	listed := make(map[string]bool, len(ifaces))
	for _, i := range ifaces {
		// Faceplate ports only (ethernet, SFP, QSFP, combo); virtual
		// interfaces (bridge, vlan, vpn, wireless, …) are dropped.
		if !topology.IsPhysicalPort(i.Name, i.Type) {
			continue
		}
		listed[i.Name] = true
		p := portInfo{Name: i.Name, Type: i.Type, Running: i.Running, Disabled: i.Disabled, Comment: i.Comment}
		if r, ok := live[i.Name]; ok {
			p.Running, p.Disabled, p.Comment = r.Running, r.Disabled, r.Comment
			p.RxBps, p.TxBps = r.RxBps, r.TxBps
		}
		resp.Ports = append(resp.Ports, p)
	}

	var extra []portInfo
	for name, r := range live {
		if listed[name] || !topology.IsPhysicalPort(name, r.Type) {
			continue
		}
		extra = append(extra, portInfo{Name: name, Type: r.Type, Running: r.Running, Disabled: r.Disabled,
			Comment: r.Comment, RxBps: r.RxBps, TxBps: r.TxBps})
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].Name < extra[j].Name })
	resp.Ports = append(resp.Ports, extra...)
	return resp
}
