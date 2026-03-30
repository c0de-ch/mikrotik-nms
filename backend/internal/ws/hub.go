package ws

import (
	"encoding/json"
	"log"
	"sync"
)

type Message struct {
	Topic     string      `json:"topic"`
	Timestamp string      `json:"timestamp"`
	Data      interface{} `json:"data"`
}

type subscription struct {
	client *Client
	topic  string
}

type Hub struct {
	mu          sync.RWMutex
	subscribers map[string]map[*Client]struct{}
	register    chan *Client
	unregister  chan *Client
	subscribe   chan subscription
	unsubscribe chan subscription
}

func NewHub() *Hub {
	return &Hub{
		subscribers: make(map[string]map[*Client]struct{}),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		subscribe:   make(chan subscription, 256),
		unsubscribe: make(chan subscription, 256),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			_ = client // tracked via subscriptions

		case client := <-h.unregister:
			h.mu.Lock()
			for topic, clients := range h.subscribers {
				delete(clients, client)
				if len(clients) == 0 {
					delete(h.subscribers, topic)
				}
			}
			h.mu.Unlock()
			close(client.send)

		case sub := <-h.subscribe:
			h.mu.Lock()
			if h.subscribers[sub.topic] == nil {
				h.subscribers[sub.topic] = make(map[*Client]struct{})
			}
			h.subscribers[sub.topic][sub.client] = struct{}{}
			h.mu.Unlock()

		case sub := <-h.unsubscribe:
			h.mu.Lock()
			if clients, ok := h.subscribers[sub.topic]; ok {
				delete(clients, sub.client)
				if len(clients) == 0 {
					delete(h.subscribers, sub.topic)
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) Register(client *Client) {
	h.register <- client
}

func (h *Hub) Unregister(client *Client) {
	h.unregister <- client
}

func (h *Hub) Subscribe(client *Client, topic string) {
	h.subscribe <- subscription{client: client, topic: topic}
}

func (h *Hub) Unsubscribe(client *Client, topic string) {
	h.unsubscribe <- subscription{client: client, topic: topic}
}

// Publish sends a message to all clients subscribed to the topic.
func (h *Hub) Publish(topic string, data interface{}) {
	msg := Message{
		Topic: topic,
		Data:  data,
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		log.Printf("ws hub: marshal error: %v", err)
		return
	}

	h.mu.RLock()
	clients := h.subscribers[topic]
	h.mu.RUnlock()

	for client := range clients {
		select {
		case client.send <- payload:
		default:
			// slow client, drop message
		}
	}
}

// TopicSubscriberCount returns the number of subscribers for a topic.
func (h *Hub) TopicSubscriberCount(topic string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers[topic])
}
