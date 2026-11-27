package routeros

import (
	"errors"
	"strconv"
	"strings"

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

// LoadSample is the minute's CPU telemetry (Tier 1): the three fields of
// /system/resource that change from one minute to the next.
type LoadSample struct {
	CPULoad       int   // percent, 0..100
	FreeMemory    int64 // bytes
	HasFreeMemory bool
	Uptime        string // as RouterOS prints it, e.g. "1w2d3h4m5s"
	UptimeSeconds int64
	HasUptime     bool
}

const (
	loadSampleProplist = "=.proplist=cpu-load,free-memory,uptime"
	cpuLoadProplist    = "=.proplist=cpu-load"
)

// ErrNoCPULoad is returned when a resource reply carries no usable cpu-load:
// a missing value is no sample, never 0 %.
var ErrNoCPULoad = errors.New("routeros: reply carries no cpu-load")

func parseCPULoad(m map[string]string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(m["cpu-load"]))
	if err != nil || v < 0 || v > 100 {
		return 0, ErrNoCPULoad
	}
	return v, nil
}

func resourceSentence(run Runner, proplist string) (map[string]string, error) {
	reply, err := run.Run("/system/resource/print", proplist)
	if err != nil {
		return nil, err
	}
	if reply == nil || len(reply.Re) == 0 {
		return nil, ErrEmptyReply
	}
	return GetSentenceMap(reply.Re[0]), nil
}

// ReadLoadSample is the Tier 1 read: one /system/resource/print proplisted
// to cpu-load, free-memory and uptime — the cheapest print that says how
// busy the device is, how much memory it has left and whether it rebooted.
func ReadLoadSample(run Runner) (LoadSample, error) {
	m, err := resourceSentence(run, loadSampleProplist)
	if err != nil {
		return LoadSample{}, err
	}
	cpu, err := parseCPULoad(m)
	if err != nil {
		return LoadSample{}, err
	}
	s := LoadSample{CPULoad: cpu, Uptime: m["uptime"]}
	if v, err := strconv.ParseInt(strings.TrimSpace(m["free-memory"]), 10, 64); err == nil && v >= 0 {
		s.FreeMemory, s.HasFreeMemory = v, true
	}
	s.UptimeSeconds, s.HasUptime = ParseUptime(s.Uptime)
	return s, nil
}

// ReadCPULoad is the post-poll sample (1c): /system/resource/print
// proplisted to cpu-load alone.
func ReadCPULoad(run Runner) (int, error) {
	m, err := resourceSentence(run, cpuLoadProplist)
	if err != nil {
		return 0, err
	}
	return parseCPULoad(m)
}

// ParseUptime converts RouterOS's uptime notation — whole segments of
// weeks, days, hours, minutes and seconds, e.g. "1w2d3h4m5s" — to seconds.
// ok is false for anything else.
func ParseUptime(s string) (seconds int64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if start == i || i == len(s) {
			return 0, false // a unit without a number, or a number without a unit
		}
		n, err := strconv.ParseInt(s[start:i], 10, 64)
		if err != nil {
			return 0, false
		}
		switch s[i] {
		case 'w':
			seconds += n * 7 * 86400
		case 'd':
			seconds += n * 86400
		case 'h':
			seconds += n * 3600
		case 'm':
			seconds += n * 60
		case 's':
			seconds += n
		default:
			return 0, false
		}
		i++
	}
	return seconds, true
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
