package routeros

import (
	ros "github.com/go-routeros/routeros/v3"
)

// FindBridgeHostPort looks a MAC up in the device's bridge host (FDB) table
// and returns the port it was dynamically learned on, or "" when absent.
// Local entries (the bridge's own MACs) are ignored.
func FindBridgeHostPort(client *ros.Client, mac string) (string, error) {
	reply, err := RunCommand(client, "/interface/bridge/host/print", "?mac-address="+mac)
	if err != nil {
		return "", err
	}
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		if m["local"] == "true" {
			continue
		}
		if iface := m["on-interface"]; iface != "" {
			return iface, nil
		}
		if iface := m["interface"]; iface != "" {
			return iface, nil
		}
	}
	return "", nil
}
