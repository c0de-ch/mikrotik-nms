package routeros

import (
	ros "github.com/go-routeros/routeros/v3"
)

type NeighborInfo struct {
	LocalInterface    string
	NeighborAddress   string
	NeighborMAC       string
	NeighborIdentity  string
	NeighborPlatform  string
	NeighborBoard     string
	NeighborVersion   string
	NeighborInterface string
	DiscoveredBy      string
}

func GetNeighbors(client *ros.Client) ([]NeighborInfo, error) {
	reply, err := RunCommand(client, "/ip/neighbor/print")
	if err != nil {
		return nil, err
	}

	var neighbors []NeighborInfo
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		n := NeighborInfo{
			LocalInterface:    m["interface"],
			NeighborAddress:   m["address"],
			NeighborMAC:       m["mac-address"],
			NeighborIdentity:  m["identity"],
			NeighborPlatform:  m["platform"],
			NeighborBoard:     m["board"],
			NeighborVersion:   m["version"],
			NeighborInterface: m["interface-name"],
			DiscoveredBy:      m["discovered-by"],
		}
		neighbors = append(neighbors, n)
	}
	return neighbors, nil
}
