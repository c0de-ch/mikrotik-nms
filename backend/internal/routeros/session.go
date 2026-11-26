package routeros

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
)

// Command deadlines.
//
// Every command runs under a deadline that starts when the caller asks: the
// wait for the session (other commands queued ahead on it) counts against
// it. When the deadline passes while the command is on the wire the session
// is over — the reply stream is at an unknown position — and is closed and
// evicted; the next GetLive redials.
var (
	// CommandTimeout is the deadline of a command nothing says more about.
	CommandTimeout = 10 * time.Second
	// WeakCommandTimeout replaces it on C1 devices (SetWeakDeviceFunc).
	WeakCommandTimeout = 15 * time.Second
	// SlowCommandTimeout is the deadline of the reads in slowCommands.
	SlowCommandTimeout = 30 * time.Second
	// OnceCommandTimeout is the default of an OnceRunner: the commands of an
	// admin's firmware job on its own one-shot session. check-for-updates
	// and the prints after it wait on MikroTik's servers, and nothing else
	// queues behind them on that session.
	OnceCommandTimeout = 30 * time.Second
)

// slowCommands are the scheduled reads that can legitimately outlast
// CommandTimeout on a large device: tables whose size is set by the network
// around the device, not by the device, printed whole. Ending the session
// over one would only make every poller redial and the read fail again at
// the next cadence, so they keep the 30 s every command had before the
// per-command deadlines.
var slowCommands = map[string]bool{
	// The bridge FDB: one row per MAC address in the L2 domain. The first
	// print after boot takes seconds even on a fast CPU.
	"/interface/bridge/host/print": true,
	// The whole log buffer, formatted line by line: 1000 lines by default,
	// tens of thousands where memory-lines was raised.
	"/log/print": true,
	// ARP and DHCP leases: one row per host of every subnet the device
	// serves.
	"/ip/arp/print":                true,
	"/ip/dhcp-server/lease/print":  true,
	"/interface/bridge/vlan/print": true, // one row per VLAN, with every member port
	// Registration tables: one row per associated client of a controller,
	// each with a dozen live counters.
	"/interface/wifi/registration-table/print":     true,
	"/interface/wireless/registration-table/print": true,
	"/caps-man/registration-table/print":           true,
	// The default-route query is answered by walking the routing table:
	// seconds on a router that carries a full BGP table.
	"/ip/route/print": true,
	// Hardware counters are fetched from the switch chip port by port.
	"/interface/ethernet/print": true,
	// A burst is at most 10 probes 0.2 s apart plus the last reply wait,
	// but a host-name target is resolved by the device first, and its
	// resolver gives up after 10 s.
	"/ping": true,
}

// commandTimeout is the deadline RunCommand gives command on client.
func commandTimeout(client *ros.Client, command string) time.Duration {
	d := CommandTimeout
	if isWeak(sessionDevice(client)) {
		d = WeakCommandTimeout
	}
	if slowCommands[command] && SlowCommandTimeout > d {
		d = SlowCommandTimeout
	}
	return d
}

// TimeoutError is a command that did not finish within its deadline.
type TimeoutError struct {
	Command string
	After   time.Duration
	// Queued is true when the deadline passed while the command still
	// waited for the session: nothing was sent and the session lives on —
	// the command holding it answers to its own deadline. Otherwise the
	// command was on the wire and the session has been closed.
	Queued bool
}

func (e *TimeoutError) Error() string {
	if e.Queued {
		return fmt.Sprintf("routeros command %q timed out after %s waiting for the device's session (another command holds it)", e.Command, e.After)
	}
	return fmt.Sprintf("routeros command %q timed out after %s", e.Command, e.After)
}

// IsSessionBusy reports a command that gave up waiting for its session: the
// session is alive and busy, which says nothing about the device.
func IsSessionBusy(err error) bool {
	var te *TimeoutError
	return errors.As(err, &te) && te.Queued
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

// IsSessionFatal reports an error after which the session it happened on is
// gone: the library ended it (a !fatal, a reply over a cap, bytes that are
// not a sentence, a transport error mid-command — ros.SessionFatalError), the
// socket failed, or the command's deadline passed on the wire.
func IsSessionFatal(err error) bool {
	if err == nil {
		return false
	}
	var fatal *ros.SessionFatalError
	if errors.As(err, &fatal) {
		return true
	}
	var te *TimeoutError
	if errors.As(err, &te) {
		return !te.Queued
	}
	return errors.Is(err, ErrSessionClosed) || isConnError(err)
}

// ErrSessionClosed is the answer to a command on a session that is already
// closed: by its owner, by an earlier session-fatal error, or by the pool.
var ErrSessionClosed = errors.New("routeros: the session is closed")

// RunCommand executes a RouterOS command and returns the reply sentences,
// under the command's default deadline: CommandTimeout, WeakCommandTimeout
// on a C1 device, SlowCommandTimeout for the reads in slowCommands.
func RunCommand(client *ros.Client, command string, args ...string) (*ros.Reply, error) {
	return RunCommandWithTimeout(client, commandTimeout(client, command), command, args...)
}

// minRunBudget is the least time a command must have left when it gets the
// session: with less it is not started. Sending a command that is bound to
// hit its deadline would end a healthy session for nothing.
const minRunBudget = time.Second

// watchdogGrace is how long after the deadline a command is given to return
// before its connection is closed under it. The connection's own deadline
// ends every read and write; this is the guarantee that a command returns
// even if that should ever fail to.
const watchdogGrace = 2 * time.Second

// RunCommandWithTimeout is RunCommand with a caller-chosen deadline, for
// commands that legitimately run longer (speed-test downloads, traceroutes,
// firmware installs).
//
// Commands on one session run one at a time. The deadline starts now: a
// command that waits for the session longer than timeout fails with a
// TimeoutError{Queued: true} and leaves the session alone. One that passes
// its deadline on the wire, or fails with any other session-fatal error
// (IsSessionFatal), ends the session: it is closed, and a pooled one is
// evicted so the next GetLive redials.
func RunCommandWithTimeout(client *ros.Client, timeout time.Duration, command string, args ...string) (reply *ros.Reply, err error) {
	// Observed after the session lock is released (defers run last-in
	// first-out), with the wait for the session counted in.
	start, timedOut := time.Now(), false
	device := sessionDevice(client) // before a failure evicts the session
	defer func() { observeCommandFor(device, command, start, reply, err, timedOut) }()

	deadline := start.Add(timeout)
	s := sessionOf(client)
	if s == nil {
		// Its bookkeeping went with the connection (sessionConn): nothing
		// is sent on a client this package no longer — or never — tracked.
		return nil, ErrSessionClosed
	}

	wait := time.NewTimer(timeout)
	select {
	case s.lock <- struct{}{}:
		wait.Stop()
	case <-wait.C:
		timedOut = true
		return nil, &TimeoutError{Command: command, After: timeout, Queued: true}
	}
	defer func() { <-s.lock }()

	if left := time.Until(deadline); left < min(minRunBudget, timeout/2) {
		timedOut = true
		return nil, &TimeoutError{Command: command, After: timeout, Queued: true}
	}

	_ = s.conn.SetDeadline(deadline)
	watchdog := time.AfterFunc(time.Until(deadline)+watchdogGrace, func() { _ = client.Close() })
	reply, err = client.RunArgs(append([]string{command}, args...))
	late := !watchdog.Stop()
	if err == nil {
		_ = s.conn.SetDeadline(time.Time{})
		return reply, nil
	}
	if late || errors.Is(err, os.ErrDeadlineExceeded) {
		timedOut = true
		err = &TimeoutError{Command: command, After: timeout}
	}
	if IsSessionFatal(err) {
		_ = client.Close() // also evicts a pooled client (sessionConn)
		return nil, err
	}
	// The device declined the command (!trap): the session is fine.
	_ = s.conn.SetDeadline(time.Time{})
	return reply, err
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
