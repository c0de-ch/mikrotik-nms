package ws

import (
	"context"
	"encoding/json"
	"log"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 30 * time.Second
	maxMsgSize = 4096
	sendBufLen = 256
)

// ClientInfo is the authenticated user behind a connection. Topic owners
// that vet subscriptions read it (e.g. a stream only admins may open).
type ClientInfo struct {
	UserID   string
	Username string
	Role     string
}

// clientSeq numbers connections for the life of the process.
var clientSeq atomic.Uint64

type Client struct {
	id   uint64
	info ClientInfo
	hub  *Hub
	conn *websocket.Conn
	send chan []byte

	// closed is set when the hub closes send (under hub.mu), so nothing
	// sends on or closes it again.
	closed bool
}

type clientMessage struct {
	Action string `json:"action"` // "subscribe" or "unsubscribe"
	Topic  string `json:"topic"`
}

// NewClient returns a connection with no user attached.
func NewClient(hub *Hub, conn *websocket.Conn) *Client {
	return NewClientFor(hub, conn, ClientInfo{})
}

// NewClientFor returns a connection opened by the given user.
func NewClientFor(hub *Hub, conn *websocket.Conn, info ClientInfo) *Client {
	return &Client{
		id:   clientSeq.Add(1),
		info: info,
		hub:  hub,
		conn: conn,
		send: make(chan []byte, sendBufLen),
	}
}

// ID is unique per connection for the life of the process.
func (c *Client) ID() uint64 { return c.id }

// Info is the user the connection was opened for.
func (c *Client) Info() ClientInfo { return c.info }

// ReadPump reads messages from the WebSocket connection.
func (c *Client) ReadPump(ctx context.Context) {
	defer func() {
		c.hub.Unregister(c)
		c.conn.Close(websocket.StatusNormalClosure, "")
	}()

	c.conn.SetReadLimit(maxMsgSize)

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				log.Printf("ws read error: %v", err)
			}
			return
		}

		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("ws invalid message: %v", err)
			continue
		}

		switch msg.Action {
		case "subscribe":
			if msg.Topic != "" {
				c.hub.Subscribe(c, msg.Topic)
			}
		case "unsubscribe":
			if msg.Topic != "" {
				c.hub.Unsubscribe(c, msg.Topic)
			}
		}
	}
}

// WritePump writes messages to the WebSocket connection.
func (c *Client) WritePump(ctx context.Context) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close(websocket.StatusNormalClosure, "")
	}()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, writeWait)
			err := c.conn.Write(writeCtx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}

		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, writeWait)
			err := c.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return
			}

		case <-ctx.Done():
			return
		}
	}
}
