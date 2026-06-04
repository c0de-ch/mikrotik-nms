package routeros

import (
	"errors"
	"strconv"

	ros "github.com/go-routeros/routeros/v3"
)

// ErrEmptyReply is returned when a RouterOS command succeeded but
// produced no sentences (e.g. a device variant that doesn't expose
// the requested resource).
var ErrEmptyReply = errors.New("routeros: empty reply")

type SystemResource struct {
	Platform     string
	Board        string
	Version      string
	Architecture string
	CPULoad      int
	MemoryFree   int64
	MemoryTotal  int64
	Uptime       string
}

func GetSystemResource(client *ros.Client) (*SystemResource, error) {
	reply, err := RunCommand(client, "/system/resource/print")
	if err != nil {
		return nil, err
	}

	if len(reply.Re) == 0 {
		return nil, ErrEmptyReply
	}

	m := GetSentenceMap(reply.Re[0])
	r := &SystemResource{
		Platform:     m["platform"],
		Board:        m["board-name"],
		Version:      m["version"],
		Architecture: m["architecture-name"],
		Uptime:       m["uptime"],
	}

	if v, err := strconv.Atoi(m["cpu-load"]); err == nil {
		r.CPULoad = v
	}
	if v, err := strconv.ParseInt(m["free-memory"], 10, 64); err == nil {
		r.MemoryFree = v
	}
	if v, err := strconv.ParseInt(m["total-memory"], 10, 64); err == nil {
		r.MemoryTotal = v
	}

	return r, nil
}

type InterfaceInfo struct {
	Name       string
	Type       string
	MACAddress string
	MTU        int
	Running    bool
	Disabled   bool
	Comment    string
}

func GetInterfaces(client *ros.Client) ([]InterfaceInfo, error) {
	reply, err := RunCommand(client, "/interface/print")
	if err != nil {
		return nil, err
	}

	var ifaces []InterfaceInfo
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		iface := InterfaceInfo{
			Name:       m["name"],
			Type:       m["type"],
			MACAddress: m["mac-address"],
			Comment:    m["comment"],
			Running:    m["running"] == "true",
			Disabled:   m["disabled"] == "true",
		}
		if v, err := strconv.Atoi(m["mtu"]); err == nil {
			iface.MTU = v
		}
		ifaces = append(ifaces, iface)
	}
	return ifaces, nil
}
