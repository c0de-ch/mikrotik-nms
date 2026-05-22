package routeros

import (
	ros "github.com/go-routeros/routeros"
)

// InterfaceDetail is the runtime snapshot used by the port-monitoring poller.
// Compared to InterfaceInfo it carries the last-link-up/down timestamps that
// RouterOS exposes via `/interface/print` (each is a free-form string like
// "feb/13/2026 09:14:22" or "" if the link never went up/down since boot).
//
// LoopProtectStatus is populated by GetInterfacesDetail when the poller is
// able to merge in `/interface/ethernet/print` (loop-protect-status is an
// ethernet-only field). Values: "" (unknown / non-ethernet), "none", or
// "on-loop" / "in-loop" when RouterOS has detected a loop on that port.
type InterfaceDetail struct {
	Name              string
	Type              string
	Running           bool
	Disabled          bool
	Slave             bool   // true when RouterOS marks the interface as bond/bridge slave (`S` flag)
	LastLinkUp        string
	LastLinkDown      string
	Comment           string
	LoopProtectStatus string
}

// GetInterfacesDetail returns runtime status for every interface on the
// device, then enriches each ethernet row with its loop-protect runtime
// status (admin can also see this in WinBox under Interface → Ethernet).
//
// Two RouterOS calls in sequence:
//  1. /interface/print          — every interface, with running/disabled/slave/comment
//  2. /interface/ethernet/print — ether interfaces only, for loop-protect-status
//
// If the second call fails (older firmware, no ether interfaces) the merge
// is skipped silently and LoopProtectStatus stays empty on every row.
//
// Filtering by interface type is done by the caller — RouterOS does not
// reliably support a `type=` filter on `/interface/print` across all
// versions, so we fetch the full list and filter in Go.
func GetInterfacesDetail(client *ros.Client) ([]InterfaceDetail, error) {
	reply, err := RunCommand(client, "/interface/print")
	if err != nil {
		return nil, err
	}

	out := make([]InterfaceDetail, 0, len(reply.Re))
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		out = append(out, InterfaceDetail{
			Name:         m["name"],
			Type:         m["type"],
			Running:      m["running"] == "true",
			Disabled:     m["disabled"] == "true",
			Slave:        m["slave"] == "true",
			LastLinkUp:   m["last-link-up-time"],
			LastLinkDown: m["last-link-down-time"],
			Comment:      m["comment"],
		})
	}

	// Enrich ether rows with loop-protect-status. Best-effort: any error
	// here means we just don't surface that field — not worth failing the
	// poll over.
	if ethReply, err := RunCommand(client, "/interface/ethernet/print"); err == nil {
		lpByName := make(map[string]string, len(ethReply.Re))
		for _, re := range ethReply.Re {
			m := GetSentenceMap(re)
			if name := m["name"]; name != "" {
				lpByName[name] = m["loop-protect-status"]
			}
		}
		for i := range out {
			if lp, ok := lpByName[out[i].Name]; ok {
				out[i].LoopProtectStatus = lp
			}
		}
	}
	return out, nil
}
