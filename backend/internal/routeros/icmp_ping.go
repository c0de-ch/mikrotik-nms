package routeros

import (
	"math"
	"strconv"
	"strings"

	ros "github.com/go-routeros/routeros/v3"
)

// PingResult summarizes one /ping burst run on a RouterOS device.
// RTT/jitter pointers are nil when no replies came back (all timeouts) or, for
// jitter, when fewer than two per-probe RTTs were observed.
type PingResult struct {
	Sent     int
	Received int
	LossPct  float64
	RTTMinMs *float64
	RTTAvgMs *float64
	RTTMaxMs *float64
	JitterMs *float64
}

// pingProbeInterval is the spacing between probes within one burst. It is
// deliberately sub-second: RunCommand holds the device's per-client mutex for
// the whole burst, serializing every other poller (traffic streams, wifi,
// topology) touching that device. At 0.2s a count=10 burst occupies the
// connection ~2-3s even against an unreachable target, instead of ~11s at the
// CLI-default 1s spacing.
const pingProbeInterval = "0.2"

// PingOptions parameterizes one RunPing burst. SrcAddress / Interface are
// optional probe-source selectors (=src-address= / =interface=) for
// multi-ISP / policy-routed setups where the probe must leave via a specific
// VLAN interface or source IP; empty means RouterOS picks per its routing
// table.
type PingOptions struct {
	Address    string
	Count      int
	SrcAddress string
	Interface  string
}

// RunPing executes the RouterOS /ping command from the device the client is
// connected to and summarizes the replies. Count is clamped to 1..10 so the
// burst (count × pingProbeInterval) stays far under the shared CommandTimeout.
//
// Not to be confused with Ping (the plain TCP liveness dial): RunPing sends
// real ICMP from the RouterOS device to an arbitrary address.
func RunPing(client *ros.Client, opts PingOptions) (*PingResult, error) {
	count := opts.Count
	if count < 1 {
		count = 1
	}
	if count > 10 {
		count = 10
	}
	args := []string{
		"=address=" + opts.Address,
		"=count=" + strconv.Itoa(count),
		"=interval=" + pingProbeInterval,
	}
	if opts.SrcAddress != "" {
		args = append(args, "=src-address="+opts.SrcAddress)
	}
	if opts.Interface != "" {
		args = append(args, "=interface="+opts.Interface)
	}
	reply, err := RunCommand(client, "/ping", args...)
	if err != nil {
		return nil, err
	}
	replies := make([]map[string]string, 0, len(reply.Re))
	for _, re := range reply.Re {
		replies = append(replies, GetSentenceMap(re))
	}
	res := summarizePing(replies, count)
	return &res, nil
}

// summarizePing converts the raw /ping reply sentences into a PingResult.
// Pure (no I/O) so it is unit-testable against captured sentence maps.
//
// RouterOS emits one !re per probe carrying cumulative counters (sent,
// received, packet-loss) plus min-rtt/avg-rtt/max-rtt once replies exist; the
// LAST sentence therefore holds the final summary and is preferred. When those
// cumulative keys are absent (older RouterOS), it falls back to counting rows
// and aggregating the per-row "time" values. Timeout rows carry a "status"
// (e.g. "timeout") and no "time".
//
// Jitter is the mean absolute difference of consecutive successful per-probe
// RTTs — needs at least two successes, else nil.
func summarizePing(replies []map[string]string, count int) PingResult {
	res := PingResult{Sent: count, Received: 0, LossPct: 100}

	// Per-probe RTTs in reply order (used for jitter and for the fallback path).
	var rtts []float64
	for _, m := range replies {
		if t := m["time"]; t != "" {
			if ms, ok := parseROSDuration(t); ok {
				rtts = append(rtts, ms)
			}
		}
	}

	cumulative := false
	if len(replies) > 0 {
		last := replies[len(replies)-1]
		sent, sErr := strconv.Atoi(last["sent"])
		recv, rErr := strconv.Atoi(last["received"])
		if sErr == nil && rErr == nil {
			cumulative = true
			res.Sent = sent
			res.Received = recv
			if loss, err := strconv.ParseFloat(strings.TrimSuffix(last["packet-loss"], "%"), 64); err == nil {
				res.LossPct = loss
			} else if sent > 0 {
				res.LossPct = float64(sent-recv) * 100 / float64(sent)
			}
			if recv > 0 {
				if ms, ok := parseROSDuration(last["min-rtt"]); ok {
					res.RTTMinMs = &ms
				}
				if ms, ok := parseROSDuration(last["avg-rtt"]); ok {
					res.RTTAvgMs = &ms
				}
				if ms, ok := parseROSDuration(last["max-rtt"]); ok {
					res.RTTMaxMs = &ms
				}
			}
		}
	}

	if !cumulative {
		res.Sent = len(replies)
		if res.Sent == 0 {
			res.Sent = count
		}
		res.Received = len(rtts)
		if res.Sent > 0 {
			res.LossPct = float64(res.Sent-res.Received) * 100 / float64(res.Sent)
		}
		if len(rtts) > 0 {
			minV, maxV, sum := rtts[0], rtts[0], 0.0
			for _, v := range rtts {
				minV = math.Min(minV, v)
				maxV = math.Max(maxV, v)
				sum += v
			}
			avg := sum / float64(len(rtts))
			res.RTTMinMs = &minV
			res.RTTMaxMs = &maxV
			res.RTTAvgMs = &avg
		}
	}

	if len(rtts) >= 2 {
		var diffSum float64
		for i := 1; i < len(rtts); i++ {
			diffSum += math.Abs(rtts[i] - rtts[i-1])
		}
		j := diffSum / float64(len(rtts)-1)
		res.JitterMs = &j
	}

	return res
}

// parseROSDuration parses RouterOS's concatenated duration notation into
// milliseconds: segments of <number><unit> with units us/ms/s/m/h, e.g.
// "11ms559us", "559us", "1s234ms", or the ROS6-style plain "11ms". Returns
// ok=false for empty strings, unknown units, or trailing garbage.
func parseROSDuration(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var totalMs float64
	i := 0
	for i < len(s) {
		start := i
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
			i++
		}
		if start == i {
			return 0, false // no digits where a number was expected
		}
		val, err := strconv.ParseFloat(s[start:i], 64)
		if err != nil {
			return 0, false
		}
		unitStart := i
		for i < len(s) && s[i] >= 'a' && s[i] <= 'z' {
			i++
		}
		switch s[unitStart:i] {
		case "us":
			totalMs += val / 1000
		case "ms":
			totalMs += val
		case "s":
			totalMs += val * 1000
		case "m":
			totalMs += val * 60 * 1000
		case "h":
			totalMs += val * 60 * 60 * 1000
		default:
			return 0, false
		}
	}
	return totalMs, true
}
