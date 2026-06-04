package routeros

import (
	"regexp"
	"strings"

	ros "github.com/go-routeros/routeros/v3"
)

// WirelessLogEvent is a parsed wireless/CAPsMAN/wifi log entry.
type WirelessLogEvent struct {
	// Raw fields from /log/print
	Time    string // RouterOS time field, e.g. "13:48:32" or "apr/08 13:48:32"
	Topics  string // e.g. "wireless,info"
	Message string // raw message text

	// Parsed fields. Event is one of "connected", "disconnected", "roamed",
	// "reconnecting", or "" if the message could not be parsed.
	Event   string
	MAC     string
	AP      string
	SSID    string
	Reason  string // e.g. "connection lost", "not responding"
	Signal  string // dBm string, e.g. "-30"
	ToMAC   string // populated for "roamed" events
	ToAP    string // populated for "roamed" events
	ToSSID  string // populated for "roamed" events
}

// Fingerprint returns a stable string identifying this log entry within a
// single device's log buffer. Used for deduping across polls.
func (e *WirelessLogEvent) Fingerprint() string {
	return e.Time + "\x1f" + e.Message
}

// Regexes are compiled once. The AP name can contain dashes/letters/digits but
// no parens; the SSID is captured separately inside the parens.
var (
	logHeaderRe = regexp.MustCompile(`^([0-9A-Fa-f:]{17})@([^()]+)\(([^)]*)\)\s+(.*)$`)
	roamTailRe  = regexp.MustCompile(`^roamed to\s+([0-9A-Fa-f:]{17})@([^()]+)\(([^)]*)\)(?:,\s*signal strength\s*(-?\d+))?\s*$`)
	signalTailRe = regexp.MustCompile(`,\s*signal strength\s*(-?\d+)\s*$`)
)

// ParseWirelessLogMessage parses the message portion of a /log/print entry
// into a structured WirelessLogEvent. Returns nil if the message does not look
// like a wireless event.
//
// Supported formats (RouterOS 7.x wireless,info topic):
//
//	MAC@AP(SSID) connected, signal strength -30
//	MAC@AP(SSID) disconnected, connection lost, signal strength -35
//	MAC@AP(SSID) disconnected, not responding, signal strength -49
//	MAC@AP(SSID) reconnecting, signal strength -68
//	MAC@AP(SSID) roamed to MAC@AP(SSID), signal strength -74
func ParseWirelessLogMessage(msg string) *WirelessLogEvent {
	m := logHeaderRe.FindStringSubmatch(strings.TrimSpace(msg))
	if m == nil {
		return nil
	}
	ev := &WirelessLogEvent{
		MAC:  strings.ToUpper(m[1]),
		AP:   m[2],
		SSID: m[3],
	}
	rest := strings.TrimSpace(m[4])

	// Pull off a trailing ", signal strength -NN" if present.
	if sm := signalTailRe.FindStringSubmatch(rest); sm != nil {
		ev.Signal = sm[1]
		rest = strings.TrimSpace(rest[:len(rest)-len(sm[0])])
	}

	switch {
	case strings.HasPrefix(rest, "connected"):
		ev.Event = "connected"
	case strings.HasPrefix(rest, "reconnecting"):
		ev.Event = "reconnecting"
	case strings.HasPrefix(rest, "disconnected"):
		ev.Event = "disconnected"
		// rest looks like "disconnected" or "disconnected, connection lost"
		if i := strings.Index(rest, ","); i >= 0 {
			ev.Reason = strings.TrimSpace(rest[i+1:])
		}
	case strings.HasPrefix(rest, "roamed to"):
		// Re-parse the roam tail (signal was already stripped above, so add a
		// trailing space if it was just stripped to make the regex anchor work).
		tail := rest
		if ev.Signal != "" {
			tail = rest + ", signal strength " + ev.Signal
		}
		rm := roamTailRe.FindStringSubmatch(tail)
		if rm == nil {
			return nil
		}
		ev.Event = "roamed"
		ev.ToMAC = strings.ToUpper(rm[1])
		ev.ToAP = rm[2]
		ev.ToSSID = rm[3]
		if ev.Signal == "" && rm[4] != "" {
			ev.Signal = rm[4]
		}
	default:
		return nil
	}
	return ev
}

// GetWirelessLogEvents fetches recent log entries from a RouterOS device and
// returns those that look like wireless/CAPsMAN/wifi client events. The
// returned slice is in the order RouterOS produced it (oldest first).
//
// We deliberately do not use a server-side topics filter because the topic
// name varies across RouterOS versions ("wireless", "wifi", "caps") and the
// API does not support a portable OR filter. The log buffer is small enough
// that pulling everything once per poll cycle is cheap.
func GetWirelessLogEvents(client *ros.Client) ([]WirelessLogEvent, error) {
	reply, err := RunCommand(client, "/log/print")
	if err != nil {
		return nil, err
	}
	out := make([]WirelessLogEvent, 0, 32)
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		topics := m["topics"]
		if !isWirelessTopic(topics) {
			continue
		}
		parsed := ParseWirelessLogMessage(m["message"])
		if parsed == nil {
			continue
		}
		parsed.Time = m["time"]
		parsed.Topics = topics
		parsed.Message = m["message"]
		out = append(out, *parsed)
	}
	return out, nil
}

func isWirelessTopic(topics string) bool {
	if topics == "" {
		return false
	}
	// topics is a comma-separated list like "wireless,info" or "wifi,info,debug".
	for _, t := range strings.Split(topics, ",") {
		switch strings.TrimSpace(t) {
		case "wireless", "wifi", "caps":
			return true
		}
	}
	return false
}
