package routeros

import (
	"strconv"
	"strings"
)

// IfIndexFromID converts a RouterOS API ".id" ("*D") to the SNMP / flow-export
// ifIndex (13): strip "*", parse hex, must fit uint32 and be > 0. RouterOS
// traffic-flow numbers in/out interfaces with the same index SNMP uses, which
// is the interface's internal id.
func IfIndexFromID(id string) (uint32, bool) {
	hexID, ok := strings.CutPrefix(id, "*")
	if !ok || hexID == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(hexID, 16, 32)
	if err != nil || v == 0 {
		return 0, false
	}
	return uint32(v), true
}

// VLANInfo is one /interface/vlan row.
type VLANInfo struct {
	Name   string
	VLANID int
	Parent string // the vlan's interface= (e.g. "bridge")
}

// GetVLANInfo runs /interface/vlan/print =.proplist=name,vlan-id,interface.
// The port-stats collector's stacking read (GetInterfaceRelations, VLANs)
// carries the same rows; this print stands in only until that has run.
func GetVLANInfo(run Runner) ([]VLANInfo, error) {
	reply, err := run.Run("/interface/vlan/print", "=.proplist=name,vlan-id,interface")
	if err != nil {
		return nil, err
	}
	out := make([]VLANInfo, 0, len(reply.Re))
	for _, re := range reply.Re {
		if v, ok := parseVLANInfo(GetSentenceMap(re)); ok {
			out = append(out, v)
		}
	}
	return out, nil
}

// parseVLANInfo converts one /interface/vlan/print sentence; false for a row
// without a name. A missing or non-numeric vlan-id parses as 0.
func parseVLANInfo(m map[string]string) (VLANInfo, bool) {
	name := strings.TrimSpace(m["name"])
	if name == "" {
		return VLANInfo{}, false
	}
	id, _ := strconv.Atoi(strings.TrimSpace(m["vlan-id"]))
	return VLANInfo{Name: name, VLANID: id, Parent: strings.TrimSpace(m["interface"])}, true
}
