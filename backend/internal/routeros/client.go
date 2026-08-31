package routeros

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
)

// CommandTimeout bounds a single RouterOS API call so one hung device cannot
// stall a serial poller goroutine indefinitely.
var CommandTimeout = 30 * time.Second

// ReconnectCooldown bounds how often GetLive will redial a single device.
// Pollers call GetLive as often as once per second (live traffic), so without a
// floor here a device that is down would turn every poll cycle into a dial.
var ReconnectCooldown = 15 * time.Second

// dialSpec remembers how a device was last dialled so the Pool can re-establish
// a connection on its own, without every caller having to carry credentials.
// The credentials live in this process's memory either way — the pool is simply
// holding on to what Dial was already given.
type dialSpec struct {
	address  string
	port     int
	username string
	password string
	useTLS   bool
}

// Pool manages RouterOS API connections keyed by device ID.
type Pool struct {
	mu        sync.RWMutex
	clients   map[string]*ros.Client
	specs     map[string]dialSpec
	lastDial  map[string]time.Time
	verifyTLS bool
}

// NewPool creates a connection pool. verifyTLS controls whether RouterOS
// API-TLS connections validate the device certificate (false = skip, the
// historical behaviour, since RouterOS ships self-signed certs).
func NewPool(verifyTLS bool) *Pool {
	return &Pool{
		clients:   make(map[string]*ros.Client),
		specs:     make(map[string]dialSpec),
		lastDial:  make(map[string]time.Time),
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

// clientOwners maps a pooled client back to the Pool and device it belongs to.
// RunCommand uses it to evict a connection whose socket has failed, without
// having to thread the Pool through every command helper.
var clientOwners sync.Map // map[*ros.Client]clientOwner

type clientOwner struct {
	pool     *Pool
	deviceID string
}

// registerClient records the per-client state the Pool keeps outside its map:
// the serializing mutex and the owning pool/device.
func registerClient(p *Pool, deviceID string, c *ros.Client) {
	clientMutexes.Store(c, &sync.Mutex{})
	clientOwners.Store(c, clientOwner{pool: p, deviceID: deviceID})
}

// unregisterClient drops everything registerClient recorded.
func unregisterClient(c *ros.Client) {
	clientMutexes.Delete(c)
	clientOwners.Delete(c)
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
		unregisterClient(c)
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
	p.specs[deviceID] = dialSpec{
		address:  address,
		port:     port,
		username: username,
		password: password,
		useTLS:   useTLS,
	}
	registerClient(p, deviceID, client)
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
//
// Prefer GetLive: Get never redials, so after a device reboots (which evicts the
// dead client) it keeps returning nil until something calls EnsureConnection.
func (p *Pool) Get(deviceID string) *ros.Client {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.clients[deviceID]
}

// GetLive returns a usable connection for a device, redialling if the pooled
// one was evicted because its socket failed.
//
// This is what the pollers want. A device that reboots mid-session leaves a dead
// socket behind; RunCommand evicts it, and GetLive then re-establishes the
// session on the next cycle instead of leaving that device dark until the next
// (hourly) info refresh happens to call EnsureConnection.
//
// Redials are throttled per device by ReconnectCooldown and are single-attempt,
// so a device that is genuinely down costs one dial per cooldown rather than
// stalling the caller in a retry/backoff loop. Returns nil when no connection is
// available, which every caller already treats as "skip this device".
func (p *Pool) GetLive(deviceID string) *ros.Client {
	p.mu.RLock()
	c, ok := p.clients[deviceID]
	spec, hasSpec := p.specs[deviceID]
	last := p.lastDial[deviceID]
	p.mu.RUnlock()

	if ok {
		return c
	}
	if !hasSpec || time.Since(last) < ReconnectCooldown {
		return nil
	}

	// Claim the redial under the write lock so concurrent pollers don't all
	// dial the same device at once.
	p.mu.Lock()
	if c, ok := p.clients[deviceID]; ok {
		p.mu.Unlock()
		return c
	}
	if time.Since(p.lastDial[deviceID]) < ReconnectCooldown {
		p.mu.Unlock()
		return nil
	}
	p.lastDial[deviceID] = time.Now()
	p.mu.Unlock()

	client, err := p.Dial(deviceID, spec.address, spec.port, spec.username, spec.password, spec.useTLS)
	if err != nil {
		log.Printf("routeros: reconnect to %s failed: %v", spec.address, err)
		return nil
	}
	log.Printf("routeros: reconnected to %s", spec.address)
	return client
}

// closeIf drops the pooled connection for a device only if it is still the
// client the caller saw fail. Without the identity check a slow failing command
// could close a healthy connection that another goroutine just redialled.
func (p *Pool) closeIf(deviceID string, client *ros.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[deviceID]; ok && c == client {
		c.Close()
		unregisterClient(c)
		delete(p.clients, deviceID)
	}
}

// Close closes and removes a connection.
func (p *Pool) Close(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[deviceID]; ok {
		c.Close()
		unregisterClient(c)
		delete(p.clients, deviceID)
	}
}

// Forget closes a connection and discards the remembered dial parameters, so
// GetLive will not redial it. Use it for transient pool keys (such as the
// auto-follow verification dial) rather than Close, which deliberately keeps the
// dial spec so a rebooted device can be reconnected.
func (p *Pool) Forget(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[deviceID]; ok {
		c.Close()
		unregisterClient(c)
		delete(p.clients, deviceID)
	}
	delete(p.specs, deviceID)
	delete(p.lastDial, deviceID)
}

// CloseAll closes all connections.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, c := range p.clients {
		c.Close()
		unregisterClient(c)
		delete(p.clients, id)
	}
}

// evictFailedClient removes a pooled client whose connection is broken, so that
// nothing hands the dead socket out again.
//
// This is the guard against a device rebooting mid-session: the pool used to
// keep the stale client indefinitely, and every poller that fetched it with Get
// failed with "broken pipe" on the same socket until the process restarted.
// Clients not created by Pool.Dial (DialOnce) are not registered and are the
// caller's to close.
func evictFailedClient(client *ros.Client) {
	v, ok := clientOwners.Load(client)
	if !ok {
		return
	}
	owner := v.(clientOwner)
	owner.pool.closeIf(owner.deviceID, client)
}

// isConnError reports whether err means the underlying socket is unusable, as
// opposed to RouterOS rejecting the command itself (an unknown command or a bad
// argument must not cost us a working connection).
func isConnError(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, io.ErrClosedPipe),
		errors.Is(err, net.ErrClosed),
		errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.ECONNABORTED):
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	// go-routeros surfaces some transport failures as plain fmt.Errorf values,
	// so the wrapped-error checks above can miss them.
	s := err.Error()
	for _, frag := range []string{
		"broken pipe",
		"connection reset",
		"connection refused",
		"use of closed network connection",
		"unexpected EOF",
		"EOF",
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
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
		if isConnError(res.err) {
			evictFailedClient(client)
		}
		return res.reply, res.err
	case <-time.After(timeout):
		client.Close() // unblocks the goroutine's read; it then exits via the buffered chan
		evictFailedClient(client)
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
