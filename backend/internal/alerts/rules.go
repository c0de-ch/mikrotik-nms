package alerts

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowguard"
)

// Rule names.
const (
	RuleDeviceUnreachable = "device_unreachable"
	RuleParallelAPIPoller = "parallel_api_poller"
	RuleTLSPinMismatch    = "tls_pin_mismatch"
	// RuleFlowExportOnSwitch: see flow_export.go.
	RuleFlowExportOnSwitch = flowguard.FindingExportOnSwitch
)

// RuleNames lists the rules this build evaluates.
func RuleNames() []string {
	return []string{RuleDeviceUnreachable, RuleParallelAPIPoller, RuleTLSPinMismatch, RuleCPUHigh, RuleMemLow, RulePortEgressDrops, RuleFlowExportOnSwitch}
}

// DefaultRules are the rules evaluated on a polling instance.
func DefaultRules(db *sql.DB) []Rule {
	return []Rule{deviceUnreachable{db}, parallelAPIPoller{db}, tlsPinMismatch{db}, cpuHigh{db}, memLow{db}, portEgressDrops{db}, flowExportOnSwitch{db}}
}

func deviceLabel(d queries.Device) string {
	if d.Identity != "" {
		return d.Identity
	}
	return d.Address
}

// deviceUnreachable fires for every device the liveness poll reports
// offline — which it only does once the device has missed pings for longer
// than offline_threshold_seconds, so a single lost ping never alerts.
type deviceUnreachable struct{ db *sql.DB }

func (deviceUnreachable) Name() string { return RuleDeviceUnreachable }

func (r deviceUnreachable) Evaluate(now time.Time) ([]Observation, error) {
	devices, err := queries.ListDevices(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleDeviceUnreachable, err)
	}
	var out []Observation
	for _, d := range devices {
		if d.Status != "offline" {
			continue
		}
		summary := fmt.Sprintf("%s (%s) is not reachable", deviceLabel(d), d.Address)
		facts := map[string]any{"device": deviceLabel(d), "address": d.Address}
		if d.LastSeen != nil && !d.LastSeen.IsZero() {
			summary += ", last seen " + d.LastSeen.UTC().Format("2006-01-02 15:04 UTC")
			facts["last_seen"] = d.LastSeen.UTC().Format(time.RFC3339)
		}
		out = append(out, Observation{
			Key: d.ID, DeviceID: d.ID, Severity: SeverityCritical, Summary: summary, Facts: facts,
		})
	}
	return out, nil
}

// parallelPollerCurrent is how recent a parallel_api_poller finding must be
// to count: the login watch re-confirms a foreign session at most once an
// hour, so a finding stays current for two confirmations and a margin.
const parallelPollerCurrent = 2*time.Hour + 15*time.Minute

// parallelAPIPoller fires while the login watch keeps seeing API logins on
// a device from an address that is not this NMS (device_findings).
type parallelAPIPoller struct{ db *sql.DB }

func (parallelAPIPoller) Name() string { return RuleParallelAPIPoller }

func (r parallelAPIPoller) Evaluate(now time.Time) ([]Observation, error) {
	findings, err := queries.ListCurrentDeviceFindings(r.db, RuleParallelAPIPoller, now.Add(-parallelPollerCurrent))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleParallelAPIPoller, err)
	}
	if len(findings) == 0 {
		return nil, nil
	}
	devices, err := queries.ListDevices(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleParallelAPIPoller, err)
	}
	names := make(map[string]string, len(devices))
	for _, d := range devices {
		names[d.ID] = deviceLabel(d)
	}
	var out []Observation
	for _, f := range findings {
		severity := SeverityWarning
		switch f.Severity {
		case "info":
			continue // seen in the log but not confirmed as a live session
		case "critical":
			severity = SeverityCritical
		}
		name := names[f.DeviceID]
		if name == "" {
			continue // the device is gone
		}
		out = append(out, Observation{
			Key: f.DeviceID + "|" + f.Detail, DeviceID: f.DeviceID, Severity: severity,
			Summary: fmt.Sprintf("%s: API login by something other than this NMS (%s)", name, f.Detail),
			Facts:   map[string]any{"device": name, "detail": f.Detail, "count": f.Count},
		})
	}
	return out, nil
}

// pinMismatchCurrent is how recent a tls_pin_mismatch finding must be to
// count. While the mismatch lasts the pool retries the device every ten
// minutes or so and each refusal touches the finding; a device nothing else
// polls is still dialled by its hourly info pass. Two of those and a margin.
const pinMismatchCurrent = 2*time.Hour + 15*time.Minute

// tlsPinMismatch fires while a TLS device answers with another certificate
// than the one pinned for it: the session was refused before any login
// (device_findings, written by the RouterOS client's pin check). It stops
// as soon as the device is pinned to the certificate that was presented or
// no longer uses TLS — the admin's decision either way — and otherwise once
// no mismatch has been seen for pinMismatchCurrent.
type tlsPinMismatch struct{ db *sql.DB }

func (tlsPinMismatch) Name() string { return RuleTLSPinMismatch }

func (r tlsPinMismatch) Evaluate(now time.Time) ([]Observation, error) {
	findings, err := queries.ListCurrentDeviceFindings(r.db, queries.FindingTLSPinMismatch, now.Add(-pinMismatchCurrent))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleTLSPinMismatch, err)
	}
	if len(findings) == 0 {
		return nil, nil
	}
	devices, err := queries.ListDevices(r.db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", RuleTLSPinMismatch, err)
	}
	byID := make(map[string]queries.Device, len(devices))
	for _, d := range devices {
		byID[d.ID] = d
	}
	var out []Observation
	for _, f := range findings {
		d, ok := byID[f.DeviceID]
		if !ok || !d.UseTLS || d.PinCertSHA256 == f.Detail {
			continue // gone, no longer on TLS, or pinned to that certificate since
		}
		name := deviceLabel(d)
		out = append(out, Observation{
			Key: f.DeviceID + "|" + f.Detail, DeviceID: f.DeviceID, Severity: SeverityCritical,
			Summary: fmt.Sprintf("%s (%s) presented a TLS certificate that is not the pinned one — the NMS did not log in", name, d.Address),
			Facts: map[string]any{
				"device": name, "address": d.Address,
				"presented": f.Detail, "pinned": d.PinCertSHA256, "count": f.Count,
			},
		})
	}
	return out, nil
}
