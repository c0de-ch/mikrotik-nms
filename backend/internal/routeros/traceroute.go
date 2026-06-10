package routeros

import (
	"strconv"
	"strings"
	"time"

	ros "github.com/go-routeros/routeros/v3"
)

// TracerouteHop is one row of a /tool/traceroute hop table. The ms fields are
// nil for hops that never answered (RouterOS shows "timeout"). The json tags
// are the wire contract — this struct is marshalled into traceroute_runs.hops
// and served as-is by the API.
type TracerouteHop struct {
	Hop     int      `json:"hop"`
	Address string   `json:"address"`
	LossPct float64  `json:"loss_pct"`
	Sent    int      `json:"sent"`
	LastMs  *float64 `json:"last_ms"`
	AvgMs   *float64 `json:"avg_ms"`
	BestMs  *float64 `json:"best_ms"`
	WorstMs *float64 `json:"worst_ms"`
	Status  string   `json:"status"`
}

// Traceroute runs /tool/traceroute on the RouterOS device and returns the
// final hop table. srcAddress / iface are optional probe-source selectors
// (=src-address= / =interface=), mirroring PingOptions.
//
// MUST be called on a dedicated DialOnce connection: a 20-hop trace against an
// unresponsive path can run for ~45s, past the shared CommandTimeout, and
// would otherwise hold a pooled client's mutex and force-close it under every
// other poller.
func Traceroute(client *ros.Client, address, srcAddress, iface string, timeout time.Duration) ([]TracerouteHop, error) {
	args := []string{
		"=address=" + address,
		"=count=1",
		"=max-hops=20",
	}
	if srcAddress != "" {
		args = append(args, "=src-address="+srcAddress)
	}
	if iface != "" {
		args = append(args, "=interface="+iface)
	}
	reply, err := RunCommandWithTimeout(client, timeout, "/tool/traceroute", args...)
	if err != nil {
		return nil, err
	}
	replies := make([]map[string]string, 0, len(reply.Re))
	for _, re := range reply.Re {
		replies = append(replies, GetSentenceMap(re))
	}
	return summarizeTraceroute(replies), nil
}

// summarizeTraceroute converts raw /tool/traceroute reply sentences into the
// final hop table. Pure (no I/O) so it is unit-testable against captured
// sentence maps.
//
// RouterOS streams the whole hop table repeatedly, tagging each refresh pass
// with a ".section" attribute (missing on very old versions => treated as
// "0"). Only the group with the HIGHEST numeric section — the final, most
// complete pass — is kept; hop numbers are the 1-based sentence index within
// that group. Timeout hops carry a status (e.g. "timeout"), often no address,
// and unparseable/absent RTT fields, which become nil.
func summarizeTraceroute(replies []map[string]string) []TracerouteHop {
	sections := make(map[int][]map[string]string)
	best := -1
	for _, m := range replies {
		sec := 0
		if v := m[".section"]; v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				sec = n
			}
		}
		sections[sec] = append(sections[sec], m)
		if sec > best {
			best = sec
		}
	}
	if best < 0 {
		return nil
	}

	last := sections[best]
	hops := make([]TracerouteHop, 0, len(last))
	for i, m := range last {
		hop := TracerouteHop{
			Hop:     i + 1,
			Address: m["address"],
			Status:  m["status"],
		}
		if n, err := strconv.Atoi(strings.TrimSpace(m["sent"])); err == nil {
			hop.Sent = n
		}
		if loss, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(m["loss"]), "%"), 64); err == nil {
			hop.LossPct = loss
		}
		hop.LastMs = parseHopMs(m["last"])
		hop.AvgMs = parseHopMs(m["avg"])
		hop.BestMs = parseHopMs(m["best"])
		hop.WorstMs = parseHopMs(m["worst"])
		hops = append(hops, hop)
	}
	return hops
}

// parseHopMs parses one hop RTT field into milliseconds. Real RouterOS output
// (verified live) serializes "last" as a duration ("20.4ms") but avg/best/worst
// as UNITLESS milliseconds ("20.4"), so try the duration notation first and
// fall back to a bare float of milliseconds. Returns nil when neither parses
// (e.g. "", "timeout").
func parseHopMs(v string) *float64 {
	v = strings.TrimSpace(v)
	if ms, ok := parseROSDuration(v); ok {
		return &ms
	}
	if ms, err := strconv.ParseFloat(v, 64); err == nil {
		return &ms
	}
	return nil
}
