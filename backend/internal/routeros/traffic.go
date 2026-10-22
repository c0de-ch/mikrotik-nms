package routeros

import (
	"strconv"
	"strings"

	ros "github.com/go-routeros/routeros/v3"
)

type TrafficData struct {
	RxBitsPerSec    int64
	TxBitsPerSec    int64
	RxPacketsPerSec int
	TxPacketsPerSec int
}

// GetTrafficSnapshot gets a one-shot traffic sample for an interface.
func GetTrafficSnapshot(client *ros.Client, ifaceName string) (*TrafficData, error) {
	reply, err := RunCommand(client, "/interface/monitor-traffic",
		"=interface="+ifaceName,
		"=once=",
	)
	if err != nil {
		return nil, err
	}

	if len(reply.Re) == 0 {
		return &TrafficData{}, nil
	}

	m := GetSentenceMap(reply.Re[0])
	t := &TrafficData{}

	if v, err := strconv.ParseInt(m["rx-bits-per-second"], 10, 64); err == nil {
		t.RxBitsPerSec = v
	}
	if v, err := strconv.ParseInt(m["tx-bits-per-second"], 10, 64); err == nil {
		t.TxBitsPerSec = v
	}
	if v, err := strconv.Atoi(m["rx-packets-per-second"]); err == nil {
		t.RxPacketsPerSec = v
	}
	if v, err := strconv.Atoi(m["tx-packets-per-second"]); err == nil {
		t.TxPacketsPerSec = v
	}

	return t, nil
}

// GetPortTraffic samples rx/tx for several interfaces in ONE
// /interface/monitor-traffic call (comma-separated), returning a map keyed by
// interface name. This keeps the switch port-grid to a single API round-trip
// instead of one call per port.
func GetPortTraffic(client *ros.Client, names []string) (map[string]TrafficData, error) {
	out := make(map[string]TrafficData, len(names))
	if len(names) == 0 {
		return out, nil
	}
	reply, err := RunCommand(client, "/interface/monitor-traffic",
		"=interface="+strings.Join(names, ","),
		"=once=",
	)
	if err != nil {
		return out, err
	}
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		name := m["name"]
		if name == "" {
			name = m["interface"]
		}
		if name == "" {
			continue
		}
		t := TrafficData{}
		if v, err := strconv.ParseInt(m["rx-bits-per-second"], 10, 64); err == nil {
			t.RxBitsPerSec = v
		}
		if v, err := strconv.ParseInt(m["tx-bits-per-second"], 10, 64); err == nil {
			t.TxBitsPerSec = v
		}
		if v, err := strconv.Atoi(m["rx-packets-per-second"]); err == nil {
			t.RxPacketsPerSec = v
		}
		if v, err := strconv.Atoi(m["tx-packets-per-second"]); err == nil {
			t.TxPacketsPerSec = v
		}
		out[name] = t
	}
	return out, nil
}

// InterfaceStats is one Tier 0 /interface/print row: the interface's
// inventory (type, MAC, MTU, comment), its link state (running, disabled,
// slave, the last link up/down times and the link-downs counter) and the
// cumulative byte, packet, error and drop counters the port-stats collector
// turns into rates. It is the one scheduled interface read: inventory, the
// port monitor, uplink/VPN detection and the flow ifIndex names all consume
// the collector's copy of it instead of printing the table again. Counters
// are 64-bit and routinely exceed 2^32.
type InterfaceStats struct {
	ID       string // ".id", e.g. "*1" — the flow-export ifIndex in hex (IfIndexFromID)
	Name     string
	Type     string
	MAC      string // mac-address as printed; "" for interfaces without one
	MTU      int    // 0 when absent or not a number
	Comment  string
	Running  bool
	Disabled bool
	Slave    bool // a bridge or bond member (the S flag)
	// LastLinkUp / LastLinkDown are RouterOS's raw timestamps, "" when the
	// link never went up / down since boot.
	LastLinkUp, LastLinkDown string
	// LinkDowns counts link-down events since boot; flap detection works on
	// its deltas. HasLinkDowns is false when the row did not carry it.
	LinkDowns            int64
	HasLinkDowns         bool
	RxBytes, TxBytes     int64
	RxPackets, TxPackets int64
	RxErrors, TxErrors   int64 // 0 when absent
	RxDrops, TxDrops     int64 // 0 when absent
	TxQueueDrops         int64 // 0 when absent
	HasCounters          bool  // false when the row carried neither rx-byte nor tx-byte
}

// interfaceStatsProplist is Tier 0, the only scheduled /interface/print: it
// names every attribute any consumer of the snapshot reads, so nothing else
// needs a print of its own. The counters come back without "=stats="
// (verified on RouterOS 7.22/7.23: =stats= returns the identical attribute
// set), and the .proplist keeps a 59-interface reply to ~30 ms.
const interfaceStatsProplist = "=.proplist=.id,name,type,mac-address,mtu,running,disabled,slave,comment," +
	"last-link-up-time,last-link-down-time,link-downs," +
	"rx-byte,tx-byte,rx-packet,tx-packet,rx-error,tx-error,rx-drop,tx-drop,tx-queue-drop"

// GetInterfaceStats reads every interface's inventory, link state and
// counters in one proplisted /interface/print. It is a short print, so it
// runs on the pooled client (ClientRunner, CommandTimeout-bounded).
func GetInterfaceStats(run Runner) ([]InterfaceStats, error) {
	reply, err := run.Run("/interface/print", interfaceStatsProplist)
	if err != nil {
		return nil, err
	}
	out := make([]InterfaceStats, 0, len(reply.Re))
	for _, re := range reply.Re {
		if s, ok := parseInterfaceStats(GetSentenceMap(re)); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// parseInterfaceStats converts one /interface/print sentence. It returns false
// for a row without a name. rx-error/tx-error/rx-drop/tx-drop/tx-queue-drop
// are absent on some rows (ether ports on the RB5009) and parse as 0.
func parseInterfaceStats(m map[string]string) (InterfaceStats, bool) {
	name := m["name"]
	if name == "" {
		return InterfaceStats{}, false
	}
	s := InterfaceStats{
		ID:           m[".id"],
		Name:         name,
		Type:         m["type"],
		MAC:          m["mac-address"],
		Comment:      m["comment"],
		Running:      m["running"] == "true",
		Disabled:     m["disabled"] == "true",
		Slave:        m["slave"] == "true",
		LastLinkUp:   m["last-link-up-time"],
		LastLinkDown: m["last-link-down-time"],
	}
	if mtu, ok := parseCounter(m, "mtu"); ok {
		s.MTU = int(mtu)
	}
	s.LinkDowns, s.HasLinkDowns = parseCounter(m, "link-downs")
	var hasRx, hasTx bool
	s.RxBytes, hasRx = parseCounter(m, "rx-byte")
	s.TxBytes, hasTx = parseCounter(m, "tx-byte")
	s.HasCounters = hasRx || hasTx
	s.RxPackets, _ = parseCounter(m, "rx-packet")
	s.TxPackets, _ = parseCounter(m, "tx-packet")
	s.RxErrors, _ = parseCounter(m, "rx-error")
	s.TxErrors, _ = parseCounter(m, "tx-error")
	s.RxDrops, _ = parseCounter(m, "rx-drop")
	s.TxDrops, _ = parseCounter(m, "tx-drop")
	s.TxQueueDrops, _ = parseCounter(m, "tx-queue-drop")
	return s, true
}

// parseCounter parses a decimal 64-bit counter (or other integer) attribute.
// ok is false when the attribute is absent or not a number (the value is
// then 0).
func parseCounter(m map[string]string, key string) (int64, bool) {
	v, present := m[key]
	if !present {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
