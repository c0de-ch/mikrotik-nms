package routeros_test

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

// otherFingerprint is a well-formed pin that is no device's.
var otherFingerprint = strings.Repeat("ab", 32)

// dialPaths are all the ways a session is opened; every one must go through
// the same pinned dial.
var dialPaths = map[string]func(pool *routeros.Pool, t routeros.Target) (*ros.Client, error){
	"Pool.Dial": func(pool *routeros.Pool, t routeros.Target) (*ros.Client, error) {
		return pool.Dial("d1", t, "nms", "secret")
	},
	"Pool.EnsureConnection": func(pool *routeros.Pool, t routeros.Target) (*ros.Client, error) {
		return pool.EnsureConnection("d1", t, "nms", "secret")
	},
	"Pool.Dial under a verify key": func(pool *routeros.Pool, t routeros.Target) (*ros.Client, error) {
		return pool.Dial("d1:verify", t, "nms", "secret")
	},
	"DialOnce": func(_ *routeros.Pool, t routeros.Target) (*ros.Client, error) {
		return routeros.DialOnce(t, "nms", "secret")
	},
	"DialOnceWith": func(_ *routeros.Pool, t routeros.Target) (*ros.Client, error) {
		return routeros.DialOnceWith(t, routeros.NewCredentials("nms", "secret"))
	},
}

// mismatches collects what OnPinMismatch reports during a test.
func mismatches(t *testing.T) func() []routeros.PinMismatch {
	t.Helper()
	var mu sync.Mutex
	var seen []routeros.PinMismatch
	routeros.OnPinMismatch(func(m routeros.PinMismatch) {
		mu.Lock()
		seen = append(seen, m)
		mu.Unlock()
	})
	t.Cleanup(func() { routeros.OnPinMismatch(nil) })
	return func() []routeros.PinMismatch {
		mu.Lock()
		defer mu.Unlock()
		return append([]routeros.PinMismatch(nil), seen...)
	}
}

// With the device's fingerprint pinned, a TLS session logs in and works.
func TestTLSPinMatch(t *testing.T) {
	for name, dial := range dialPaths {
		t.Run(name, func(t *testing.T) {
			reported := mismatches(t)
			fake := routerostest.NewTLSDevice(t)
			fake.SetReply("/system/identity/print", map[string]string{"name": "sw1"})
			pool := routeros.NewPool()
			t.Cleanup(pool.CloseAll)

			client, err := dial(pool, fake.Target("d1"))
			if err != nil {
				t.Fatalf("dial with the right pin: %v", err)
			}
			defer client.Close()
			reply, err := routeros.RunCommand(client, "/system/identity/print")
			if err != nil || len(reply.Re) != 1 || reply.Re[0].Map["name"] != "sw1" {
				t.Fatalf("command over the pinned session = %v, %v", reply, err)
			}
			if fake.Logins() != 1 {
				t.Errorf("logins = %d, want 1", fake.Logins())
			}
			if got := reported(); len(got) != 0 {
				t.Errorf("mismatches reported for a matching pin: %+v", got)
			}
		})
	}
}

// Another certificate than the pinned one: the handshake is aborted, the
// login never sent, and the mismatch reported with the fingerprint seen.
func TestTLSPinMismatchWithholdsTheLogin(t *testing.T) {
	for name, dial := range dialPaths {
		t.Run(name, func(t *testing.T) {
			reported := mismatches(t)
			fake := routerostest.NewTLSDevice(t)
			target := fake.Target("d1")
			target.Pin = otherFingerprint
			pool := routeros.NewPool()

			client, err := dial(pool, target)
			if client != nil {
				client.Close()
				t.Fatal("a session was opened to a device presenting another certificate")
			}
			var mismatch *routeros.PinMismatchError
			if !errors.As(err, &mismatch) || !errors.Is(err, routeros.ErrPinMismatch) {
				t.Fatalf("err = %v, want a PinMismatchError", err)
			}
			if mismatch.Seen != fake.Fingerprint() || mismatch.Pinned != otherFingerprint {
				t.Errorf("mismatch = %+v, want seen %s pinned %s", mismatch, fake.Fingerprint(), otherFingerprint)
			}
			if !routeros.IsDialRefusal(err) || routeros.IsLoginRefused(err) {
				t.Errorf("a pin mismatch is a dial refusal, not a refused login: %v", err)
			}
			if fake.Logins() != 0 || len(fake.Commands()) != 0 {
				t.Fatalf("the device received %d login(s) and %v: credentials were sent past a pin mismatch",
					fake.Logins(), fake.Commands())
			}
			got := reported()
			if len(got) != 1 || got[0].DeviceID != "d1" || got[0].Seen != fake.Fingerprint() ||
				got[0].Pinned != otherFingerprint || got[0].Address != fake.Host {
				t.Errorf("reported = %+v, want one mismatch for d1 with the fingerprint seen", got)
			}
			if pool.Get("d1") != nil || pool.Get("d1:verify") != nil {
				t.Error("a refused dial left a pooled session")
			}
		})
	}
}

// A TLS device without a pin is not contacted at all: no connection, no
// handshake, no login — whatever the path.
func TestTLSWithoutPinIsNotDialled(t *testing.T) {
	for name, dial := range dialPaths {
		t.Run(name, func(t *testing.T) {
			reported := mismatches(t)
			fake := routerostest.NewTLSDevice(t)
			target := fake.Target("d1")
			target.Pin = ""
			pool := routeros.NewPool()

			client, err := dial(pool, target)
			if client != nil {
				client.Close()
			}
			if !errors.Is(err, routeros.ErrPinMissing) || !routeros.IsDialRefusal(err) {
				t.Fatalf("err = %v, want ErrPinMissing", err)
			}
			if fake.Conns() != 0 || fake.Logins() != 0 {
				t.Fatalf("the device saw %d connection(s) and %d login(s), want none", fake.Conns(), fake.Logins())
			}
			if len(reported()) != 0 {
				t.Error("a missing pin was reported as a mismatch")
			}
			// Nothing was remembered: the pollers' GetLive does not try either.
			if pool.GetLive("d1") != nil || fake.Conns() != 0 {
				t.Error("GetLive dialled a TLS device without a pin")
			}
		})
	}
}

// A stored pin that is not a fingerprint is treated as no pin.
func TestMalformedStoredPinIsNotDialled(t *testing.T) {
	fake := routerostest.NewTLSDevice(t)
	for _, pin := range []string{"not-a-fingerprint", strings.ToUpper(fake.Fingerprint()), fake.Fingerprint()[:62]} {
		target := fake.Target("d1")
		target.Pin = pin
		if _, err := routeros.DialOnce(target, "nms", "secret"); !errors.Is(err, routeros.ErrPinMissing) {
			t.Errorf("pin %q: err = %v, want ErrPinMissing", pin, err)
		}
	}
	if fake.Conns() != 0 {
		t.Errorf("connections = %d, want 0", fake.Conns())
	}
}

// A plain-API device behaves as before: no pin is needed, and one that
// happens to be stored is ignored.
func TestPlainAPIDevicesNeedNoPin(t *testing.T) {
	reported := mismatches(t)
	fake := routerostest.NewDevice(t)
	for _, pin := range []string{"", otherFingerprint} {
		target := fake.Target("d1")
		target.Pin = pin
		client, err := routeros.DialOnce(target, "nms", "secret")
		if err != nil {
			t.Fatalf("plain dial with pin %q: %v", pin, err)
		}
		if _, err := routeros.RunCommand(client, "/system/identity/print"); err != nil {
			t.Fatalf("plain command with pin %q: %v", pin, err)
		}
		client.Close()
	}
	if len(reported()) != 0 {
		t.Error("a plain-API dial reported a pin mismatch")
	}
}

// TLS asked of an endpoint that does not speak it: the handshake fails and
// the password stays here.
func TestTLSToAPlainEndpointSendsNoLogin(t *testing.T) {
	fake := routerostest.NewDevice(t) // plain API
	target := fake.Target("d1")
	target.UseTLS, target.Pin = true, otherFingerprint

	old := routeros.HandshakeTimeout
	routeros.HandshakeTimeout = 500 * time.Millisecond
	t.Cleanup(func() { routeros.HandshakeTimeout = old })

	client, err := routeros.DialOnce(target, "nms", "secret")
	if err == nil {
		client.Close()
		t.Fatal("a TLS dial to a plain endpoint succeeded")
	}
	if fake.Logins() != 0 {
		t.Fatalf("logins = %d: the password was sent in the clear", fake.Logins())
	}
	if routeros.IsLoginRefused(err) {
		t.Errorf("err = %v, must not read as a refused login", err)
	}
}

// silentListener accepts connections and never says a word.
func silentListener(t *testing.T) routeros.Target {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
	})
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return routeros.Target{Address: host, Port: p}
}

// A peer that accepts the connection and then stays silent does not hold a
// dial for ever: the TLS handshake and the login each have their own bound.
func TestDialPhasesAreBounded(t *testing.T) {
	oldH, oldL := routeros.HandshakeTimeout, routeros.LoginTimeout
	routeros.HandshakeTimeout, routeros.LoginTimeout = 60*time.Millisecond, 60*time.Millisecond
	t.Cleanup(func() { routeros.HandshakeTimeout, routeros.LoginTimeout = oldH, oldL })

	t.Run("TLS handshake", func(t *testing.T) {
		target := silentListener(t)
		target.UseTLS, target.Pin = true, otherFingerprint
		start := time.Now()
		_, err := routeros.DialOnce(target, "nms", "secret")
		if err == nil || !strings.Contains(err.Error(), "TLS handshake") {
			t.Fatalf("err = %v, want a handshake timeout", err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("the dial returned after %s, want it at the 60ms handshake bound", took)
		}
	})

	t.Run("login", func(t *testing.T) {
		start := time.Now()
		_, err := routeros.DialOnce(silentListener(t), "nms", "secret")
		if err == nil || routeros.IsLoginRefused(err) {
			t.Fatalf("err = %v, want a login timeout (not a refused login)", err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("the dial returned after %s, want it at the 60ms login bound", took)
		}
	})

	t.Run("login on a device that hangs", func(t *testing.T) {
		fake := routerostest.NewDevice(t)
		release := make(chan struct{})
		fake.OnCommand("/login", func() { <-release })
		t.Cleanup(func() { close(release) })
		pool := routeros.NewPool()
		if _, err := pool.Dial("d1", fake.Target(""), "nms", "secret"); err == nil || routeros.IsLoginRefused(err) {
			t.Fatalf("err = %v, want a login timeout", err)
		}
		if pool.Get("d1") != nil {
			t.Error("a session whose login timed out was pooled")
		}
	})
}

// A refused login is recognised as one, on a plain and on a TLS session.
func TestLoginRefused(t *testing.T) {
	for name, fake := range map[string]*routerostest.Device{
		"plain": routerostest.NewDevice(t), "tls": routerostest.NewTLSDevice(t),
	} {
		fake.RefuseLogins(true)
		_, err := routeros.DialOnce(fake.Target("d1"), "nms", "wrong")
		if !routeros.IsLoginRefused(err) || routeros.IsDialRefusal(err) {
			t.Errorf("%s: err = %v, want a refused login", name, err)
		}
	}
}

func TestNormalizePin(t *testing.T) {
	hex64 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	colons := "01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF"
	for in, want := range map[string]string{
		"":                                       "",
		"   ":                                    "",
		hex64:                                    hex64,
		strings.ToUpper(hex64):                   hex64,
		"  " + hex64 + "\n":                      hex64,
		colons:                                   hex64,
		strings.ToLower(colons):                  hex64,
		" " + colons + " ":                       hex64,
		strings.ToUpper(hex64[:32]) + hex64[32:]: hex64,
	} {
		got, err := routeros.NormalizePin(in)
		if err != nil || got != want {
			t.Errorf("NormalizePin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"abc", hex64[:63], hex64 + "0", strings.Replace(hex64, "0", "g", 1),
		"sha256:" + hex64, hex64[:40], strings.Repeat("zz", 32), hex64[:32] + " " + hex64[32:],
	} {
		if got, err := routeros.NormalizePin(bad); err == nil {
			t.Errorf("NormalizePin(%q) = %q, want an error", bad, got)
		}
	}
}

// CertFingerprint is what the fake prints: the pin format round-trips.
func TestCertFingerprintIsNormalised(t *testing.T) {
	fake := routerostest.NewTLSDevice(t)
	fp := fake.Fingerprint()
	if got, err := routeros.NormalizePin(fp); err != nil || got != fp || len(fp) != 64 {
		t.Fatalf("fingerprint %q is not in the stored form (%q, %v)", fp, got, err)
	}
	// Pasted the way WinBox shows it — upper case with colons — it is the
	// same pin.
	var pasted []string
	for i := 0; i < len(fp); i += 2 {
		pasted = append(pasted, strings.ToUpper(fp[i:i+2]))
	}
	pin, err := routeros.NormalizePin(strings.Join(pasted, ":"))
	if err != nil {
		t.Fatal(err)
	}
	target := fake.Target("d1")
	target.Pin = pin
	client, err := routeros.DialOnce(target, "nms", "secret")
	if err != nil {
		t.Fatalf("dial with the pasted fingerprint: %v", err)
	}
	client.Close()
}
