package ws

import "strings"

// MaxSubscriptions is the number of topics one connection may be subscribed
// to at a time. The UI needs a handful per page; the cap keeps one browser
// (or a script holding a token) from making the hub — and, through the
// per-interface stream topics, the devices — work for an unbounded set.
const MaxSubscriptions = 64

// maxTopicLen bounds a topic name. The longest legitimate one is a stream
// topic: "traffic." + a device id + "." + an interface name.
const maxTopicLen = 512

// Refusal codes of a subscribe.error frame the hub itself sends. (A topic's
// owner refuses with codes of its own, e.g. the stream's class caps.)
const (
	CodeUnknownTopic         = "unknown_topic"
	CodeTooManySubscriptions = "too_many_subscriptions"
)

// Refusal is the data of a subscribe.error frame sent by the hub.
type Refusal struct {
	Code  string `json:"code"`
	Error string `json:"error"`
	// Max is the per-connection cap (too_many_subscriptions only).
	Max int `json:"max,omitempty"`
}

// topics is every fixed topic the server publishes. A subscription to any
// other name is refused: nothing would ever be published on it, and each
// one would still cost hub state.
var topics = map[string]struct{}{
	"device.health":           {},
	"topology.update":         {},
	"topology.traffic":        {},
	"traffic.ports":           {},
	"firmware.update":         {},
	"wifi.event":              {},
	"network.health":          {},
	"network.health.event":    {},
	"connectivity.sample":     {},
	"connectivity.speed":      {},
	"connectivity.traceroute": {},
	"flows.flushed":           {},
	"flows.status":            {},
	"alerts.update":           {},
}

// Topic families: a fixed prefix followed by an id the subscriber names.
const (
	// streamPrefix + "<device id>.<interface>" is one interface's live
	// stream (vetted further by its owner, the traffic manager).
	streamPrefix = "traffic."
	// upgradePrefix + "<job id>" is one firmware job's progress.
	upgradePrefix = "upgrade.progress."
)

// Topics returns the fixed topic names, unordered.
func Topics() []string {
	out := make([]string, 0, len(topics))
	for t := range topics {
		out = append(out, t)
	}
	return out
}

// TopicAllowed reports whether topic is one the server can publish on: a
// fixed topic, an interface stream or a firmware job's progress.
func TopicAllowed(topic string) bool {
	if topic == "" || len(topic) > maxTopicLen {
		return false
	}
	if _, ok := topics[topic]; ok {
		return true
	}
	if rest, ok := strings.CutPrefix(topic, upgradePrefix); ok {
		return rest != ""
	}
	if rest, ok := strings.CutPrefix(topic, streamPrefix); ok {
		// "<device id>.<interface>": the device id has no dot; the interface
		// name is the rest and may contain anything.
		dev, iface, found := strings.Cut(rest, ".")
		return found && dev != "" && iface != ""
	}
	return false
}
