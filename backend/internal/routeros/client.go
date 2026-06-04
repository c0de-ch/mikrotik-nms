package routeros

import (
	"crypto/tls"
	"fmt"
	"log"
	"sync"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
)

// CommandTimeout bounds a single RouterOS API call so one hung device cannot
// stall a serial poller goroutine indefinitely.
var CommandTimeout = 30 * time.Second

// Pool manages RouterOS API connections keyed by device ID.
type Pool struct {
	mu        sync.RWMutex
	clients   map[string]*ros.Client
	verifyTLS bool
}

// NewPool creates a connection pool. verifyTLS controls whether RouterOS
// API-TLS connections validate the device certificate (false = skip, the
// historical behaviour, since RouterOS ships self-signed certs).
func NewPool(verifyTLS bool) *Pool {
	return &Pool{
		clients:   make(map[string]*ros.Client),
		verifyTLS: verifyTLS,
	}
}

// clientMutexes serializes access to each *ros.Client. The go-routeros
// library wraps a single bufio.Reader per connection, which is NOT safe
// for concurrent use. Two pollers calling RunArgs on the same client
// from different goroutines corrupt the buffered reader and produce
// "slice bounds out of range [:N] with capacity 4096" panics.
//
// The Pool registers a mutex on Dial and releases it on Close.
// RunCommand looks up and acquires the mutex; clients that weren't
// registered (one-shot connections created outside the Pool, e.g. the
// device test endpoint) run unlocked, which is safe as long as they
// are used from a single goroutine.
var clientMutexes sync.Map // map[*ros.Client]*sync.Mutex

func registerClientLock(c *ros.Client) {
	clientMutexes.Store(c, &sync.Mutex{})
}

func releaseClientLock(c *ros.Client) {
	clientMutexes.Delete(c)
}

// Dial connects to a RouterOS device and stores the connection.
func (p *Pool) Dial(deviceID, address string, port int, username, password string, useTLS bool) (*ros.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Close existing connection if any
	if c, ok := p.clients[deviceID]; ok {
		c.Close()
		releaseClientLock(c)
		delete(p.clients, deviceID)
	}

	addr := fmt.Sprintf("%s:%d", address, port)

	var client *ros.Client
	var err error

	if useTLS {
		client, err = ros.DialTLS(addr, username, password, &tls.Config{
			InsecureSkipVerify: !p.verifyTLS, //nolint:gosec // self-signed certs are common; opt-in verification via MIKROTIK_NMS_ROS_TLS_VERIFY
		})
	} else {
		client, err = ros.Dial(addr, username, password)
	}
	if err != nil {
		return nil, fmt.Errorf("routeros dial %s: %w", addr, err)
	}

	p.clients[deviceID] = client
	registerClientLock(client)
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
		releaseClientLock(c)
		delete(p.clients, deviceID)
	}
}

// CloseAll closes all connections.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, c := range p.clients {
		c.Close()
		releaseClientLock(c)
		delete(p.clients, id)
	}
}

// RunCommand executes a RouterOS command and returns the reply sentences.
// Serializes access to clients that were created via Pool.Dial so multiple
// pollers cannot trigger concurrent reads on the same bufio.Reader.
//
// The call is bounded by CommandTimeout: a hung device would otherwise block
// the calling (serial) poller goroutine forever while holding the per-client
// mutex. On timeout the client is closed, which unblocks the in-flight read so
// the helper goroutine can exit, and the next EnsureConnection redials.
func RunCommand(client *ros.Client, command string, args ...string) (*ros.Reply, error) {
	if v, ok := clientMutexes.Load(client); ok {
		mu := v.(*sync.Mutex)
		mu.Lock()
		defer mu.Unlock()
	}

	type result struct {
		reply *ros.Reply
		err   error
	}
	done := make(chan result, 1)
	go func() {
		reply, err := client.RunArgs(append([]string{command}, args...))
		done <- result{reply, err}
	}()

	select {
	case res := <-done:
		return res.reply, res.err
	case <-time.After(CommandTimeout):
		client.Close() // unblocks the goroutine's read; it then exits via the buffered chan
		return nil, fmt.Errorf("routeros command %q timed out after %s", command, CommandTimeout)
	}
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
