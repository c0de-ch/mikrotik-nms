package api

import (
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowguard"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// deviceRoles classifies every managed device from the stored facts —
// board, probed class, fresh egress rows, the interface and bridge tables —
// judging the egress rows by the pollers' own staleness rule. It reads the
// database only: asking for a role never costs a device a command.
func (s *Server) deviceRoles(now time.Time) (map[string]topology.DeviceRole, []queries.Device, error) {
	return topology.LoadDeviceRoles(s.db, poller.UplinkAge(s.cadenceSource()), now)
}

// attachRole fills a device's role, the reason, and whether flow export may
// be set up on it.
func attachRole(d *queries.Device, r topology.DeviceRole) {
	d.Role, d.RoleReason = string(r.Role), r.Reason
	d.FlowExportEligible = flowguard.Judge(deviceDisplayName(*d), r).Eligible
}
