package routeros

import (
	"sync/atomic"
	"time"

	ros "github.com/go-routeros/routeros/v3"
)

// CommandEvent describes one finished RouterOS API command: what the NMS
// asked a device and what it cost. It is the raw material of the NMS
// self-metrics ("is it us?"): commands, wall time, reply rows and timeouts
// per device and minute.
type CommandEvent struct {
	// DeviceID is the pool key of the session the command ran on. It is ""
	// for a one-shot session that was not tied to a device (TagSession).
	DeviceID string
	Command  string
	// Start is when the caller asked; Wall includes the wait for the
	// session (other commands queued ahead on it) and the execution.
	Start time.Time
	Wall  time.Duration
	// Rows is the number of !re sentences in the reply.
	Rows int
	Err  error
	// Timeout: the command hit its deadline, which also ends the session.
	Timeout bool
}

// SessionEvent describes one attempt to open a pooled session to a device.
// A successful attempt after the first is a reconnect.
type SessionEvent struct {
	DeviceID string
	At       time.Time
	Err      error
}

// Observer receives every command and every pooled dial. Its methods are
// called on the caller's goroutine, after the command or dial has finished
// and outside the session lock: they must be cheap and must not block.
type Observer interface {
	Command(CommandEvent)
	Session(SessionEvent)
}

type observerBox struct{ o Observer }

var observer atomic.Pointer[observerBox]

// SetObserver installs the process-wide observer (nil removes it). There is
// one: the poller manager's device-load recorder.
func SetObserver(o Observer) {
	if o == nil {
		observer.Store(nil)
		return
	}
	observer.Store(&observerBox{o: o})
}

func currentObserver() Observer {
	if b := observer.Load(); b != nil {
		return b.o
	}
	return nil
}

// TagSession ties a one-shot session (DialOnce, DialOnceWith) to a device so
// that its commands are counted against it. The caller untags it when it
// closes the session (UntagSession); pooled sessions are tagged by the pool.
func TagSession(c *ros.Client, deviceID string) {
	if c != nil && deviceID != "" {
		sessionTags.Store(c, deviceID)
	}
}

// UntagSession forgets a TagSession.
func UntagSession(c *ros.Client) {
	if c != nil {
		sessionTags.Delete(c)
	}
}

// sessionDevice is the device a session's commands are counted against.
func sessionDevice(c *ros.Client) string {
	if v, ok := clientOwners.Load(c); ok {
		return v.(clientOwner).deviceID
	}
	if v, ok := sessionTags.Load(c); ok {
		return v.(string)
	}
	return ""
}

func observeCommandFor(deviceID, command string, start time.Time, reply *ros.Reply, err error, timeout bool) {
	o := currentObserver()
	if o == nil {
		return
	}
	rows := 0
	if reply != nil {
		rows = len(reply.Re)
	}
	o.Command(CommandEvent{
		DeviceID: deviceID, Command: command, Start: start, Wall: time.Since(start),
		Rows: rows, Err: err, Timeout: timeout,
	})
}

func observeSession(deviceID string, err error) {
	if o := currentObserver(); o != nil {
		o.Session(SessionEvent{DeviceID: deviceID, At: time.Now(), Err: err})
	}
}
