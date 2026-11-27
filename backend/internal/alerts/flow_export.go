package alerts

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowguard"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// flowExportCurrent is how recent a flow_export_on_switch finding must be to
// count. The collector refreshes it at most once a minute while datagrams
// keep arriving; a device that exports only its own management traffic can
// go quiet for minutes, so the window is generous — and it is how long the
// alert stays open after the export was stopped.
const flowExportCurrent = 30 * time.Minute

// flowExportOnSwitch fires while flow datagrams arrive from the address of a
// managed device that must not export flows — a switch, an access point or
// CPE, a C1 device (the collector's device_findings row, refreshed with
// every minute of datagrams).
//
// A source address proves nothing by itself, so the alert is a warning
// marked unconfirmed. It becomes critical only when the collector's read of
// the device's own traffic-flow settings (flow_export_checks, at most once
// an hour per device) says the device exports; that alert carries the
// rollback steps for the device's RouterOS version. A read that says the
// device does not export, or one that failed, leaves it a warning.
type flowExportOnSwitch struct{ db *sql.DB }

func (flowExportOnSwitch) Name() string { return RuleFlowExportOnSwitch }

func (r flowExportOnSwitch) Evaluate(now time.Time) ([]Observation, error) {
	findings, err := queries.ListCurrentDeviceFindings(r.db, RuleFlowExportOnSwitch, now.Add(-flowExportCurrent))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleFlowExportOnSwitch, err)
	}
	if len(findings) == 0 {
		return nil, nil
	}
	// The newest finding per device (one per sender address; a device has
	// one address).
	newest := map[string]queries.DeviceFinding{}
	for _, f := range findings {
		if cur, ok := newest[f.DeviceID]; !ok || f.LastSeen.After(cur.LastSeen) {
			newest[f.DeviceID] = f
		}
	}
	checks, err := queries.ListFlowExportChecks(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleFlowExportOnSwitch, err)
	}
	checkOf := make(map[string]queries.FlowExportCheck, len(checks))
	for _, c := range checks {
		checkOf[c.DeviceID] = c
	}
	// Roles only word the alert; without them (a read error) it still fires.
	roles, devices, err := topology.LoadDeviceRoles(r.db, nil, now)
	if err != nil {
		if devices, err = queries.ListDevices(r.db); err != nil {
			return nil, fmt.Errorf("%s: %w", RuleFlowExportOnSwitch, err)
		}
	}
	advertise, _ := queries.GetSetting(r.db, "flow_advertise_address")
	// The diagnose pass that a confirmed export starts; without it (a read
	// error) the alert still fires.
	causes, _ := likelyCauses(r.db, RuleFlowExportOnSwitch, now)

	var out []Observation
	for i := range devices {
		d := &devices[i]
		f, ok := newest[d.ID]
		if !ok {
			continue
		}
		name := deviceLabel(*d)
		chk := checkOf[d.ID]
		address := chk.Address
		if address == "" {
			address = d.Address
		}
		who := fmt.Sprintf("%s (%s)", name, address)
		facts := map[string]any{"device": name, "address": address, "confirmed": false, "sightings": f.Count,
			"check": flowguard.CheckCommands()}
		if role, ok := roles[d.ID]; ok {
			facts["role"], facts["role_reason"] = string(role.Role), role.Reason
		}
		if !chk.CheckedAt.IsZero() {
			facts["checked_at"] = chk.CheckedAt.UTC().Format(time.RFC3339)
			facts["evidence"] = chk.Detail
		}
		o := Observation{Key: d.ID, DeviceID: d.ID, Severity: SeverityWarning, Unconfirmed: true, Facts: facts}
		switch {
		case chk.Confirmed(now):
			o.Severity, o.Unconfirmed = SeverityCritical, false
			o.Summary = who + " is exporting flows although it must not — confirmed on the device (" + chk.Detail +
				"). The export loads its CPU: stop it with the rollback steps"
			facts["confirmed"] = true
			target, port := flowguard.RollbackTarget(advertise, chk.CollectorPort, chk.Targets)
			facts["rollback"] = flowguard.Rollback(d.ROSVersion, target, port).Lines()
			causes[d.ID].attach(facts)
		case chk.Result == queries.FlowExportExporting && chk.CheckedAt.Before(chk.FirstSeen) &&
			now.Sub(chk.CheckedAt) <= queries.FlowExportConfirmationValid:
			// The device confirmed an export, but before these datagrams
			// began: after the export was stopped, a datagram with its
			// address may just as well be forged.
			o.Summary = "Flow datagrams arrive again from the address of " + who + ", which must not export flows — not " +
				"confirmed: the device was last read at " + chk.CheckedAt.UTC().Format("2006-01-02 15:04 UTC") +
				", before these datagrams began"
		case chk.Result == queries.FlowExportExporting:
			// The device confirmed it once, but nobody has read it since
			// (it is paused, or unreachable): that is no confirmation of
			// the datagrams arriving now.
			o.Summary = "Flow datagrams arrive from the address of " + who + ", which must not export flows — not " +
				"confirmed: the device's last read (" + chk.CheckedAt.UTC().Format("2006-01-02 15:04 UTC") +
				") is too old to count"
		case chk.Result == queries.FlowExportNotExporting:
			o.Summary = "Flow datagrams carry the address of " + who + ", which must not export flows, but the device " +
				"says it does not export (" + chk.Detail + ") — not confirmed: the source address may be forged"
		case chk.Result == queries.FlowExportCheckFailed:
			o.Summary = "Flow datagrams arrive from the address of " + who + ", which must not export flows — not " +
				"confirmed: the device could not be read (" + chk.Detail + ")"
		default:
			o.Summary = "Flow datagrams arrive from the address of " + who + ", which must not export flows — not " +
				"confirmed on the device yet"
		}
		out = append(out, o)
	}
	return out, nil
}
