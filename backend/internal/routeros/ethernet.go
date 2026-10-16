package routeros

// EthernetStats is one Tier 4a /interface/ethernet/print row: the
// ethernet-only state the general interface print lacks — loop-protect,
// flow control, the switch chip — and the hardware drop, error and pause
// counters. Fields a RouterOS version (or a print without =stats=) does not
// return stay zero / empty.
type EthernetStats struct {
	Name   string
	Switch string // switch chip ("switch1"); "" on ports without one
	// LoopProtect is the configured mode ("on", "off", "default");
	// LoopProtectStatus the runtime state ("off", "on", "on-loop", …).
	LoopProtect       string
	LoopProtectStatus string
	RxFlowControl     string // "on", "off", "auto"
	TxFlowControl     string
	TxDropPackets     int64
	TxDropBytes       int64
	RxOverflow        int64
	RxFCSErrors       int64
	RxPause           int64
	TxPause           int64
	DriverRxPackets   int64
	DriverTxPackets   int64
	RxBroadcast       int64
	RxMulticast       int64
	HasStats          bool // true when the row carried any of the counters
}

// ethernetProplist is Tier 4a. The drop fields exist from 7.20; the
// counters may need =stats= on some versions (to be settled in the lab), so
// the parser takes whatever comes back rather than failing on a gap.
const ethernetProplist = "=.proplist=name,switch,loop-protect,loop-protect-status," +
	"rx-flow-control,tx-flow-control,tx-drop-packet,tx-drop-byte,rx-overflow,rx-fcs-error," +
	"rx-pause,tx-pause,driver-rx-packet,driver-tx-packet,rx-broadcast,rx-multicast"

// GetEthernetStats reads every ethernet port's loop-protect state and
// hardware counters in one proplisted /interface/ethernet/print. The values
// change slowly and the print costs a switch more than the general interface
// print, so it runs on its own (5 min) cadence.
func GetEthernetStats(run Runner) ([]EthernetStats, error) {
	reply, err := run.Run("/interface/ethernet/print", ethernetProplist)
	if err != nil {
		return nil, err
	}
	out := make([]EthernetStats, 0, len(reply.Re))
	for _, re := range reply.Re {
		if e, ok := parseEthernetStats(GetSentenceMap(re)); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// parseEthernetStats converts one /interface/ethernet/print sentence; false
// for a row without a name (it could not be merged into the snapshot).
func parseEthernetStats(m map[string]string) (EthernetStats, bool) {
	name := m["name"]
	if name == "" {
		return EthernetStats{}, false
	}
	e := EthernetStats{
		Name:              name,
		Switch:            m["switch"],
		LoopProtect:       m["loop-protect"],
		LoopProtectStatus: m["loop-protect-status"],
		RxFlowControl:     m["rx-flow-control"],
		TxFlowControl:     m["tx-flow-control"],
	}
	for _, f := range []struct {
		key string
		dst *int64
	}{
		{"tx-drop-packet", &e.TxDropPackets},
		{"tx-drop-byte", &e.TxDropBytes},
		{"rx-overflow", &e.RxOverflow},
		{"rx-fcs-error", &e.RxFCSErrors},
		{"rx-pause", &e.RxPause},
		{"tx-pause", &e.TxPause},
		{"driver-rx-packet", &e.DriverRxPackets},
		{"driver-tx-packet", &e.DriverTxPackets},
		{"rx-broadcast", &e.RxBroadcast},
		{"rx-multicast", &e.RxMulticast},
	} {
		var ok bool
		if *f.dst, ok = parseCounter(m, f.key); ok {
			e.HasStats = true
		}
	}
	return e, true
}

// InterfaceSnapshot is one interface as the port-stats collector serves it:
// the Tier 0 row, plus the latest Tier 4a row of the same name for ethernet
// ports.
type InterfaceSnapshot struct {
	InterfaceStats
	Ethernet EthernetStats
	// HasEthernet is false for non-ethernet interfaces, and for every row
	// before the first (or after a stale) ethernet read.
	HasEthernet bool
}

// MergeEthernet joins Tier 0 rows with Tier 4a rows by interface name into a
// new slice (the input is not retained). eth may be nil.
func MergeEthernet(rows []InterfaceStats, eth map[string]EthernetStats) []InterfaceSnapshot {
	out := make([]InterfaceSnapshot, len(rows))
	for i, r := range rows {
		out[i].InterfaceStats = r
		if e, ok := eth[r.Name]; ok {
			out[i].Ethernet, out[i].HasEthernet = e, true
		}
	}
	return out
}
