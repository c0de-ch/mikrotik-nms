package routeros

import (
	"crypto/tls"
	"fmt"
	"log"
	"sync"
	"time"

	ros "github.com/go-routeros/routeros"
	"github.com/go-routeros/routeros/proto"
)

// Pool manages RouterOS API connections keyed by device ID.
type Pool struct {
	mu      sync.RWMutex
	clients map[string]*ros.Client
}

func NewPool() *Pool {
	return &Pool{
		clients: make(map[string]*ros.Client),
	}
}

// Dial connects to a RouterOS device and stores the connection.
func (p *Pool) Dial(deviceID, address string, port int, username, password string, useTLS bool) (*ros.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Close existing connection if any
	if c, ok := p.clients[deviceID]; ok {
		c.Close()
		delete(p.clients, deviceID)
	}

	addr := fmt.Sprintf("%s:%d", address, port)

	var client *ros.Client
	var err error

	if useTLS {
		client, err = ros.DialTLS(addr, username, password, &tls.Config{
			InsecureSkipVerify: true, // RouterOS uses self-signed certs
		})
	} else {
		client, err = ros.Dial(addr, username, password)
	}
	if err != nil {
		return nil, fmt.Errorf("routeros dial %s: %w", addr, err)
	}

	p.clients[deviceID] = client
	return client, nil
}

// Get returns an existing connection or nil.
func (p *Pool) Get(deviceID string) *ros.Client {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.clients[deviceID]
}

// Close closes and removes a connection.
func (p *Pool) Close(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[deviceID]; ok {
		c.Close()
		delete(p.clients, deviceID)
	}
}

// CloseAll closes all connections.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, c := range p.clients {
		c.Close()
		delete(p.clients, id)
	}
}

// RunCommand executes a RouterOS command and returns the reply sentences.
func RunCommand(client *ros.Client, command string, args ...string) (*ros.Reply, error) {
	return client.RunArgs(append([]string{command}, args...))
}

// GetSentenceMap returns the key-value map from a RouterOS reply sentence.
func GetSentenceMap(s *proto.Sentence) map[string]string {
	if s.Map != nil {
		return s.Map
	}
	return make(map[string]string)
}

// KeepAlive sends a lightweight command to keep the connection alive.
func KeepAlive(client *ros.Client) error {
	_, err := RunCommand(client, "/system/identity/print")
	return err
}

// EnsureConnection gets or establishes a connection to a device.
func (p *Pool) EnsureConnection(deviceID, address string, port int, username, password string, useTLS bool) (*ros.Client, error) {
	if c := p.Get(deviceID); c != nil {
		// Test if connection is alive
		if err := KeepAlive(c); err == nil {
			return c, nil
		}
		log.Printf("routeros: stale connection to %s, reconnecting", address)
		p.Close(deviceID)
	}

	// Retry with backoff
	var lastErr error
	for attempt := range 3 {
		client, err := p.Dial(deviceID, address, port, username, password, useTLS)
		if err == nil {
			return client, nil
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return nil, lastErr
}
