package ws

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTopicAllowed(t *testing.T) {
	allowed := []string{
		"device.health", "topology.update", "topology.traffic", "traffic.ports",
		"firmware.update", "wifi.event", "network.health", "network.health.event",
		"connectivity.sample", "connectivity.speed", "connectivity.traceroute",
		"flows.flushed", "flows.status",
		"traffic.4c1f6a52-0d0e-4a57-9a56-2f4b1a1d6f10.ether1",
		"traffic.d1.sfp-sfpplus1",
		"traffic.d1.vlan 28 / guests", // interface names may hold spaces and slashes
		"traffic.d1.bond1.100",        // … and dots
		"upgrade.progress.7",
		"upgrade.progress.0b9c3c2e",
	}
	for _, topic := range allowed {
		if !TopicAllowed(topic) {
			t.Errorf("TopicAllowed(%q) = false, want true", topic)
		}
	}
	refused := []string{
		"", "device", "device.health.extra", "Device.Health", "traffic", "traffic.",
		"traffic.d1", "traffic.d1.", "traffic..ether1", "upgrade.progress", "upgrade.progress.",
		"flows", "flows.other", "*", "traffic.*",
		"traffic.d1." + strings.Repeat("x", maxTopicLen),
	}
	for _, topic := range refused {
		if TopicAllowed(topic) {
			t.Errorf("TopicAllowed(%q) = true, want false", topic)
		}
	}
	for _, topic := range Topics() {
		if !TopicAllowed(topic) {
			t.Errorf("fixed topic %q is not allowed", topic)
		}
	}
}

// nextFrame returns the next frame queued for c, or fails.
func nextFrame(t *testing.T, c *Client) Message {
	t.Helper()
	select {
	case payload := <-c.send:
		var msg Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return msg
	case <-time.After(time.Second):
		t.Fatal("no frame within a second")
	}
	return Message{}
}

func refusalOf(t *testing.T, msg Message) Refusal {
	t.Helper()
	raw, err := json.Marshal(msg.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var r Refusal
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("refusal data %s: %v", raw, err)
	}
	return r
}

// A subscribe to a name the server never publishes on is refused with a
// control frame and leaves no hub state behind.
func TestHubRefusesUnknownTopic(t *testing.T) {
	h := NewHub()
	go h.Run()
	c := NewClient(h, nil)

	h.Subscribe(c, "made.up")
	msg := nextFrame(t, c)
	if msg.Type != TypeSubscribeError || msg.Topic != "made.up" {
		t.Fatalf("frame = %+v, want a subscribe.error for made.up", msg)
	}
	if r := refusalOf(t, msg); r.Code != CodeUnknownTopic || r.Error == "" {
		t.Fatalf("refusal = %+v, want code %q with a message", r, CodeUnknownTopic)
	}
	if n := h.TopicSubscriberCount("made.up"); n != 0 {
		t.Fatalf("made.up has %d subscribers, want 0", n)
	}

	// The connection is still good for a real topic.
	h.Subscribe(c, "device.health")
	waitFor(t, func() bool { return h.TopicSubscriberCount("device.health") == 1 })
}

// One connection holds at most MaxSubscriptions topics: the next one is
// refused, a repeat of a held topic is not, and leaving a topic frees a slot.
func TestHubCapsSubscriptionsPerConnection(t *testing.T) {
	h := NewHub()
	go h.Run()
	c := NewClient(h, nil)
	stream := func(i int) string { return fmt.Sprintf("traffic.d1.ether%d", i) }

	for i := 0; i < MaxSubscriptions; i++ {
		h.Subscribe(c, stream(i))
	}
	waitFor(t, func() bool { return h.TopicSubscriberCount(stream(MaxSubscriptions-1)) == 1 })

	h.Subscribe(c, stream(MaxSubscriptions))
	msg := nextFrame(t, c)
	if msg.Type != TypeSubscribeError || msg.Topic != stream(MaxSubscriptions) {
		t.Fatalf("frame = %+v, want a subscribe.error for the topic over the cap", msg)
	}
	if r := refusalOf(t, msg); r.Code != CodeTooManySubscriptions || r.Max != MaxSubscriptions {
		t.Fatalf("refusal = %+v, want code %q max %d", r, CodeTooManySubscriptions, MaxSubscriptions)
	}
	if n := h.TopicSubscriberCount(stream(MaxSubscriptions)); n != 0 {
		t.Fatalf("the refused topic has %d subscribers, want 0", n)
	}

	// Subscribing again to a topic already held is a fresh request for its
	// owner (a new Seq), not another slot.
	before := h.SubscribersWithPrefix(stream(0))[stream(0)][0].Seq
	h.Subscribe(c, stream(0))
	waitFor(t, func() bool { return h.SubscribersWithPrefix(stream(0))[stream(0)][0].Seq != before })
	select {
	case payload := <-c.send:
		t.Fatalf("a repeated subscribe was answered with %s", payload)
	case <-time.After(30 * time.Millisecond):
	}

	// Another connection has a cap of its own.
	other := NewClient(h, nil)
	h.Subscribe(other, stream(MaxSubscriptions))
	waitFor(t, func() bool { return h.TopicSubscriberCount(stream(MaxSubscriptions)) == 1 })

	// Leaving one topic makes room for one more.
	h.Unsubscribe(c, stream(1))
	h.Subscribe(c, "device.health")
	waitFor(t, func() bool { return h.TopicSubscriberCount("device.health") == 1 })
	h.Subscribe(c, "wifi.event")
	msg = nextFrame(t, c)
	if r := refusalOf(t, msg); msg.Topic != "wifi.event" || r.Code != CodeTooManySubscriptions {
		t.Fatalf("frame = %+v, want wifi.event refused over the cap", msg)
	}
}

// An owner's refusal (UnsubscribeSeq) and a disconnect both give the slots
// back, so a reconnecting browser starts from zero.
func TestHubSubscriptionCountFollowsUnsubscribeAndDisconnect(t *testing.T) {
	h := NewHub()
	go h.Run()
	c := NewClient(h, nil)

	h.Subscribe(c, "traffic.d1.ether1")
	waitFor(t, func() bool { return h.TopicSubscriberCount("traffic.d1.ether1") == 1 })
	seq := h.SubscribersWithPrefix("traffic.d1.")["traffic.d1.ether1"][0].Seq

	h.UnsubscribeSeq(c, "traffic.d1.ether1", seq+1) // not the current subscribe: nothing happens
	h.UnsubscribeSeq(c, "traffic.d1.ether1", seq)
	h.Unsubscribe(c, "traffic.d1.ether1") // already gone: must not count twice
	waitFor(t, func() bool { return h.TopicSubscriberCount("traffic.d1.ether1") == 0 })
	h.Subscribe(c, "device.health")
	waitFor(t, func() bool { return h.TopicSubscriberCount("device.health") == 1 })
	h.mu.RLock()
	n := h.subCount[c]
	h.mu.RUnlock()
	if n != 1 {
		t.Fatalf("subscription count = %d, want 1", n)
	}

	h.Unregister(c)
	waitFor(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		_, held := h.subCount[c]
		return !held && len(h.subscribers) == 0
	})
}
