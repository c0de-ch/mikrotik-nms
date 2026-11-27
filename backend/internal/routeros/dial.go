package routeros

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	ros "github.com/go-routeros/routeros/v3"
)

// Every session — pooled or one-shot — is opened here, on a connection this
// package owns: the dial policy and the pin are checked first, each phase has
// its own bound, and the connection's deadline is what later ends a command
// that outlives its budget (see RunCommandWithTimeout).
var (
	// ConnectTimeout bounds the TCP connect.
	ConnectTimeout = 5 * time.Second
	// TCPKeepAlive is the idle time before, and the interval between, TCP
	// keep-alive probes on a session: a device that vanished without a FIN
	// (power loss, a cable pulled) is noticed between commands.
	TCPKeepAlive = 15 * time.Second
	// HandshakeTimeout bounds the TLS handshake; WeakHandshakeTimeout
	// applies to C1 devices, whose CPUs take seconds over one.
	HandshakeTimeout     = 10 * time.Second
	WeakHandshakeTimeout = 15 * time.Second
	// LoginTimeout bounds the login exchange.
	LoginTimeout = 10 * time.Second
)

// Target is where and how to reach a device's API.
type Target struct {
	// DeviceID ties the dial to a stored device: its commands and sessions
	// are observed against it (Observer), its class picks the deadlines
	// (SetWeakDeviceFunc) and a pin mismatch is reported for it
	// (OnPinMismatch). "" for a device that is not stored yet; a pooled
	// session then counts under its pool key.
	DeviceID string
	Address  string
	Port     int
	UseTLS   bool
	// Pin is the fingerprint the device's TLS certificate must have
	// (devices.pin_cert_sha256): required with UseTLS, ignored without.
	Pin string
}

var weakDevice atomic.Pointer[func(string) bool]

// SetWeakDeviceFunc installs the process-wide answer to "is this device a
// weak one (class C1)?" (nil removes it). Weak devices get the longer
// handshake and command deadlines. Without it no device is weak. fn is
// called on every dial and command and must be cheap.
func SetWeakDeviceFunc(fn func(deviceID string) bool) {
	if fn == nil {
		weakDevice.Store(nil)
		return
	}
	weakDevice.Store(&fn)
}

func isWeak(deviceID string) bool {
	if deviceID == "" {
		return false
	}
	if fn := weakDevice.Load(); fn != nil {
		return (*fn)(deviceID)
	}
	return false
}

// handshakeTimeout is the bound of a TLS handshake with deviceID.
func handshakeTimeout(deviceID string) time.Duration {
	if isWeak(deviceID) {
		return WeakHandshakeTimeout
	}
	return HandshakeTimeout
}

// session is what this package keeps about an open client: the connection
// whose deadline bounds its commands, and the lock that serialises them (the
// library's reader is not safe for concurrent use, and RouterOS answers one
// untagged command at a time).
type session struct {
	conn net.Conn
	// lock holds one token: taking it is the right to run a command. A
	// channel rather than a mutex because the wait must be able to give up
	// at the command's deadline.
	lock chan struct{}
}

// sessions maps every open client to its session; an entry goes when the
// connection closes (sessionConn).
var sessions sync.Map // map[*ros.Client]*session

func sessionOf(c *ros.Client) *session {
	if v, ok := sessions.Load(c); ok {
		return v.(*session)
	}
	return nil
}

// sessionConn is the connection handed to the library. Whoever closes it —
// the caller, the pool, the library itself after a session-fatal error, or a
// command deadline — the client's bookkeeping goes with it, and a pooled
// client leaves its pool, so nothing can hand a dead session out again.
type sessionConn struct {
	net.Conn
	once    sync.Once
	cleanup func()
}

func (c *sessionConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.cleanup != nil {
			c.cleanup()
		}
	})
	return err
}

// forgetClient drops everything kept about a closed client.
func forgetClient(c *ros.Client) {
	sessions.Delete(c)
	sessionTags.Delete(c)
	if v, ok := clientOwners.LoadAndDelete(c); ok {
		owner := v.(clientOwner)
		owner.pool.dropClient(owner.key, c)
	}
}

// checkDial is everything that can refuse a dial before a single packet is
// sent: the dial policy, a stored secret that could not be decrypted, and a
// TLS device without a pin.
func checkDial(t Target, password string) error {
	if err := CheckTarget(t.Address); err != nil {
		return err
	}
	if err := checkSecret(password); err != nil {
		return err
	}
	return checkPin(t)
}

// IsDialRefusal reports a dial this instance refused to make or to complete,
// which trying again cannot change: polling is off, the target is outside the
// allow-list, the stored credentials are unreadable, the TLS device has no
// pin, or its certificate is not the pinned one.
func IsDialRefusal(err error) bool {
	return errors.Is(err, ErrPollersOff) || errors.Is(err, ErrTargetNotAllowed) ||
		errors.Is(err, ErrCredentialsUnreadable) || errors.Is(err, ErrPinMissing) ||
		errors.Is(err, ErrPinMismatch)
}

// dialSession opens a session to t and logs in.
//
//	TCP connect  ConnectTimeout, keep-alive TCPKeepAlive
//	TLS          handshake within HandshakeTimeout (WeakHandshakeTimeout on
//	             C1), the leaf certificate compared with the pin
//	login        under a LoginTimeout deadline on the connection
//
// Nothing is sent before checkDial passes, and no credential before the
// pinned handshake does.
func dialSession(t Target, username, password string) (*ros.Client, error) {
	if err := checkDial(t, password); err != nil {
		return nil, err
	}
	addr := JoinHostPort(t.Address, t.Port)
	fail := func(err error) (*ros.Client, error) {
		return nil, fmt.Errorf("routeros dial %s: %w", addr, err)
	}

	dialer := net.Dialer{Timeout: ConnectTimeout, KeepAlive: TCPKeepAlive}
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return fail(err)
	}

	if t.UseTLS {
		tc := tls.Client(conn, pinnedTLSConfig(t))
		ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout(t.DeviceID))
		err := tc.HandshakeContext(ctx)
		cancel()
		if err != nil {
			_ = conn.Close()
			var mismatch *PinMismatchError
			if errors.As(err, &mismatch) {
				reportPinMismatch(PinMismatch{DeviceID: t.DeviceID, Address: t.Address, Seen: mismatch.Seen, Pinned: mismatch.Pinned})
				return fail(mismatch)
			}
			return fail(fmt.Errorf("TLS handshake: %w", err))
		}
		conn = tc
	}

	sc := &sessionConn{Conn: conn}
	client, err := ros.NewClient(sc)
	if err != nil {
		_ = conn.Close()
		return fail(err)
	}
	// The library logs every sentence it sends at Debug — the login with
	// its password included. Its logger never sees them (redact.go).
	client.SetLogHandler(clientLogHandler{})
	sc.cleanup = func() { forgetClient(client) }

	_ = conn.SetDeadline(time.Now().Add(LoginTimeout))
	if err := client.Login(username, password); err != nil {
		_ = client.Close()
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})

	sessions.Store(client, &session{conn: conn, lock: make(chan struct{}, 1)})
	if t.DeviceID != "" {
		TagSession(client, t.DeviceID)
	}
	return client, nil
}

// DialOnce opens a one-shot dedicated session to a device: the same dial as
// the pool's (policy, timeouts, pinned TLS), but not pooled. The caller owns
// the session and must Close() it. With t.DeviceID set its commands are
// counted against that device (TagSession) until it is closed.
//
// Intended for long-running commands (speed-test /tool/fetch downloads,
// /tool/traceroute, firmware jobs) that would otherwise hold the device's
// pooled session — and every poller waiting for it — for their whole run.
func DialOnce(t Target, username, password string) (*ros.Client, error) {
	return dialSession(t, username, password)
}
