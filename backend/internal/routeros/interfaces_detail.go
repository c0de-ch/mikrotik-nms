package routeros

import (
	ros "github.com/go-routeros/routeros"
)

// InterfaceDetail is the runtime snapshot used by the port-monitoring poller.
// Compared to InterfaceInfo it carries the last-link-up/down timestamps that
// RouterOS exposes via `/interface/print` (each is a free-form string like
// "feb/13/2026 09:14:22" or "" if the link never went up/down since boot).
type InterfaceDetail struct {
	Name           string
	Type           string
	Running        bool
	Disabled       bool
	LastLinkUp     string
	LastLinkDown   string
	Comment        string
}

// GetInterfacesDetail returns runtime status for every interface on the
// device. Used by the network-health poller to detect link transitions and
// admin-disable events on physical and bridge ports.
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
			LastLinkUp:   m["last-link-up-time"],
			LastLinkDown: m["last-link-down-time"],
			Comment:      m["comment"],
		})
	}
	return out, nil
}
