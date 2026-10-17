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

	// Fields is the proplisted sentence as the device returned it, for
	// readers that need more than the typed fields: the class probe hands it
	// to devclass.ParseResourceFacts (cpu-count, cpu-frequency, …).
	Fields map[string]string
}

// systemResourceProplist limits the resource print to what its readers use:
// the inventory and health fields above plus the CPU facts the device class
// is derived from. Without it RouterOS formats every field (bad blocks,
// write-sector counters, build time, …) on each print.
const systemResourceProplist = "=.proplist=board-name,version,platform,architecture-name," +
	"cpu,cpu-count,cpu-frequency,cpu-load,total-memory,free-memory,uptime"

func GetSystemResource(client *ros.Client) (*SystemResource, error) {
	return ReadSystemResource(ClientRunner{Client: client})
}

// ReadSystemResource is GetSystemResource on a Runner: one proplisted
// /system/resource/print.
func ReadSystemResource(run Runner) (*SystemResource, error) {
	reply, err := run.Run("/system/resource/print", systemResourceProplist)
	if err != nil {
		return nil, err
	}

	if reply == nil || len(reply.Re) == 0 {
		return nil, ErrEmptyReply
	}

	m := GetSentenceMap(reply.Re[0])
	r := &SystemResource{
		Platform:     m["platform"],
		Board:        m["board-name"],
		Version:      m["version"],
		Architecture: m["architecture-name"],
		Uptime:       m["uptime"],
		Fields:       m,
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

// InterfaceInfo is the identity slice of an interface: its name, type and
// MAC.
type InterfaceInfo struct {
	Name       string
	Type       string
	MACAddress string
}

// interfaceIdentityProplist limits GetInterfaces to what identity checks read.
const interfaceIdentityProplist = "=.proplist=name,type,mac-address"

// GetInterfaces lists the device's interfaces with name, type and MAC only.
// It is not a poll: the scheduled interface read is Tier 0
// (GetInterfaceStats), which every consumer gets from the port-stats
// collector's snapshot. This one confirms the identity of a device answering
// at a candidate address (IP auto-follow), which no snapshot covers yet.
func GetInterfaces(run Runner) ([]InterfaceInfo, error) {
	reply, err := run.Run("/interface/print", interfaceIdentityProplist)
	if err != nil {
		return nil, err
	}

	ifaces := make([]InterfaceInfo, 0, len(reply.Re))
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		ifaces = append(ifaces, InterfaceInfo{
			Name:       m["name"],
			Type:       m["type"],
			MACAddress: m["mac-address"],
		})
	}
	return ifaces, nil
}
