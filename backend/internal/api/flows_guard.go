package api

import (
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/flow"
	"github.com/mikrotik-nms/backend/internal/flowguard"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// The flow-export guardrails as the API sees them: every managed device's
// role and what follows from it for flow export, and the block list. All of
// it is read from the database — the same stored facts the collector judges
// senders by — so no /flows endpoint reaches a device.

// flowGuard is the per-request view of the guardrails.
type flowGuard struct {
	roles   map[string]topology.DeviceRole
	devices []queries.Device
	byAddr  map[netip.Addr]*queries.Device
	// never maps the address of a managed device that must not export
	// flows (switch, access point, CPE, C1) to the reason.
	never map[netip.Addr]string
	// blocked is the block list (flow_sender_policy) by address.
	blocked     []queries.FlowSenderPolicy
	blockedAddr map[netip.Addr]*queries.FlowSenderPolicy
	autoAccept  string
}

func (s *Server) loadFlowGuard(now time.Time) (*flowGuard, error) {
	roles, devices, err := s.deviceRoles(now)
	if err != nil {
		return nil, err
	}
	g := &flowGuard{roles: roles, devices: devices, byAddr: make(map[netip.Addr]*queries.Device, len(devices)),
		never: map[netip.Addr]string{}, blockedAddr: map[netip.Addr]*queries.FlowSenderPolicy{},
		autoAccept: flow.AutoAcceptClasses(s.db)}
	for i := range devices {
		d := &devices[i]
		a, ok := parseIP(d.Address)
		if !ok {
			continue
		}
		g.byAddr[a] = d
		if never, why := flowguard.NeverExports(deviceDisplayName(*d), roles[d.ID]); never {
			g.never[a] = why
		}
	}
	if g.blocked, err = queries.ListFlowSenderPolicies(s.db); err != nil {
		return nil, err
	}
	for i := range g.blocked {
		if a, ok := parseIP(g.blocked[i].Address); ok {
			g.blockedAddr[a] = &g.blocked[i]
		}
	}
	return g, nil
}

// roleAt is the role and reason of the managed device at an address ("" when
// there is none).
func (g *flowGuard) roleAt(a netip.Addr) (role, reason string) {
	if d := g.byAddr[a]; d != nil {
		r := g.roles[d.ID]
		return string(r.Role), r.Reason
	}
	return "", ""
}

// blockReason says why an exporter must not be created at an address without
// the admin saying so: the address is on the block list, or it belongs to a
// managed device that must not export. "" = nothing stands in the way.
func (g *flowGuard) blockReason(a netip.Addr) string {
	if p := g.blockedAddr[a]; p != nil {
		if p.Reason != "" {
			return p.Reason
		}
		return "the sender was blocked"
	}
	return g.never[a]
}

// apply puts the guardrails into a collector status: each exporter's and
// unknown sender's device role, the warning on an exporter whose device must
// not export, the block list (the table is the authority; the collector's
// live drop counters are kept) and the auto-accept setting in force.
func (g *flowGuard) apply(st *flow.Status) {
	for i := range st.Exporters {
		e := &st.Exporters[i]
		e.DeviceRole, e.DeviceRoleReason, e.ExportWarning = "", "", ""
		if a, ok := parseIP(e.Address); ok {
			e.DeviceRole, e.DeviceRoleReason = g.roleAt(a)
			e.ExportWarning = g.never[a]
		}
	}
	for i := range st.Unknown {
		u := &st.Unknown[i]
		if a, ok := parseIP(u.Address); ok {
			u.DeviceRole, u.DeviceRoleReason = g.roleAt(a)
		}
	}
	live := make(map[string]flow.BlockedSender, len(st.Blocked))
	for _, b := range st.Blocked {
		live[b.Address] = b
	}
	st.Blocked = make([]flow.BlockedSender, 0, len(g.blocked))
	for _, p := range g.blocked {
		b := flow.BlockedSenderFromRow(p)
		if l, ok := live[p.Address]; ok {
			b.Datagrams = max(b.Datagrams, l.Datagrams)
			if l.LastSeen != nil && (b.LastSeen == nil || l.LastSeen.After(*b.LastSeen)) {
				b.LastSeen = l.LastSeen
			}
		}
		if a, ok := parseIP(p.Address); ok {
			if d := g.byAddr[a]; d != nil {
				b.DeviceName = deviceDisplayName(*d)
				b.DeviceRole, b.DeviceRoleReason = g.roleAt(a)
			}
		}
		st.Blocked = append(st.Blocked, b)
	}
	st.AutoAcceptClasses = g.autoAccept
}

// routerRoles is the device id → role map the derived-view suggestions read.
func (g *flowGuard) routerRoles() map[string]devclass.Role {
	out := make(map[string]devclass.Role, len(g.roles))
	for id, r := range g.roles {
		out[id] = r.Role
	}
	return out
}

// nonRouterExporter reports an exporter that sits on a managed device whose
// role is not a router's — the records of such a device say little about a
// port, so an admin's derived view of that port is preferred over its
// native point.
func (g *flowGuard) nonRouterExporter(e queries.FlowExporter) bool {
	if e.DeviceID != "" {
		if r, ok := g.roles[e.DeviceID]; ok {
			return !r.Role.RoutesTraffic()
		}
	}
	if a, ok := parseIP(e.Address); ok {
		if d := g.byAddr[a]; d != nil {
			return !g.roles[d.ID].Role.RoutesTraffic()
		}
	}
	return false // not a managed device (a firewall, a probe): as before
}

// ---------- block list (admin) ----------

// handleUnblockFlowSender removes a block (admin): DELETE
// /flows/blocked?address=. The sender is an unknown sender again — and a
// managed device that must not export is blocked again by its next datagram:
// to accept such a device anyway, add it as an exporter with unblock=true.
// 204.
func (s *Server) handleUnblockFlowSender(w http.ResponseWriter, r *http.Request) {
	a, ok := parseIP(r.URL.Query().Get("address"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid address")
		return
	}
	p, err := queries.GetFlowSenderPolicy(s.db, a.String())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the block list")
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "sender is not blocked")
		return
	}
	switch err := queries.UnblockFlowSender(s.db, a.String()); {
	case errors.Is(err, queries.ErrFlowNotFound):
		writeError(w, http.StatusNotFound, "sender is not blocked")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to unblock the sender")
		return
	}
	s.audit(r, "flow_sender_unblocked", p.DeviceID, a.String()+" (was blocked: "+p.Reason+")")
	s.flowChanged()
	w.WriteHeader(http.StatusNoContent)
}

// advertiseAddress reads the admin-approved collector address the setup
// hints may use (the flow_advertise_address setting). The address is never
// detected from the process: a wrong guess would send a device's export
// somewhere else. An address that is a managed device's own is refused —
// warning says why the setting is not used.
func advertiseAddress(value string, devices []queries.Device) (addr netip.Addr, warning string) {
	a, ok := parseIP(value)
	if !ok {
		return netip.Addr{}, ""
	}
	for i := range devices {
		if da, ok := parseIP(devices[i].Address); ok && da == a {
			return netip.Addr{}, "The advertise address " + a.String() + " is the address of the managed device " +
				deviceDisplayName(devices[i]) + ", not of this NMS: fix it under Settings → Flow collector."
		}
	}
	return a, ""
}

// sortedKeys returns a set's members in order.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
