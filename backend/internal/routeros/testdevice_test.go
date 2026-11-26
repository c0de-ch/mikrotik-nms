package routeros

import (
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-routeros/routeros/v3/proto"
)

// testDevice is the smallest RouterOS API endpoint the tests inside this
// package need (routerostest, the full fake, imports this package and cannot
// be used from here): it accepts a login unless told to refuse it and
// answers every command with !done.
type testDevice struct {
	host   string
	port   int
	refuse atomic.Bool
	// hang makes the device accept the login and then answer nothing.
	hang   atomic.Bool
	conns  atomic.Int32
	closed chan struct{} // one value per connection the peer closed
}

// newHangingDevice is a device that logs a session in and never answers a
// command.
func newHangingDevice(t *testing.T) *testDevice {
	t.Helper()
	d := newTestDevice(t)
	d.hang.Store(true)
	return d
}

func newTestDevice(t *testing.T) *testDevice {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	d := &testDevice{host: host, closed: make(chan struct{}, 64)}
	d.port, _ = strconv.Atoi(port)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.conns.Add(1)
			go d.serve(c)
		}
	}()
	return d
}

func (d *testDevice) target() Target { return Target{Address: d.host, Port: d.port} }

func (d *testDevice) serve(c net.Conn) {
	defer func() {
		_ = c.Close()
		d.closed <- struct{}{}
	}()
	r, w := proto.NewReader(c), proto.NewWriter(c)
	for {
		r.BeginReply()
		sen, err := r.ReadSentence()
		if err != nil {
			return
		}
		if d.hang.Load() && sen.Word != "/login" {
			continue // read on, say nothing
		}
		w.BeginSentence()
		if sen.Word == "/login" && d.refuse.Load() {
			w.WriteWord("!trap")
			w.WriteWord("=message=invalid user name or password (6)")
			if w.EndSentence() != nil {
				return
			}
			w.BeginSentence()
		}
		w.WriteWord("!done")
		if w.EndSentence() != nil {
			return
		}
	}
}

// waitClosed waits until the device has seen n of its connections closed by
// the peer.
func (d *testDevice) waitClosed(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-d.closed:
		case <-time.After(10 * time.Second):
			t.Fatal("the device still holds a connection the pool should have closed")
		}
	}
}
