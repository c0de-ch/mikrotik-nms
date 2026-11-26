package routeros

import (
	"errors"
	"slices"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
)

// weakIs makes exactly the named devices weak (C1) for the test.
func weakIs(t *testing.T, ids ...string) {
	t.Helper()
	SetWeakDeviceFunc(func(id string) bool { return slices.Contains(ids, id) })
	t.Cleanup(func() { SetWeakDeviceFunc(nil) })
}

func dialOnceAs(t *testing.T, dev *testDevice, deviceID string) *ros.Client {
	t.Helper()
	c, err := DialOnce(Target{DeviceID: deviceID, Address: dev.host, Port: dev.port}, "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The default deadline of a command: 10 s, 15 s on a C1 device, 30 s for the
// reads listed as slow — whoever the session belongs to.
func TestCommandTimeoutByClassAndCommand(t *testing.T) {
	dev := newTestDevice(t)
	weakIs(t, "weak-1")
	strong, weak, unknown := dialOnceAs(t, dev, "strong-1"), dialOnceAs(t, dev, "weak-1"), dialOnceAs(t, dev, "")

	for _, cmd := range []string{"/interface/print", "/system/resource/print", "/interface/monitor-traffic", "/ip/neighbor/print"} {
		if got := commandTimeout(strong, cmd); got != 10*time.Second {
			t.Errorf("%s on a strong device: %s, want 10s", cmd, got)
		}
		if got := commandTimeout(unknown, cmd); got != 10*time.Second {
			t.Errorf("%s on an unknown device: %s, want 10s", cmd, got)
		}
		if got := commandTimeout(weak, cmd); got != 15*time.Second {
			t.Errorf("%s on a C1 device: %s, want 15s", cmd, got)
		}
	}
	if len(slowCommands) == 0 {
		t.Fatal("no slow commands listed")
	}
	for cmd := range slowCommands {
		if a, b := commandTimeout(strong, cmd), commandTimeout(weak, cmd); a != 30*time.Second || b != 30*time.Second {
			t.Errorf("%s: %s on a strong device, %s on a C1 device; want 30s on both", cmd, a, b)
		}
	}

	// A pooled session takes its class from the device it was dialled for,
	// or from its pool key.
	pool := NewPool()
	t.Cleanup(pool.CloseAll)
	byKey, err := pool.Dial("weak-1", dev.target(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	byTarget, err := pool.Dial("weak-1:verify", Target{DeviceID: "weak-1", Address: dev.host, Port: dev.port}, "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	other, err := pool.Dial("strong-1", dev.target(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := commandTimeout(byKey, "/interface/print"), commandTimeout(byTarget, "/interface/print"), commandTimeout(other, "/interface/print")
	if a != 15*time.Second || b != 15*time.Second || c != 10*time.Second {
		t.Errorf("pooled deadlines = %s (by key), %s (by target), %s (strong); want 15s, 15s, 10s", a, b, c)
	}
}

func TestHandshakeTimeoutByClass(t *testing.T) {
	weakIs(t, "weak-1")
	if got := handshakeTimeout("weak-1"); got != 15*time.Second {
		t.Errorf("C1 handshake bound = %s, want 15s", got)
	}
	for _, id := range []string{"strong-1", ""} {
		if got := handshakeTimeout(id); got != 10*time.Second {
			t.Errorf("handshake bound of %q = %s, want 10s", id, got)
		}
	}
	SetWeakDeviceFunc(nil)
	if got := handshakeTimeout("weak-1"); got != 10*time.Second {
		t.Errorf("without a class hook: %s, want 10s (no device is weak)", got)
	}
}

// RunCommand applies those defaults; a caller's own timeout is kept as it is.
func TestRunCommandUsesTheDefaultDeadline(t *testing.T) {
	oldC, oldW, oldS := CommandTimeout, WeakCommandTimeout, SlowCommandTimeout
	CommandTimeout, WeakCommandTimeout, SlowCommandTimeout = 40*time.Millisecond, 70*time.Millisecond, 110*time.Millisecond
	t.Cleanup(func() { CommandTimeout, WeakCommandTimeout, SlowCommandTimeout = oldC, oldW, oldS })
	weakIs(t, "weak-1")

	for _, tc := range []struct {
		name, device, command string
		want                  time.Duration
	}{
		{"default", "strong-1", "/interface/print", 40 * time.Millisecond},
		{"C1", "weak-1", "/interface/print", 70 * time.Millisecond},
		{"slow read", "strong-1", "/interface/bridge/host/print", 110 * time.Millisecond},
		{"slow read on C1", "weak-1", "/log/print", 110 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := dialOnceAs(t, newHangingDevice(t), tc.device)
			_, err := RunCommand(client, tc.command)
			var te *TimeoutError
			if !errors.As(err, &te) || te.Queued || te.After != tc.want {
				t.Fatalf("err = %v, want a timeout after %s", err, tc.want)
			}
		})
	}

	t.Run("a caller's longer timeout is kept", func(t *testing.T) {
		client := dialOnceAs(t, newHangingDevice(t), "strong-1")
		_, err := RunCommandWithTimeout(client, 150*time.Millisecond, "/tool/fetch")
		var te *TimeoutError
		if !errors.As(err, &te) || te.After != 150*time.Millisecond {
			t.Fatalf("err = %v, want a timeout after the caller's 150ms", err)
		}
		if _, err := (OnceRunner{Client: client, Timeout: time.Hour}).Run("/x"); !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("after the timeout the session must be closed, got %v", err)
		}
	})
}

// An OnceRunner without a timeout of its own gives a firmware job's commands
// 30 s, not the pollers' 10.
func TestOnceRunnerDefault(t *testing.T) {
	if OnceCommandTimeout != 30*time.Second {
		t.Errorf("OnceCommandTimeout = %s, want 30s", OnceCommandTimeout)
	}
	old := OnceCommandTimeout
	OnceCommandTimeout = 90 * time.Millisecond
	t.Cleanup(func() { OnceCommandTimeout = old })
	client := dialOnceAs(t, newHangingDevice(t), "")
	_, err := (OnceRunner{Client: client}).Run("/system/package/update/check-for-updates")
	var te *TimeoutError
	if !errors.As(err, &te) || te.After != 90*time.Millisecond {
		t.Fatalf("err = %v, want a timeout after OnceCommandTimeout", err)
	}
}
