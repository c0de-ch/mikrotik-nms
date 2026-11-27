// Package routerostest provides a stand-in RouterOS API endpoint for tests.
//
// A Device accepts the login a real device would and then records every
// command it receives, answering each with an empty !done so the caller does
// not hang (or with the rows a test scripted for it with Reply). Dialled through a real routeros.Pool it is a tripwire: a test
// hands the pool to the code under test and asserts the device saw nothing,
// which proves a path costs the devices no I/O whatever helper it might call.
//
// NewTLSDevice is the same endpoint behind TLS with a self-signed
// certificate, as a RouterOS api-ssl service presents one; Fingerprint is
// what the device would print as that certificate's fingerprint.
package routerostest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-routeros/routeros/v3/proto"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Device is a fake RouterOS API endpoint on the loopback interface.
type Device struct {
	ln   net.Listener
	Host string
	Port int
	// UseTLS is true for a NewTLSDevice.
	UseTLS      bool
	fingerprint string

	mu           sync.Mutex
	conns        int
	logins       int
	commands     []string
	args         [][]string
	replies      map[string][]map[string]string
	counts       map[string]int
	traps        map[string]string
	fatals       map[string]string
	raws         map[string][]byte
	hooks        map[string]func()
	refuseLogins bool
}

// Reply scripts the rows (one !re sentence each, attribute → value) that
// answer every later command with this word, e.g. "/system/resource/print".
func (d *Device) Reply(command string, rows ...map[string]string) {
	d.SetReply(command, rows...)
}

// NewDevice starts a fake device; it stops when the test ends.
func NewDevice(t testing.TB) *Device {
	t.Helper()
	return start(t, nil)
}

// NewTLSDevice starts a fake device that speaks the API over TLS with a
// freshly generated self-signed certificate (see Fingerprint).
func NewTLSDevice(t testing.TB) *Device {
	t.Helper()
	cert, fingerprint := selfSigned(t)
	d := start(t, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	d.UseTLS, d.fingerprint = true, fingerprint
	return d
}

func start(t testing.TB, tlsCfg *tls.Config) *Device {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("routerostest: listen: %v", err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	d := &Device{ln: ln, Host: host}
	d.Port, _ = strconv.Atoi(port)
	t.Cleanup(func() { _ = ln.Close() })
	go d.accept(tlsCfg)
	return d
}

// selfSigned makes the certificate of a TLS device and its fingerprint: the
// lower-case hex SHA-256 of the certificate's DER encoding.
func selfSigned(t testing.TB) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("routerostest: generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "routerostest"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("routerostest: create certificate: %v", err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, hex.EncodeToString(sum[:])
}

// Fingerprint is the SHA-256 of the device's certificate (lower-case hex),
// "" for a device without TLS.
func (d *Device) Fingerprint() string { return d.fingerprint }

// Target is how to dial the device: its address and port, and for a TLS
// device the TLS flag with the right pin. deviceID ties the dial to a
// stored device ("" leaves a pooled session counted under its pool key).
func (d *Device) Target(deviceID string) routeros.Target {
	return routeros.Target{DeviceID: deviceID, Address: d.Host, Port: d.Port, UseTLS: d.UseTLS, Pin: d.fingerprint}
}

// Dial logs the pool in to the device under deviceID, as the info loop does
// for a real one, so GetLive hands out a live client for it.
func (d *Device) Dial(t testing.TB, pool *routeros.Pool, deviceID string) {
	t.Helper()
	if _, err := pool.Dial(deviceID, d.Target(""), "nms", "secret"); err != nil {
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

// Logins is how many /login sentences reached the device: each one carried
// a user name and a password, whether or not it was accepted.
func (d *Device) Logins() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.logins
}

func (d *Device) accept(tlsCfg *tls.Config) {
	for {
		c, err := d.ln.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.conns++
		d.mu.Unlock()
		if tlsCfg != nil {
			c = tls.Server(c, tlsCfg)
		}
		go d.serve(c)
	}
}

func (d *Device) serve(c net.Conn) {
	defer c.Close()
	r, w := proto.NewReader(c), proto.NewWriter(c)
	for {
		r.BeginReply()
		sen, err := r.ReadSentence()
		if err != nil {
			var tlsErr tls.RecordHeaderError
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.As(err, &tlsErr) && !isHandshakeAbort(err) {
				// Bytes that do not even parse are traffic too.
				d.record("<unparsed: "+err.Error()+">", nil)
			}
			return
		}
		d.mu.Lock()
		if sen.Word == "/login" {
			d.logins++
		}
		hook := d.hooks[sen.Word]
		d.mu.Unlock()
		if hook != nil {
			hook()
		}
		d.mu.Lock()
		refuse := d.refuseLogins
		rows := d.replies[sen.Word]
		count, counted := d.counts[sen.Word]
		trap, trapped := d.traps[sen.Word]
		fatal, isFatal := d.fatals[sen.Word]
		raw, isRaw := d.raws[sen.Word]
		d.mu.Unlock()
		if sen.Word == "/login" {
			isFatal, isRaw = false, false
			if refuse {
				trap, trapped = "invalid user name or password (6)", true
			} else {
				trapped, rows = false, nil
			}
			counted = false
		} else {
			d.record(sen.Word, sen.List)
			counted = counted && hasWord(sen.List, "count-only")
		}
		switch {
		case isRaw:
			// Bytes straight onto the wire: whatever a broken or hostile
			// device might send instead of a reply.
			if _, err := c.Write(raw); err != nil {
				return
			}
			continue
		case isFatal:
			// A real device says !fatal and hangs up.
			w.BeginSentence()
			w.WriteWord("!fatal")
			w.WriteWord("=message=" + fatal)
			_ = w.EndSentence()
			return
		case trapped:
			w.BeginSentence()
			w.WriteWord("!trap")
			w.WriteWord("=message=" + trap)
			if err := w.EndSentence(); err != nil {
				return
			}
		case counted:
			// =count-only=: no rows, the count in the !done sentence.
		default:
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
		if counted && !trapped {
			w.WriteWord("=ret=" + strconv.Itoa(count))
		}
		if err := w.EndSentence(); err != nil {
			return
		}
	}
}

// isHandshakeAbort reports a TLS handshake the client gave up on (a pin
// that did not match): no API traffic was involved.
func isHandshakeAbort(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "remote error"
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

// SetCount makes the device answer command, when it is sent with
// =count-only=, with that row count in the !done sentence and no rows — as
// RouterOS does. Sent without =count-only=, the command is answered as
// before (SetReply's rows).
func (d *Device) SetCount(command string, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.counts == nil {
		d.counts = make(map[string]int)
	}
	d.counts[command] = n
}

// hasWord reports an attribute word (=key=…) among a sentence's pairs.
func hasWord(pairs []proto.Pair, key string) bool {
	for _, p := range pairs {
		if p.Key == key {
			return true
		}
	}
	return false
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

// SetFatal makes the device answer command with a !fatal carrying message
// and close the connection, as RouterOS does when it ends a session.
func (d *Device) SetFatal(command, message string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fatals == nil {
		d.fatals = make(map[string]string)
	}
	d.fatals[command] = message
}

// SetRaw makes the device answer command with exactly these bytes and
// nothing else — no !done unless they contain one. For replies no well-behaved
// device sends: see OversizedWord and Sentences.
func (d *Device) SetRaw(command string, wire []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.raws == nil {
		d.raws = make(map[string][]byte)
	}
	d.raws[command] = wire
}

// OversizedWord is the start of a reply whose first word declares size
// bytes: only the length prefix, as the body is never sent.
func OversizedWord(size uint32) []byte {
	return []byte{0xF0, byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size)}
}

// Sentences encodes sentences (each a list of words) for SetRaw.
func Sentences(sentences ...[]string) []byte {
	var out []byte
	for _, sen := range sentences {
		for _, word := range sen {
			out = append(out, encodeLength(len(word))...)
			out = append(out, word...)
		}
		out = append(out, 0)
	}
	return out
}

// Rows encodes n generated !re rows followed by !done, for SetRaw.
func Rows(n int) []byte {
	out := make([]byte, 0, n*16+8)
	for i := range n {
		out = append(out, Sentences([]string{"!re", "=n=" + strconv.Itoa(i)})...)
	}
	return append(out, Sentences([]string{"!done"})...)
}

func encodeLength(l int) []byte {
	switch {
	case l < 0x80:
		return []byte{byte(l)}
	case l < 0x4000:
		return []byte{byte(l>>8) | 0x80, byte(l)}
	case l < 0x200000:
		return []byte{byte(l>>16) | 0xC0, byte(l >> 8), byte(l)}
	case l < 0x10000000:
		return []byte{byte(l>>24) | 0xE0, byte(l >> 16), byte(l >> 8), byte(l)}
	default:
		return []byte{0xF0, byte(l >> 24), byte(l >> 16), byte(l >> 8), byte(l)}
	}
}

// OnCommand runs fn each time the device receives command, before it answers
// — for a device whose state changes with what it is told, e.g. one that
// reports a new version once it was told to install it. fn may call SetReply
// and SetTrap. A fn that blocks makes the device hang on that command
// ("/login" included).
func (d *Device) OnCommand(command string, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.hooks == nil {
		d.hooks = make(map[string]func())
	}
	d.hooks[command] = fn
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
