package poller

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// deviceAt is a device row that points at fake (TLS when fake is), with pin
// as its pinned fingerprint.
func deviceAt(id string, fake *routerostest.Device, pin string) queries.Device {
	return queries.Device{ID: id, Identity: id, Address: fake.Host, APIPort: fake.Port, Username: "nms",
		PasswordEnc: "secret", UseTLS: fake.UseTLS, PinCertSHA256: pin, Status: "online", Tags: "[]"}
}

// DialTarget carries everything a dial of a stored device needs — the pin
// and the device it is for included.
func TestDialTarget(t *testing.T) {
	dev := queries.Device{ID: "d1", Address: "192.0.2.10", APIPort: 8729, UseTLS: true, PinCertSHA256: strings.Repeat("0a", 32)}
	want := routeros.Target{DeviceID: "d1", Address: "192.0.2.10", Port: 8729, UseTLS: true, Pin: strings.Repeat("0a", 32)}
	if got := DialTarget(dev); got != want {
		t.Fatalf("DialTarget = %+v, want %+v", got, want)
	}
}

// A TLS session refused over its pin becomes a critical finding of the
// device, with the fingerprint that was presented; a repeat bumps the row.
// The login is never sent.
func TestPinMismatchBecomesADeviceFinding(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := pollerTestDB(t)
	t.Cleanup(InstallRouterOSHooks(db))

	fake := routerostest.NewTLSDevice(t)
	dev := deviceAt("d1", fake, strings.Repeat("ab", 32)) // pinned to another certificate
	mustCreateDevice(t, db, &dev)

	pool := routeros.NewPool()
	for range 2 {
		_, err := pool.Dial("d1", DialTarget(dev), dev.Username, dev.PasswordEnc)
		if !errors.Is(err, routeros.ErrPinMismatch) {
			t.Fatalf("dial = %v, want a pin mismatch", err)
		}
	}
	if fake.Logins() != 0 {
		t.Fatalf("logins = %d, want none", fake.Logins())
	}
	findings, err := queries.ListDeviceFindings(db, "d1", 10)
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings = %+v, %v; want one", findings, err)
	}
	f := findings[0]
	if f.Kind != queries.FindingTLSPinMismatch || f.Severity != "critical" || f.Detail != fake.Fingerprint() || f.Count != 2 {
		t.Fatalf("finding = %+v, want tls_pin_mismatch / critical / the presented fingerprint / count 2", f)
	}

	// The one-shot paths report it as well: a speed test of the device.
	sample := RunSpeedTest(db, queries.SpeedTest{ID: "st", DeviceID: "d1", URL: "http://192.0.2.1/x"})
	if !strings.Contains(sample.Error, "pin mismatch") {
		t.Fatalf("speed test error = %q, want the pin mismatch", sample.Error)
	}
	if findings, _ := queries.ListDeviceFindings(db, "d1", 10); len(findings) != 1 || findings[0].Count != 3 {
		t.Fatalf("after the speed test: %+v, want the same finding with count 3", findings)
	}
	if fake.Logins() != 0 {
		t.Fatalf("logins = %d, want none", fake.Logins())
	}
}

// A mismatch on a dial that names no stored device (the connection test of
// a device being added) is not a finding: there is no device to hang it on.
func TestPinMismatchWithoutADeviceIsNotAFinding(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := pollerTestDB(t)
	t.Cleanup(InstallRouterOSHooks(db))
	fake := routerostest.NewTLSDevice(t)
	target := fake.Target("")
	target.Pin = strings.Repeat("ab", 32)
	if _, err := routeros.DialOnce(target, "nms", "secret"); !errors.Is(err, routeros.ErrPinMismatch) {
		t.Fatalf("dial = %v, want a pin mismatch", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_findings`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("findings = %d, %v; want none", n, err)
	}
}

// A TLS device without a pin is never dialled by the info pass — the loop
// that opens every pooled session — and nothing is stored about it.
func TestInfoPassDoesNotDialATLSDeviceWithoutPin(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := pollerTestDB(t)
	t.Cleanup(InstallRouterOSHooks(db))
	fake := routerostest.NewTLSDevice(t)
	dev := deviceAt("d1", fake, "")
	mustCreateDevice(t, db, &dev)

	pool := routeros.NewPool()
	m := &Manager{db: db, pool: pool, hub: ws.NewHub(), cadence: NewCadence(db, nil),
		inventory: newTierClock(NewScheduleRegistry()), classes: newClassGate()}
	m.refreshDeviceInfo(dev)
	if fake.Conns() != 0 || pool.Get("d1") != nil || pool.GetLive("d1") != nil {
		t.Fatalf("a TLS device without a pin was contacted (%d connection(s))", fake.Conns())
	}

	// With its pin the same pass logs in and reads the device.
	fake.SetReply("/system/resource/print", map[string]string{"board-name": "CRS326", "version": "7.20", "cpu-count": "1"})
	dev.PinCertSHA256 = fake.Fingerprint()
	if err := queries.UpdateDevice(db, &dev); err != nil {
		t.Fatal(err)
	}
	m.refreshDeviceInfo(dev)
	if pool.Get("d1") == nil || fake.Logins() != 1 {
		t.Fatalf("with the pin: pooled=%v logins=%d, want a session", pool.Get("d1") != nil, fake.Logins())
	}
	t.Cleanup(pool.CloseAll)
}

// The class hook: C1 devices — probed, unprobed, overridden, or on the
// light profile — are weak; the answer is cached and follows a change.
func TestWeakDevices(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"c1": devclass.C1, "c2": devclass.C2, "c3": devclass.C3, "new": "", "light": devclass.C3})
	if err := queries.SetDevicePollProfile(db, "light", devclass.ProfileLight); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: at(12, 0, 0)}
	w := &weakDevices{db: db, ttl: weakDevicesTTL, now: clock.now}

	for id, want := range map[string]bool{"c1": true, "new": true, "light": true, "c2": false, "c3": false, "no-such-device": false, "": false} {
		if got := w.is(id); got != want {
			t.Errorf("weak(%q) = %v, want %v", id, got, want)
		}
	}

	// An override to C1 applies once the cache has aged, not before.
	if err := queries.SetDeviceClassOverride(db, "c3", devclass.C1); err != nil {
		t.Fatal(err)
	}
	clock.set(at(12, 0, 0).Add(weakDevicesTTL - time.Second))
	if w.is("c3") {
		t.Error("the cached answer changed before its TTL")
	}
	clock.set(at(12, 0, 0).Add(weakDevicesTTL))
	if !w.is("c3") {
		t.Error("a device overridden to C1 is not weak after the cache aged")
	}
}

// InstallRouterOSHooks gives a C1 device the longer command deadline, and
// its remove function takes it away again.
func TestInstalledHooksSetTheDeadlineOfWeakDevices(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"c1": devclass.C1, "c3": devclass.C3})
	oldC, oldW := routeros.CommandTimeout, routeros.WeakCommandTimeout
	routeros.CommandTimeout, routeros.WeakCommandTimeout = 40*time.Millisecond, 90*time.Millisecond
	t.Cleanup(func() { routeros.CommandTimeout, routeros.WeakCommandTimeout = oldC, oldW })
	remove := InstallRouterOSHooks(db)
	t.Cleanup(remove)

	deadlineOf := func(id string) time.Duration {
		t.Helper()
		fake := routerostest.NewDevice(t)
		release := make(chan struct{})
		fake.OnCommand("/interface/print", func() { <-release })
		t.Cleanup(func() { close(release) })
		pool := routeros.NewPool()
		if _, err := pool.Dial(id, fake.Target(id), "nms", "secret"); err != nil {
			t.Fatal(err)
		}
		_, err := routeros.RunCommand(pool.Get(id), "/interface/print")
		var te *routeros.TimeoutError
		if !errors.As(err, &te) {
			t.Fatalf("%s: err = %v, want a timeout", id, err)
		}
		return te.After
	}
	if got := deadlineOf("c1"); got != 90*time.Millisecond {
		t.Errorf("C1 deadline = %s, want the weak one", got)
	}
	if got := deadlineOf("c3"); got != 40*time.Millisecond {
		t.Errorf("C3 deadline = %s, want the default", got)
	}
	remove()
	if got := deadlineOf("c1"); got != 40*time.Millisecond {
		t.Errorf("C1 deadline without the hook = %s, want the default", got)
	}
}

// When the liveness ping reaches a device again after it was down, the pool
// stops waiting out the backoff of the dials that failed meanwhile: the
// pollers reconnect at their next tick.
func TestLivenessRecoveryEndsTheReconnectBackoff(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := pollerTestDB(t)
	fake := routerostest.NewDevice(t)
	dev := deviceAt("d1", fake, "")
	dev.Board, dev.ROSVersion = "CRS326", "7.20" // known: the ping does not start an info refresh
	mustCreateDevice(t, db, &dev)

	pool := routeros.NewPool()
	m := &Manager{db: db, pool: pool, hub: ws.NewHub(), cadence: NewCadence(db, nil)}

	// The device is "down": its login is refused, the pool backs off.
	fake.RefuseLogins(true)
	if _, err := pool.EnsureConnection("d1", DialTarget(dev), dev.Username, dev.PasswordEnc); !routeros.IsLoginRefused(err) {
		t.Fatalf("dial = %v, want a refused login", err)
	}
	fake.RefuseLogins(false)
	var held *routeros.BackoffError
	if _, err := pool.EnsureConnection("d1", DialTarget(dev), dev.Username, dev.PasswordEnc); !errors.As(err, &held) {
		t.Fatalf("second dial = %v, want it held back by the backoff", err)
	}

	// A ping of a device that was online all along changes nothing.
	dev.Status = "online"
	m.pollDevice(dev)
	if pool.GetLive("d1") != nil {
		t.Fatal("the backoff ended although the device never went away")
	}

	// It was reported down, and answers again: the next ask reconnects.
	for _, was := range []string{"offline", "unknown"} {
		dev.Status = was
		m.pollDevice(dev)
		client := pool.GetLive("d1")
		if client == nil {
			t.Fatalf("after recovering from %q the pool still waits out the backoff", was)
		}
		// Back into a backoff for the next round.
		fake.RefuseLogins(true)
		pool.Close("d1")
		pool.ResetBackoff("d1")
		if pool.GetLive("d1") != nil {
			t.Fatal("login was not refused")
		}
		fake.RefuseLogins(false)
	}
	t.Cleanup(pool.CloseAll)
}
