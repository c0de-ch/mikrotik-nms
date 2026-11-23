// Package routerostest provides a stand-in RouterOS API endpoint for tests.
//
// A Device accepts the login a real device would and then records every
// command it receives, answering each with an empty !done so the caller does
// not hang. Dialled through a real routeros.Pool it is a tripwire: a test
// hands the pool to the code under test and asserts the device saw nothing,
// which proves a path costs the devices no I/O whatever helper it might call.
package routerostest

import (
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/go-routeros/routeros/v3/proto"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Device is a fake RouterOS API endpoint on the loopback interface.
type Device struct {
	ln   net.Listener
	Host string
	Port int

	mu           sync.Mutex
	conns        int
	commands     []string
	args         [][]string
	replies      map[string][]map[string]string
	traps        map[string]string
	refuseLogins bool
}

// NewDevice starts a fake device; it stops when the test ends.
func NewDevice(t testing.TB) *Device {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("routerostest: listen: %v", err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	d := &Device{ln: ln, Host: host}
	d.Port, _ = strconv.Atoi(port)
	t.Cleanup(func() { _ = ln.Close() })
	go d.accept()
	return d
}

// Dial logs the pool in to the device under deviceID, as the info loop does
// for a real one, so GetLive hands out a live client for it.
func (d *Device) Dial(t testing.TB, pool *routeros.Pool, deviceID string) {
	t.Helper()
	if _, err := pool.Dial(deviceID, d.Host, d.Port, "nms", "secret", false); err != nil {
		t.Fatalf("routerostest: dial %s: %v", deviceID, err)
	}
	t.Cleanup(func() { pool.Close(deviceID) })
}

// Commands returns every command received after login, in order.
func (d *Device) Commands() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.commands...)
}

// Conns is how many connections the device accepted.
func (d *Device) Conns() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conns
}

func (d *Device) accept() {
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conns++
		d.mu.Unlock()
		go d.serve(c)
	}
}

func (d *Device) serve(c net.Conn) {
	defer c.Close()
	r, w := proto.NewReader(c), proto.NewWriter(c)
	for {
		sen, err := r.ReadSentence()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				// Bytes that do not even parse are traffic too.
				d.record("<unparsed: "+err.Error()+">", nil)
			}
			return
		}
		d.mu.Lock()
		refuse := d.refuseLogins
		rows := d.replies[sen.Word]
		trap, trapped := d.traps[sen.Word]
		d.mu.Unlock()
		if sen.Word == "/login" {
			if refuse {
				trap, trapped = "invalid user name or password (6)", true
			} else {
				trapped, rows = false, nil
			}
		} else {
			d.record(sen.Word, sen.List)
		}
		if trapped {
			w.BeginSentence()
			w.WriteWord("!trap")
			w.WriteWord("=message=" + trap)
			if err := w.EndSentence(); err != nil {
				return
			}
		} else {
			for _, row := range rows {
				w.BeginSentence()
				w.WriteWord("!re")
				for k, v := range row {
					w.WriteWord("=" + k + "=" + v)
				}
				if err := w.EndSentence(); err != nil {
					return
				}
			}
		}
		w.BeginSentence()
		w.WriteWord("!done")
		if err := w.EndSentence(); err != nil {
			return
		}
	}
}

// SetReply makes the device answer command with these !re rows.
func (d *Device) SetReply(command string, rows ...map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.replies == nil {
		d.replies = make(map[string][]map[string]string)
	}
	d.replies[command] = rows
}

// SetTrap makes the device answer command with a !trap carrying message.
func (d *Device) SetTrap(command, message string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.traps == nil {
		d.traps = make(map[string]string)
	}
	d.traps[command] = message
}

// RefuseLogins makes every later login fail as a wrong password does.
func (d *Device) RefuseLogins(refuse bool) {
	d.mu.Lock()
	d.refuseLogins = refuse
	d.mu.Unlock()
}

// Args returns the argument words of every command received after login,
// in the order of Commands.
func (d *Device) Args() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][]string, len(d.args))
	for i, a := range d.args {
		out[i] = append([]string(nil), a...)
	}
	return out
}

func (d *Device) record(cmd string, pairs []proto.Pair) {
	words := make([]string, 0, len(pairs))
	for _, p := range pairs {
		words = append(words, "="+p.Key+"="+p.Value)
	}
	d.mu.Lock()
	d.commands = append(d.commands, cmd)
	d.args = append(d.args, words)
	d.mu.Unlock()
}
