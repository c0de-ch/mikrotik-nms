package routeros

import (
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	ros "github.com/go-routeros/routeros/v3"
)

// ReconnectCooldown is the least time between two automatic dials of one
// device, and the first step of the backoff after a failed one. Pollers call
// GetLive as often as once per second (live traffic), so without a floor a
// device that is down would turn every poll cycle into a dial.
var ReconnectCooldown = 15 * time.Second

// ReconnectBackoffMax is where the backoff after repeated failures stops
// growing: 15 s, 30 s, 1 min, … 10 min, each ±backoffJitter.
var ReconnectBackoffMax = 10 * time.Minute

// backoffJitter spreads the retries of a fleet that lost its devices at the
// same moment (a switch reboot, a power cut): ±20 %.
const backoffJitter = 0.2

// dialSpec remembers how a device was last dialled so the Pool can re-establish
// a connection on its own, without every caller having to carry credentials.
// The credentials live in this process's memory either way — the pool is simply
// holding on to what Dial was already given.
type dialSpec struct {
	target   Target
	username string
	password string
}

// dialFlight is one dial of a pool key in progress. Callers that want the
// same session while it runs wait for it instead of dialling again.
type dialFlight struct {
	spec dialSpec
	done chan struct{}
	// superseded is set by Forget: the device was edited or removed while
	// the dial ran, so its result is discarded.
	superseded bool
	client     *ros.Client
	err        error
}

// backoffState is what the pool remembers about the dials of one key.
type backoffState struct {
	failures int
	// next is the earliest time of the next automatic dial (GetLive,
	// EnsureConnection). An explicit Dial is never held back by it.
	next time.Time
	// lastErr is the error of the last failed dial, nil after a success.
	lastErr error
}

// Pool manages RouterOS API connections keyed by device ID.
//
// The pool's lock is never held across a dial or a close: both are network
// I/O, and one unreachable device must not stall the pollers of every other.
type Pool struct {
	mu      sync.Mutex
	clients map[string]*ros.Client
	specs   map[string]dialSpec
	flights map[string]*dialFlight
	backoff map[string]*backoffState

	// Seams for tests: the clock, the jitter (a factor in [-1, 1)), the
	// dial itself, and a callback when a caller starts waiting for a dial
	// already in flight.
	now    func() time.Time
	jitter func() float64
	dialer func(spec dialSpec) (*ros.Client, error)
	joined func()
}

// NewPool creates a connection pool.
func NewPool() *Pool {
	return &Pool{
		clients: make(map[string]*ros.Client),
		specs:   make(map[string]dialSpec),
		flights: make(map[string]*dialFlight),
		backoff: make(map[string]*backoffState),
		now:     time.Now,
		jitter:  func() float64 { return 2*rand.Float64() - 1 },
		dialer: func(spec dialSpec) (*ros.Client, error) {
			return dialSession(spec.target, spec.username, spec.password)
		},
	}
}

// clientOwners maps a pooled client back to the Pool and key it belongs to,
// and to the device its commands are counted against. Closing the client's
// connection (sessionConn) uses it to take the client out of its pool,
// without having to thread the Pool through every command helper.
var clientOwners sync.Map // map[*ros.Client]clientOwner

// sessionTags maps a one-shot session to the device its commands are counted
// against (TagSession); pooled sessions are found through clientOwners.
var sessionTags sync.Map // map[*ros.Client]string

type clientOwner struct {
	pool *Pool
	// key is the pool key the client is stored under.
	key string
	// deviceID is what the session is observed as: the stored device the
	// dial named (Target.DeviceID), or the pool key.
	deviceID string
}

// observedAs is the device a pooled session of key counts against.
func observedAs(key string, t Target) string {
	if t.DeviceID != "" {
		return t.DeviceID
	}
	return key
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

// Dial connects to a RouterOS device and stores the connection under key,
// replacing the one stored there. It is the explicit dial — an admin action,
// a test — and is never held back by the backoff of earlier failures, though
// its outcome counts for it. The dial policy, the stored secret and the TLS
// pin are checked first, so a refused target never evicts the existing
// connection.
func (p *Pool) Dial(key string, t Target, username, password string) (*ros.Client, error) {
	if err := checkDial(t, password); err != nil {
		return nil, err
	}
	return p.connect(key, dialSpec{target: t, username: username, password: password})
}

// connect opens the session for key with spec, outside the pool lock.
// Callers asking for the same key with the same spec while it runs share its
// result: two pollers that find a device without a session cause one dial.
func (p *Pool) connect(key string, spec dialSpec) (*ros.Client, error) {
	var flight *dialFlight
	for {
		p.mu.Lock()
		running := p.flights[key]
		if running == nil {
			flight = &dialFlight{spec: spec, done: make(chan struct{})}
			p.flights[key] = flight
			break
		}
		p.mu.Unlock()
		if p.joined != nil {
			p.joined()
		}
		<-running.done
		if running.spec == spec {
			return running.client, running.err
		}
		// A dial with other parameters finished; ours is next.
	}
	// Remembered from the first attempt on, so that GetLive can retry a
	// device that was down when it was first dialled.
	p.specs[key] = spec
	old := p.clients[key]
	delete(p.clients, key)
	p.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	// A pooled session belongs to the device the dial names, or to the one
	// its key names: that is whose class bounds the handshake, and whom the
	// session and a pin mismatch are counted against.
	as := spec
	as.target.DeviceID = observedAs(key, spec.target)
	client, err := p.dialContained(as)
	observeSession(as.target.DeviceID, err)
	if err == nil {
		clientOwners.Store(client, clientOwner{pool: p, key: key, deviceID: as.target.DeviceID})
	}

	p.mu.Lock()
	if p.flights[key] == flight {
		delete(p.flights, key)
	}
	var discard *ros.Client
	switch {
	case flight.superseded:
		// Forget ran while the dial did: the session was opened with
		// parameters that no longer apply.
		if err == nil {
			discard, client = client, nil
		}
		err = ErrDialSuperseded
	case err == nil:
		p.clients[key] = client
		p.backoff[key] = &backoffState{next: p.now().Add(ReconnectCooldown)}
	default:
		p.noteFailureLocked(key, err)
	}
	p.mu.Unlock()

	if discard != nil {
		_ = discard.Close()
	}
	flight.client, flight.err = client, err
	close(flight.done)
	return client, err
}

// dialContained runs the dial and turns a panic in it into an error. The
// callers that wait for this dial wait on its flight: were a panic to unwind
// past connect, the flight would never finish, and every later GetLive of the
// device would wait on it for ever.
func (p *Pool) dialContained(spec dialSpec) (client *ros.Client, err error) {
	defer func() {
		if r := recover(); r != nil {
			client, err = nil, fmt.Errorf("routeros dial %s: panic: %v", spec.target.Address, r)
		}
	}()
	return p.dialer(spec)
}

// ErrDialSuperseded is the result of a dial that was still running when its
// device was edited or removed (Forget): the session it opened is closed.
var ErrDialSuperseded = errors.New("routeros: the dial was overtaken by a change of the device")

// noteFailureLocked records a failed dial of key and schedules the next
// automatic one: ReconnectCooldown after the first failure, doubling up to
// ReconnectBackoffMax, each ±backoffJitter. A refused login and a pin
// mismatch go to the maximum at once — the same password or certificate
// will be refused again, and every attempt is a line in the device's log.
func (p *Pool) noteFailureLocked(key string, err error) {
	b := p.backoff[key]
	if b == nil {
		b = &backoffState{}
		p.backoff[key] = b
	}
	b.failures++
	b.lastErr = err
	delay := backoffDelay(b.failures, ReconnectCooldown, ReconnectBackoffMax)
	if IsLoginRefused(err) || errors.Is(err, ErrPinMismatch) {
		delay = ReconnectBackoffMax
	}
	delay += time.Duration(float64(delay) * backoffJitter * p.jitter())
	b.next = p.now().Add(delay)
}

// backoffDelay is the wait after the n-th consecutive failure, before
// jitter: base, doubling, capped at limit.
func backoffDelay(n int, base, limit time.Duration) time.Duration {
	d := base
	for i := 1; i < n && d < limit; i++ {
		d *= 2
	}
	return min(d, limit)
}

// BackoffError is EnsureConnection's answer while the pool waits out earlier
// failed dials of the device: no dial was made. It unwraps to the error of
// the last one.
type BackoffError struct {
	Until time.Time
	Last  error
}

func (e *BackoffError) Error() string {
	if e.Last == nil {
		return fmt.Sprintf("routeros: not redialled before %s (reconnect cooldown)", e.Until.Format(time.TimeOnly))
	}
	return fmt.Sprintf("routeros: not redialled before %s after: %v", e.Until.Format(time.TimeOnly), e.Last)
}

func (e *BackoffError) Unwrap() error { return e.Last }

// heldLocked reports whether an automatic dial of key has to wait, with the
// state that says until when.
func (p *Pool) heldLocked(key string) (*backoffState, bool) {
	b := p.backoff[key]
	return b, b != nil && p.now().Before(b.next)
}

// ResetBackoff lets the next GetLive or EnsureConnection dial key at once,
// whatever failed before. The liveness loop calls it when a device's API
// port answers again after it was down: the pollers reconnect at their next
// tick instead of waiting out a backoff that grew while the device was away.
func (p *Pool) ResetBackoff(key string) {
	p.mu.Lock()
	delete(p.backoff, key)
	p.mu.Unlock()
}

// Get returns an existing connection or nil.
//
// Prefer GetLive: Get never redials, so after a device reboots (which evicts the
// dead client) it keeps returning nil until something dials again.
func (p *Pool) Get(key string) *ros.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.clients[key]
}

// GetLive returns a usable connection for a device, redialling if the pooled
// one was evicted because its session ended.
//
// This is what the pollers want. A device that reboots mid-session leaves a dead
// socket behind; RunCommand evicts it, and GetLive then re-establishes the
// session on the next cycle instead of leaving that device dark until the next
// (hourly) info refresh happens to call EnsureConnection.
//
// Redials are single-attempt and spaced per device: ReconnectCooldown apart
// at least, backing off up to ReconnectBackoffMax while they keep failing
// (ResetBackoff ends the wait). A caller that arrives while a dial of the
// device is running waits for that dial instead of starting another. Returns
// nil when no connection is available, which every caller already treats as
// "skip this device".
func (p *Pool) GetLive(key string) *ros.Client {
	p.mu.Lock()
	if c, ok := p.clients[key]; ok {
		p.mu.Unlock()
		return c
	}
	spec, hasSpec := p.specs[key]
	_, held := p.heldLocked(key)
	running := p.flights[key] != nil
	p.mu.Unlock()

	if !hasSpec || (held && !running) {
		return nil
	}
	client, err := p.connect(key, spec)
	if err != nil {
		if !running {
			log.Printf("routeros: reconnect to %s failed: %v", spec.target.Address, err)
		}
		return nil
	}
	if !running {
		log.Printf("routeros: reconnected to %s", spec.target.Address)
	}
	return client
}

// dropClient takes client out of the pool if it is still what key holds. It
// is called when the client's connection closes (sessionConn), whoever
// closed it; the identity check keeps a late close of an old client from
// removing the session that replaced it.
func (p *Pool) dropClient(key string, client *ros.Client) {
	p.mu.Lock()
	if p.clients[key] == client {
		delete(p.clients, key)
	}
	p.mu.Unlock()
}

// take removes and returns key's client; the caller closes it outside the
// lock.
func (p *Pool) take(key string) *ros.Client {
	p.mu.Lock()
	c := p.clients[key]
	delete(p.clients, key)
	p.mu.Unlock()
	return c
}

// Close closes and removes a connection. The dial parameters stay, so
// GetLive reconnects a device that merely rebooted.
func (p *Pool) Close(key string) {
	if c := p.take(key); c != nil {
		_ = c.Close()
	}
}

// Forget closes a connection and discards everything remembered about the
// key: the dial parameters (GetLive will not redial it), the backoff, and a
// dial still in flight, whose session is closed when it arrives. Use it when
// the device was edited or deleted, and for transient pool keys (the
// address-move verification dial) — Close deliberately keeps the dial
// parameters so a rebooted device can be reconnected.
func (p *Pool) Forget(key string) {
	p.mu.Lock()
	c := p.clients[key]
	delete(p.clients, key)
	delete(p.specs, key)
	delete(p.backoff, key)
	if f := p.flights[key]; f != nil {
		f.superseded = true
	}
	p.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// CloseAll closes all connections.
func (p *Pool) CloseAll() {
	p.mu.Lock()
	all := make([]*ros.Client, 0, len(p.clients))
	for key, c := range p.clients {
		all = append(all, c)
		delete(p.clients, key)
	}
	p.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}

// EnsureConnection returns the device's pooled session, opening it when
// there is none, when the one there no longer answers, or when it was opened
// with other parameters than the ones given (the address, port, TLS flag,
// pin or credentials changed since).
//
// It makes at most one dial and honours the backoff: while earlier dials of
// the device are being waited out it returns a *BackoffError without
// dialling. A refusal (IsDialRefusal) is returned as it is and costs nothing.
func (p *Pool) EnsureConnection(key string, t Target, username, password string) (*ros.Client, error) {
	spec := dialSpec{target: t, username: username, password: password}

	p.mu.Lock()
	c := p.clients[key]
	known, hasSpec := p.specs[key]
	p.mu.Unlock()
	switch {
	case hasSpec && known != spec:
		// A session — or a backoff — that belongs to other parameters.
		if c != nil {
			log.Printf("routeros: connection parameters of %s changed, reconnecting", t.Address)
		}
		p.Forget(key)
	case c != nil:
		// Test if the connection is alive. A session that is merely busy
		// with another command is.
		err := KeepAlive(c)
		if err == nil || IsSessionBusy(err) {
			return c, nil
		}
		log.Printf("routeros: stale connection to %s, reconnecting", t.Address)
		p.Close(key)
	}

	if err := checkDial(t, password); err != nil {
		return nil, err
	}
	p.mu.Lock()
	b, held := p.heldLocked(key)
	running := p.flights[key] != nil
	p.mu.Unlock()
	if held && !running {
		return nil, &BackoffError{Until: b.next, Last: b.lastErr}
	}
	return p.connect(key, spec)
}
