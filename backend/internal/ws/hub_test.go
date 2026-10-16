package ws

import (
	"encoding/json"
	"strings"
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

// A client's subscribe and unsubscribe must apply in the order it sent them:
// with one channel per kind, select could apply the unsubscribe first and
// leave the client on a topic it had already left.
func TestHubAppliesClientOpsInOrder(t *testing.T) {
	h := NewHub()
	go h.Run()

	c := NewClient(h, nil)
	for range 200 {
		h.Subscribe(c, "t")
		h.Unsubscribe(c, "t")
	}
	// A sentinel subscription behind the burst: once it shows, every earlier
	// op has been applied.
	h.Subscribe(c, "sentinel")
	waitFor(t, func() bool { return h.TopicSubscriberCount("sentinel") == 1 })
	if n := h.TopicSubscriberCount("t"); n != 0 {
		t.Fatalf("subscribers of t after subscribe/unsubscribe pairs = %d, want 0", n)
	}
}

func TestHubNotifyReachesOnlyThatLiveClient(t *testing.T) {
	h := NewHub()
	go h.Run()

	a := NewClientFor(h, nil, ClientInfo{Username: "alice", Role: "admin"})
	b := NewClient(h, nil)
	h.Register(a)
	h.Register(b)
	h.Subscribe(a, "x")
	h.Subscribe(b, "x")
	waitFor(t, func() bool { return h.TopicSubscriberCount("x") == 2 })

	h.Notify(a, TypeSubscribeError, "x", map[string]string{"code": "admin_only"})
	select {
	case payload := <-a.send:
		var msg Message
		if err := json.Unmarshal(payload, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type != TypeSubscribeError || msg.Topic != "x" {
			t.Fatalf("control frame = %+v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("notified client got nothing")
	}
	select {
	case <-b.send:
		t.Fatal("a control frame leaked to another subscriber")
	case <-time.After(30 * time.Millisecond):
	}

	// After a disconnect a notify is dropped, never sent on the closed
	// channel, and a second disconnect must not close it twice.
	h.Unregister(a)
	h.Unregister(a)
	waitFor(t, func() bool { return h.TopicSubscriberCount("x") == 1 })
	h.Notify(a, TypeSubscribeAck, "x", nil)

	if got := a.Info().Username; got != "alice" {
		t.Errorf("Info().Username = %q", got)
	}
	if a.ID() == b.ID() || a.ID() == 0 {
		t.Errorf("client ids not unique: %d %d", a.ID(), b.ID())
	}
}

// Data frames carry no type, so a client can tell them from control frames.
func TestHubDataFrameHasNoType(t *testing.T) {
	h := NewHub()
	go h.Run()
	c := NewClient(h, nil)
	h.Subscribe(c, "d")
	waitFor(t, func() bool { return h.TopicSubscriberCount("d") == 1 })
	h.Publish("d", 1)
	select {
	case payload := <-c.send:
		if strings.Contains(string(payload), `"type"`) {
			t.Fatalf("data frame carries a type: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("no data frame")
	}
}

func TestHubListenersSeeValuesAndCountAsSubscribers(t *testing.T) {
	h := NewHub()
	go h.Run()

	type snap struct{ N int }
	var got []snap
	cancel := h.Listen("feed", func(v any) { got = append(got, v.(snap)) })
	if n := h.TopicSubscriberCount("feed"); n != 1 {
		t.Fatalf("listener not counted: %d", n)
	}
	h.Publish("feed", snap{N: 7}) // no WS client: only the listener runs
	if len(got) != 1 || got[0].N != 7 {
		t.Fatalf("listener got %+v, want the published value", got)
	}

	cancel()
	cancel() // idempotent
	if n := h.TopicSubscriberCount("feed"); n != 0 {
		t.Fatalf("subscribers after cancel = %d", n)
	}
	h.Publish("feed", snap{N: 8})
	if len(got) != 1 {
		t.Fatalf("cancelled listener still called: %+v", got)
	}
}

// A listener may publish in turn (a derived topic) without deadlocking.
func TestHubListenerMayPublish(t *testing.T) {
	h := NewHub()
	go h.Run()
	c := NewClient(h, nil)
	h.Subscribe(c, "derived")
	waitFor(t, func() bool { return h.TopicSubscriberCount("derived") == 1 })
	defer h.Listen("source", func(v any) { h.Publish("derived", v) })()

	done := make(chan struct{})
	go func() {
		h.Publish("source", 42)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publishing from a listener deadlocked")
	}
	select {
	case <-c.send:
	case <-time.After(time.Second):
		t.Fatal("derived topic not delivered")
	}
}

func TestHubSubscribersWithPrefix(t *testing.T) {
	h := NewHub()
	go h.Run()
	a, b := NewClient(h, nil), NewClient(h, nil)
	h.Subscribe(a, "traffic.d1.ether1")
	h.Subscribe(b, "traffic.d1.ether1")
	h.Subscribe(a, "traffic.ports")
	h.Subscribe(a, "device.health")
	waitFor(t, func() bool { return h.TopicSubscriberCount("device.health") == 1 })

	got := h.SubscribersWithPrefix("traffic.")
	if len(got) != 2 || len(got["traffic.d1.ether1"]) != 2 || len(got["traffic.ports"]) != 1 {
		t.Fatalf("SubscribersWithPrefix = %v", got)
	}
	if _, ok := got["device.health"]; ok {
		t.Fatal("a topic outside the prefix was returned")
	}
}
