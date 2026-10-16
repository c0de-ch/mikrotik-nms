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

	mu       sync.Mutex
	conns    int
	commands []string
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
				d.record("<unparsed: " + err.Error() + ">")
			}
			return
		}
		if sen.Word != "/login" {
			d.record(sen.Word)
		}
		w.BeginSentence()
		w.WriteWord("!done")
		if err := w.EndSentence(); err != nil {
			return
		}
	}
}

func (d *Device) record(cmd string) {
	d.mu.Lock()
	d.commands = append(d.commands, cmd)
	d.mu.Unlock()
}
