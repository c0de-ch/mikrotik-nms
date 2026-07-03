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
