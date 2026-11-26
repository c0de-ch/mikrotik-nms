package routeros

import (
	"strconv"
	"strings"
)

// TrafficFlowTarget is one /ip/traffic-flow/target row: a collector the
// device exports to.
type TrafficFlowTarget struct {
	DstAddress string
	Port       int
	SrcAddress string
	Version    string // 1 | 5 | 9 | ipfix
	Disabled   bool
}

// TrafficFlowState is what a device says about its flow export.
type TrafficFlowState struct {
	Enabled    bool
	Interfaces string // the interfaces= list ("all" is the RouterOS default)
	Targets    []TrafficFlowTarget
}

// Exporting reports whether the device sends flow records: traffic-flow is
// enabled and at least one target is not disabled. Enabled without a target
// accounts flows but sends nothing.
func (s TrafficFlowState) Exporting() bool {
	if !s.Enabled {
		return false
	}
	for _, t := range s.Targets {
		if !t.Disabled {
			return true
		}
	}
	return false
}

// Summary is the state in one line, for a finding or an alert.
func (s TrafficFlowState) Summary() string {
	out := "traffic-flow disabled"
	if s.Enabled {
		out = "traffic-flow enabled"
		if s.Interfaces != "" {
			out += " (interfaces=" + s.Interfaces + ")"
		}
	}
	if len(s.Targets) == 0 {
		return out + "; no targets"
	}
	parts := make([]string, 0, len(s.Targets))
	for _, t := range s.Targets {
		p := t.DstAddress + ":" + strconv.Itoa(t.Port)
		notes := make([]string, 0, 2)
		if t.Version != "" {
			notes = append(notes, t.Version)
		}
		if t.Disabled {
			notes = append(notes, "disabled")
		}
		if len(notes) > 0 {
			p += " (" + strings.Join(notes, ", ") + ")"
		}
		parts = append(parts, p)
	}
	return out + "; targets: " + strings.Join(parts, ", ")
}

// ReadTrafficFlow reads a device's flow-export configuration: two read-only,
// proplisted prints (/ip/traffic-flow and its targets). It changes nothing
// on the device. An error — a trap, a transport failure, an empty settings
// reply — means "could not tell", never "not exporting".
func ReadTrafficFlow(run Runner) (TrafficFlowState, error) {
	var st TrafficFlowState
	reply, err := run.Run("/ip/traffic-flow/print", "=.proplist=enabled,interfaces,active-flow-timeout")
	if err != nil {
		return st, err
	}
	if reply == nil || len(reply.Re) == 0 {
		return st, ErrEmptyReply
	}
	m := reply.Re[0].Map
	st.Enabled, st.Interfaces = rosBool(m["enabled"]), m["interfaces"]

	reply, err = run.Run("/ip/traffic-flow/target/print", "=.proplist=dst-address,port,src-address,version,disabled")
	if err != nil {
		return TrafficFlowState{}, err
	}
	if reply != nil {
		for _, re := range reply.Re {
			t := TrafficFlowTarget{DstAddress: re.Map["dst-address"], SrcAddress: re.Map["src-address"],
				Version: re.Map["version"], Disabled: rosBool(re.Map["disabled"])}
			t.Port, _ = strconv.Atoi(strings.TrimSpace(re.Map["port"]))
			st.Targets = append(st.Targets, t)
		}
	}
	return st, nil
}
