package routeros

import (
	"regexp"
	"strings"

	ros "github.com/go-routeros/routeros/v3"
)

// BridgeLogEvent is a parsed bridge,info log line that signals a loop or flap.
type BridgeLogEvent struct {
	Time    string
	Topics  string
	Message string

	// Kind is one of:
	//   "loop_detected" — bridge detected an L2 loop on a port
	//   "mac_flap"      — same MAC observed on two different ports
	//   "bpdu_on_edge"  — BPDU received on a port configured as edge
	Kind      string
	Bridge    string
	Port      string
	OtherPort string
	MAC       string
}

// Fingerprint dedupes events across consecutive polls of the same log buffer.
func (e *BridgeLogEvent) Fingerprint() string {
	return e.Time + "\x1f" + e.Message
}

// Pattern notes: RouterOS bridge,info messages vary across versions. Each
// regex captures a known phrase plus the relevant port / MAC. We keep the
// patterns intentionally loose; if a line matches none we discard it.
// portToken matches RouterOS interface-like names: at least one letter and
// at least one digit, optionally separated by `-`, `_`, `.`, or `/` (for
// names like ether1, sfp-sfpplus1, ether1/2, wlan-2). This filter keeps the
// looser ".*?\b(...)\b" patterns from grabbing English connectives like
// "on", "to", or "port".
const portToken = `[A-Za-z][A-Za-z._/\-]*\d[A-Za-z0-9._/\-]*`

var (
	loopDetectedRe = regexp.MustCompile(`(?i)\bloop\s+detected\b.*?\b(?:on|at)\s+(` + portToken + `)`)
	macFlapPairRe  = regexp.MustCompile(`(?i)\b((?:[0-9A-F]{2}:){5}[0-9A-F]{2})\b.*?(?:moved\s+from|from)\s+(` + portToken + `)\s+to\s+(` + portToken + `)`)
	macFlapShortRe = regexp.MustCompile(`(?i)\bmac\s+flap\w*\b.*?\b((?:[0-9A-F]{2}:){5}[0-9A-F]{2})\b.*?\b(` + portToken + `)`)
	bpduEdgeRe     = regexp.MustCompile(`(?i)\bbpdu\b.*?\b(?:non-?edge|edge)\b.*?\b(` + portToken + `)`)
	bridgePrefixRe = regexp.MustCompile(`^([A-Za-z0-9._\-]+):\s+`)
)

// ParseBridgeLogMessage classifies a /log/print message body. Returns nil if
// the message does not look like a loop / flap signal.
func ParseBridgeLogMessage(msg string) *BridgeLogEvent {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return nil
	}

	// Many lines start with "<bridge>: ..." — peel that off but remember it.
	bridge := ""
	if m := bridgePrefixRe.FindStringSubmatch(msg); m != nil {
		bridge = m[1]
	}

	switch {
	case loopDetectedRe.MatchString(msg):
		m := loopDetectedRe.FindStringSubmatch(msg)
		return &BridgeLogEvent{Kind: "loop_detected", Bridge: bridge, Port: m[1]}

	case macFlapPairRe.MatchString(msg):
		m := macFlapPairRe.FindStringSubmatch(msg)
		return &BridgeLogEvent{
			Kind:      "mac_flap",
			Bridge:    bridge,
			MAC:       strings.ToUpper(m[1]),
			Port:      m[2],
			OtherPort: m[3],
		}

	case macFlapShortRe.MatchString(msg):
		m := macFlapShortRe.FindStringSubmatch(msg)
		return &BridgeLogEvent{
			Kind:   "mac_flap",
			Bridge: bridge,
			MAC:    strings.ToUpper(m[1]),
			Port:   m[2],
		}

	case bpduEdgeRe.MatchString(msg) && strings.Contains(strings.ToLower(msg), "edge"):
		m := bpduEdgeRe.FindStringSubmatch(msg)
		return &BridgeLogEvent{Kind: "bpdu_on_edge", Bridge: bridge, Port: m[1]}
	}

	return nil
}

// BridgeLogEventsFrom returns the loop / flap signals among a log read. The
// wireless parser consumes the same read (poller.LogReader hands both the
// same lines); each filters on the topic field.
func BridgeLogEventsFrom(entries []LogEntry) []BridgeLogEvent {
	out := make([]BridgeLogEvent, 0, 8)
	for i := range entries {
		e := &entries[i]
		if !isBridgeTopic(e.Topics) {
			continue
		}
		parsed := ParseBridgeLogMessage(e.Message)
		if parsed == nil {
			continue
		}
		parsed.Time = e.Time
		parsed.Topics = e.Topics
		parsed.Message = e.Message
		out = append(out, *parsed)
	}
	return out
}

// GetBridgeLogEvents reads the device's whole log buffer and returns its
// bridge anomaly events. One-shot callers only; pollers go through the shared
// cursored read in poller.LogReader.
func GetBridgeLogEvents(client *ros.Client) ([]BridgeLogEvent, error) {
	entries, err := ReadLog(ClientRunner{Client: client})
	if err != nil {
		return nil, err
	}
	return BridgeLogEventsFrom(entries), nil
}

func isBridgeTopic(topics string) bool {
	return hasTopic(topics, "bridge", "stp", "rstp")
}
