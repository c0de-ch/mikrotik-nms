package ws

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"
)

// Message is one frame the server sends. A data frame carries a topic's
// payload and no type. A control frame (Type set) tells one client about its
// own subscription to Topic and is never broadcast.
type Message struct {
	Topic     string      `json:"topic"`
	Type      string      `json:"type,omitempty"`
	Timestamp string      `json:"timestamp"`
	Data      interface{} `json:"data"`
}

// Control frame types. Most topics are never vetted and get neither; a
// topic whose server-side owner checks subscriptions (the per-interface
// traffic stream) answers each subscribe with one of them.
const (
	// TypeSubscribeAck confirms a subscription; Data says how the topic is
	// served (for a stream, its effective sampling period).
	TypeSubscribeAck = "subscribe.ack"
	// TypeSubscribeError refuses a subscription: the server has dropped it,
	// and Data says why and what to show instead.
	TypeSubscribeError = "subscribe.error"
)

type opKind int

const (
	opRegister opKind = iota
	opUnregister
	opSubscribe
	opUnsubscribe
)

// hubOp is one change to the hub's client or subscription state.
type hubOp struct {
	kind   opKind
	client *Client
	topic  string
	seq    uint64 // opUnsubscribe: drop only this subscription (0 = any)
}

// Subscription is one client's subscription to a topic as the hub applied
// it. Seq numbers every subscribe the hub applies, so a topic owner that
// answers subscriptions can tell a client that subscribed again — a fresh
// request — from the subscription it has already answered.
type Subscription struct {
	Client *Client
	Seq    uint64
}

type Hub struct {
	mu          sync.RWMutex
	clients     map[*Client]struct{}          // registered or subscribed, not yet unregistered
	subscribers map[string]map[*Client]uint64 // topic → client → Seq of its subscribe
	nextSeq     uint64

	// listeners are in-process subscribers (see Listen), keyed by topic and
	// listener id.
	listeners    map[string]map[uint64]func(any)
	nextListener uint64

	// ops is one channel for every state change, so the hub applies a
	// client's subscribe, unsubscribe and disconnect in the order it sent
	// them. (Separate channels let select reorder them, which could leave a
	// client subscribed to a topic it had already left.)
	ops chan hubOp
}

func NewHub() *Hub {
	return &Hub{
		clients:     make(map[*Client]struct{}),
		subscribers: make(map[string]map[*Client]uint64),
		listeners:   make(map[string]map[uint64]func(any)),
		ops:         make(chan hubOp, 256),
	}
}

func (h *Hub) Run() {
	for op := range h.ops {
		h.mu.Lock()
		switch op.kind {
		case opRegister:
			h.clients[op.client] = struct{}{}

		case opUnregister:
			for topic, clients := range h.subscribers {
				delete(clients, op.client)
				if len(clients) == 0 {
					delete(h.subscribers, topic)
				}
			}
			delete(h.clients, op.client)
			if !op.client.closed {
				op.client.closed = true
				close(op.client.send)
			}

		case opSubscribe:
			if op.client.closed {
				break // already disconnected; nothing would ever drain it
			}
			h.clients[op.client] = struct{}{}
			if h.subscribers[op.topic] == nil {
				h.subscribers[op.topic] = make(map[*Client]uint64)
			}
			// Every subscribe gets a new number, a repeated one included:
			// it is a new request for the topic's owner to answer.
			h.nextSeq++
			h.subscribers[op.topic][op.client] = h.nextSeq

		case opUnsubscribe:
			if clients, ok := h.subscribers[op.topic]; ok {
				if seq, ok := clients[op.client]; ok && (op.seq == 0 || op.seq == seq) {
					delete(clients, op.client)
				}
				if len(clients) == 0 {
					delete(h.subscribers, op.topic)
				}
			}
		}
		h.mu.Unlock()
	}
}

func (h *Hub) Register(client *Client) {
	h.ops <- hubOp{kind: opRegister, client: client}
}

func (h *Hub) Unregister(client *Client) {
	h.ops <- hubOp{kind: opUnregister, client: client}
}

func (h *Hub) Subscribe(client *Client, topic string) {
	h.ops <- hubOp{kind: opSubscribe, client: client, topic: topic}
}

func (h *Hub) Unsubscribe(client *Client, topic string) {
	h.ops <- hubOp{kind: opUnsubscribe, client: client, topic: topic}
}

// UnsubscribeSeq drops the client's subscription to topic only while it is
// still the one numbered seq. A topic owner refusing a subscription uses it,
// so the refusal can never take out a subscribe the client sent after it.
func (h *Hub) UnsubscribeSeq(client *Client, topic string, seq uint64) {
	h.ops <- hubOp{kind: opUnsubscribe, client: client, topic: topic, seq: seq}
}

// Publish sends a message to all clients subscribed to the topic, after
// handing data to the topic's in-process listeners.
func (h *Hub) Publish(topic string, data interface{}) {
	h.mu.RLock()
	var listeners []func(any)
	for _, fn := range h.listeners[topic] {
		listeners = append(listeners, fn)
	}
	clients := len(h.subscribers[topic])
	h.mu.RUnlock()

	// Listeners run outside the lock: one that published in turn would
	// otherwise take the read lock recursively, which deadlocks as soon as
	// Run is waiting to write.
	for _, fn := range listeners {
		fn(data)
	}
	if clients == 0 {
		return // in-process listeners only: skip the marshal
	}

	payload, err := json.Marshal(Message{
		Topic:     topic,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	})
	if err != nil {
		log.Printf("ws hub: marshal error: %v", err)
		return
	}

	// Hold the read lock across the whole send loop: Run() mutates the inner
	// map (delete on unregister) and closes client.send under the write lock,
	// so iterating the live map after RUnlock would race both (fatal concurrent
	// map iteration / send on closed channel). Sends are non-blocking, so
	// holding the lock here is cheap.
	h.mu.RLock()
	defer h.mu.RUnlock()

	for client := range h.subscribers[topic] {
		select {
		case client.send <- payload:
		default:
			// slow client, drop message
		}
	}
}

// Notify sends a control frame about topic to one client and reports whether
// it was queued. It is not when the client has disconnected or its send
// buffer is full — like any frame to a slow client — so a caller that must be
// heard (a refusal) can try again later.
func (h *Hub) Notify(client *Client, typ, topic string, data interface{}) bool {
	payload, err := json.Marshal(Message{
		Topic:     topic,
		Type:      typ,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Data:      data,
	})
	if err != nil {
		log.Printf("ws hub: marshal error: %v", err)
		return false
	}
	// Same reason as Publish: the read lock keeps Run from closing send
	// between the membership check and the send.
	h.mu.RLock()
	defer h.mu.RUnlock()
	if _, ok := h.clients[client]; !ok || client.closed {
		return false
	}
	select {
	case client.send <- payload:
		return true
	default:
		return false
	}
}

// Listen registers an in-process listener: fn receives every value published
// on topic, as the publisher passed it (never marshalled). A listener counts
// as a subscriber, so a publisher that only publishes while someone is
// subscribed keeps publishing for it. fn runs on the publisher's goroutine
// and must not block; it must treat the value as read-only, since WS clients
// may be sent the same value. The returned func removes the listener.
func (h *Hub) Listen(topic string, fn func(data any)) (cancel func()) {
	h.mu.Lock()
	h.nextListener++
	id := h.nextListener
	if h.listeners[topic] == nil {
		h.listeners[topic] = make(map[uint64]func(any))
	}
	h.listeners[topic][id] = fn
	h.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.listeners[topic], id)
			if len(h.listeners[topic]) == 0 {
				delete(h.listeners, topic)
			}
			h.mu.Unlock()
		})
	}
}

// TopicSubscriberCount returns the number of subscribers for a topic: WS
// clients plus in-process listeners.
func (h *Hub) TopicSubscriberCount(topic string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers[topic]) + len(h.listeners[topic])
}

// SubscribersWithPrefix returns the WS subscriptions of every topic that
// starts with prefix and has at least one, keyed by topic. Owners of a topic
// family use it to vet subscriptions (see the Type* control frames).
func (h *Hub) SubscribersWithPrefix(prefix string) map[string][]Subscription {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string][]Subscription)
	for topic, clients := range h.subscribers {
		if !strings.HasPrefix(topic, prefix) || len(clients) == 0 {
			continue
		}
		list := make([]Subscription, 0, len(clients))
		for c, seq := range clients {
			list = append(list, Subscription{Client: c, Seq: seq})
		}
		out[topic] = list
	}
	return out
}
