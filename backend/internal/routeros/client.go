package routeros

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
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

// JoinHostPort builds a dial target from a host and port. Unlike a bare
// "host:port" format it brackets an IPv6 literal correctly (so an IPv6 address
// doesn't fail with "too many colons in address"), and it strips one pair of
// surrounding brackets first so an already-bracketed literal (e.g. "[2001:db8::1]"
// entered by hand) isn't double-wrapped into "[[...]]". For an IPv4 address or a
// hostname the result is identical to "host:port".
func JoinHostPort(host string, port int) string {
	h := strings.TrimSpace(host)
	if len(h) >= 2 && h[0] == '[' && h[len(h)-1] == ']' {
		h = h[1 : len(h)-1]
	}
	return net.JoinHostPort(h, strconv.Itoa(port))
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

	addr := JoinHostPort(address, port)

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

// DialOnce opens a one-shot dedicated connection to a RouterOS device,
// mirroring Pool.Dial's dial logic (TLS handling, IPv6-safe host:port) but NOT
// registering the client in clientMutexes and NOT pooling it. The caller owns
// the connection and must Close() it.
//
// Intended for long-running commands (speed-test /tool/fetch downloads,
// /tool/traceroute) that would otherwise hold the shared per-client mutex past
// CommandTimeout and force-close the pooled connection out from under every
// other poller.
func DialOnce(address string, port int, username, password string, useTLS, verifyTLS bool) (*ros.Client, error) {
	addr := JoinHostPort(address, port)

	var client *ros.Client
	var err error
	if useTLS {
		client, err = ros.DialTLS(addr, username, password, &tls.Config{
			InsecureSkipVerify: !verifyTLS, //nolint:gosec // self-signed certs are common; opt-in verification via MIKROTIK_NMS_ROS_TLS_VERIFY
		})
	} else {
		client, err = ros.Dial(addr, username, password)
	}
	if err != nil {
		return nil, fmt.Errorf("routeros dial %s: %w", addr, err)
	}
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
	return RunCommandWithTimeout(client, CommandTimeout, command, args...)
}

// RunCommandWithTimeout is RunCommand with a caller-chosen timeout, for
// commands that legitimately run longer than CommandTimeout (speed-test
// downloads, traceroutes). It acquires the per-client mutex IF the client was
// registered via Pool.Dial — dedicated DialOnce clients aren't, which is fine
// as long as they are used from a single goroutine.
func RunCommandWithTimeout(client *ros.Client, timeout time.Duration, command string, args ...string) (*ros.Reply, error) {
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
	case <-time.After(timeout):
		client.Close() // unblocks the goroutine's read; it then exits via the buffered chan
		return nil, fmt.Errorf("routeros command %q timed out after %s", command, timeout)
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
