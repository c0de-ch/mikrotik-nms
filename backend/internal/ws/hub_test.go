package ws

import (
	"encoding/json"
	"testing"
	"time"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestHubDeliversToSubscribersOnly(t *testing.T) {
	h := NewHub()
	go h.Run()

	sub := NewClient(h, nil)   // subscribes to "alpha"
	other := NewClient(h, nil) // subscribes to "beta"
	h.Subscribe(sub, "alpha")
	h.Subscribe(other, "beta")
	waitFor(t, func() bool { return h.TopicSubscriberCount("alpha") == 1 && h.TopicSubscriberCount("beta") == 1 })

	h.Publish("alpha", map[string]any{"k": "v"})

	select {
	case payload := <-sub.send:
		var msg Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if msg.Topic != "alpha" {
			t.Fatalf("wrong topic: %q", msg.Topic)
		}
		if msg.Timestamp == "" {
			t.Fatal("timestamp should be set on publish")
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive the published message")
	}

	// The other client (subscribed to a different topic) must not receive it.
	select {
	case <-other.send:
		t.Fatal("non-subscriber received a message")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	go h.Run()

	c := NewClient(h, nil)
	h.Subscribe(c, "t")
	waitFor(t, func() bool { return h.TopicSubscriberCount("t") == 1 })
	h.Unsubscribe(c, "t")
	waitFor(t, func() bool { return h.TopicSubscriberCount("t") == 0 })

	h.Publish("t", map[string]any{"x": 1})
	select {
	case <-c.send:
		t.Fatal("received a message after unsubscribing")
	case <-time.After(50 * time.Millisecond):
	}
}
