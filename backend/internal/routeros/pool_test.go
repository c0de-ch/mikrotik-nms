package routeros

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
)

// These tests drive the pool through its seams — a clock, a jitter and a
// dial function — so nothing in them waits on real time. The sessions they
// hand out are real ones to a loopback listener (testDevice) where a test
// needs a client.

// fakeClock is the pool's time in a test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var errDown = errors.New("dial tcp: connection refused")

// scriptedPool returns a pool on a fake clock, without jitter, whose dials
// are answered by next (counted in *dials).
func scriptedPool(next func(spec dialSpec) (*ros.Client, error)) (*Pool, *fakeClock, *atomic.Int32) {
	p, clock, dials := NewPool(), newClock(), &atomic.Int32{}
	p.now = clock.now
	p.jitter = func() float64 { return 0 }
	p.dialer = func(spec dialSpec) (*ros.Client, error) {
		dials.Add(1)
		return next(spec)
	}
	return p, clock, dials
}

func failing(dialSpec) (*ros.Client, error) { return nil, errDown }

func specFor(addr string) dialSpec {
	return dialSpec{target: Target{Address: addr, Port: 8728}, username: "u", password: "p"}
}

// GetLive never invents a connection for a device nobody dialled.
func TestGetLiveNoSpec(t *testing.T) {
	p, _, dials := scriptedPool(failing)
	if c := p.GetLive("never-dialled"); c != nil {
		t.Error("GetLive invented a connection for an unknown device")
	}
	if dials.Load() != 0 {
		t.Errorf("dials = %d, want 0", dials.Load())
	}
}

// A device that is down costs one dial per backoff step, however often the
// pollers ask: 15 s, 30 s, 1 min … capped at 10 min.
func TestGetLiveBacksOffExponentially(t *testing.T) {
	p, clock, dials := scriptedPool(failing)
	p.specs["d1"] = specFor("192.0.2.1")

	want := []time.Duration{
		15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
		4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute,
	}
	for i, step := range want {
		if c := p.GetLive("d1"); c != nil {
			t.Fatal("GetLive returned a client from a dial that fails")
		}
		if got := dials.Load(); got != int32(i+1) {
			t.Fatalf("after failure %d: dials = %d, want %d", i+1, got, i+1)
		}
		// Asking again inside the step dials nothing…
		for range 5 {
			p.GetLive("d1")
		}
		clock.advance(step - time.Second)
		p.GetLive("d1")
		if got := dials.Load(); got != int32(i+1) {
			t.Fatalf("step %d (%s): a dial was made %s into it", i+1, step, step-time.Second)
		}
		// …and the first ask after it dials once.
		clock.advance(time.Second)
	}
}

// The backoff is spread by ±20 %.
func TestBackoffJitter(t *testing.T) {
	for _, tc := range []struct {
		jitter float64
		want   time.Duration
	}{{-1, 12 * time.Second}, {0, 15 * time.Second}, {0.5, 16500 * time.Millisecond}, {1, 18 * time.Second}} {
		p, clock, _ := scriptedPool(failing)
		p.jitter = func() float64 { return tc.jitter }
		p.specs["d1"] = specFor("192.0.2.1")
		p.GetLive("d1")
		if got := p.backoff["d1"].next.Sub(clock.now()); got != tc.want {
			t.Errorf("jitter %+.1f: first backoff = %s, want %s", tc.jitter, got, tc.want)
		}
	}
	// At the cap: 10 min ±20 %.
	p, clock, _ := scriptedPool(failing)
	p.jitter = func() float64 { return 1 }
	p.specs["d1"] = specFor("192.0.2.1")
	for range 12 {
		p.GetLive("d1")
		clock.advance(time.Hour)
	}
	p.GetLive("d1")
	if got := p.backoff["d1"].next.Sub(clock.now()); got != 12*time.Minute {
		t.Errorf("capped backoff with +20%% jitter = %s, want 12m", got)
	}
}

func TestBackoffDelay(t *testing.T) {
	base, limit := 15*time.Second, 10*time.Minute
	for n, want := range map[int]time.Duration{
		1: 15 * time.Second, 2: 30 * time.Second, 3: time.Minute, 6: 8 * time.Minute,
		7: 10 * time.Minute, 8: 10 * time.Minute, 500: 10 * time.Minute,
	} {
		if got := backoffDelay(n, base, limit); got != want {
			t.Errorf("backoffDelay(%d) = %s, want %s", n, got, want)
		}
	}
}

// A success ends the backoff: the next failure starts again at the first
// step. Between two automatic dials there is always the cooldown, so a
// session that dies right after its login is not redialled in a loop.
func TestSuccessResetsBackoff(t *testing.T) {
	dev := newTestDevice(t)
	up := false
	p, clock, dials := scriptedPool(func(spec dialSpec) (*ros.Client, error) {
		if !up {
			return nil, errDown
		}
		return dialSession(spec.target, spec.username, spec.password)
	})
	p.specs["d1"] = dialSpec{target: dev.target(), username: "u", password: "p"}

	for range 4 { // 15 s, 30 s, 1 min, 2 min
		p.GetLive("d1")
		clock.advance(time.Hour)
	}
	if got := p.backoff["d1"].failures; got != 4 {
		t.Fatalf("failures = %d, want 4", got)
	}
	up = true
	c := p.GetLive("d1")
	if c == nil {
		t.Fatal("no client once the device is back")
	}
	if b := p.backoff["d1"]; b.failures != 0 || b.lastErr != nil {
		t.Fatalf("after a success: failures = %d, lastErr = %v; want a clean state", b.failures, b.lastErr)
	}

	// The session ends right away; the device is down again.
	up = false
	before := dials.Load()
	_ = c.Close()
	if p.GetLive("d1") != nil || dials.Load() != before {
		t.Fatal("a session that ended inside the cooldown was redialled at once")
	}
	clock.advance(ReconnectCooldown)
	p.GetLive("d1")
	if got := dials.Load(); got != before+1 {
		t.Fatalf("dials after the cooldown = %d, want %d", got, before+1)
	}
	if got := p.backoff["d1"].next.Sub(clock.now()); got != 15*time.Second {
		t.Errorf("first backoff after a success = %s, want 15s (the first step again)", got)
	}
}

// Forget (the device was edited) and ResetBackoff (its API port answers
// again) both let the next dial happen at once.
func TestForgetAndResetEndTheBackoff(t *testing.T) {
	p, clock, dials := scriptedPool(failing)
	spec := specFor("192.0.2.1")
	for range 6 {
		_, _ = p.EnsureConnection("d1", spec.target, "u", "p")
		clock.advance(time.Hour)
	}
	_, _ = p.EnsureConnection("d1", spec.target, "u", "p") // now waiting ~10 min
	n := dials.Load()

	if p.GetLive("d1") != nil || dials.Load() != n {
		t.Fatal("a dial was made inside the backoff")
	}
	p.ResetBackoff("d1")
	p.GetLive("d1")
	if got := dials.Load(); got != n+1 {
		t.Fatalf("dials after ResetBackoff = %d, want %d", got, n+1)
	}
	if got := p.backoff["d1"].next.Sub(clock.now()); got != 15*time.Second {
		t.Errorf("backoff after ResetBackoff = %s, want 15s (counting from one failure)", got)
	}

	p.Forget("d1")
	if _, ok := p.backoff["d1"]; ok {
		t.Error("Forget kept the backoff")
	}
	if p.GetLive("d1") != nil {
		t.Error("Forget kept the dial parameters")
	}
	if _, err := p.EnsureConnection("d1", spec.target, "u", "new-password"); !errors.Is(err, errDown) {
		t.Fatalf("EnsureConnection after Forget = %v, want the dial error (a dial was made)", err)
	}
	if got := dials.Load(); got != n+2 {
		t.Fatalf("dials after Forget = %d, want %d", got, n+2)
	}
}

// EnsureConnection makes one dial, not three with sleeps in between, and
// honours the backoff: inside it no dial is made and the error says so,
// carrying the last dial's error.
func TestEnsureConnectionHonoursBackoff(t *testing.T) {
	p, clock, dials := scriptedPool(failing)
	tgt := Target{Address: "192.0.2.1", Port: 8728}

	if _, err := p.EnsureConnection("d1", tgt, "u", "p"); !errors.Is(err, errDown) {
		t.Fatalf("first EnsureConnection = %v, want the dial error", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1 (no retry loop)", got)
	}
	_, err := p.EnsureConnection("d1", tgt, "u", "p")
	var held *BackoffError
	if !errors.As(err, &held) || !errors.Is(err, errDown) {
		t.Fatalf("EnsureConnection inside the backoff = %v, want a BackoffError wrapping the dial error", err)
	}
	if want := clock.now().Add(15 * time.Second); !held.Until.Equal(want) {
		t.Errorf("held until %s, want %s", held.Until, want)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials inside the backoff = %d, want 1", got)
	}
	// The parameters were remembered although the dial failed: once the
	// wait is over the pollers' GetLive retries on its own.
	clock.advance(15 * time.Second)
	p.GetLive("d1")
	if got := dials.Load(); got != 2 {
		t.Fatalf("dials after the backoff = %d, want 2", got)
	}
}

// The backoff belongs to one pool key. The address-move verification key and
// an explicit Dial are not held back by the polling key's failures.
func TestBackoffIsPerKeyAndSparesExplicitDials(t *testing.T) {
	p, _, dials := scriptedPool(failing)
	tgt := Target{DeviceID: "d1", Address: "192.0.2.1", Port: 8728}
	for range 3 {
		_, _ = p.EnsureConnection("d1", tgt, "u", "p")
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
	if _, err := p.Dial("d1:verify", tgt, "u", "p"); !errors.Is(err, errDown) {
		t.Fatalf("verify dial = %v, want it to dial (and fail with the dial error)", err)
	}
	if _, err := p.Dial("d1", tgt, "u", "p"); !errors.Is(err, errDown) {
		t.Fatalf("explicit dial = %v, want it to dial", err)
	}
	if got := dials.Load(); got != 3 {
		t.Fatalf("dials = %d, want 3", got)
	}
	if _, err := p.EnsureConnection("d2", Target{Address: "192.0.2.2", Port: 8728}, "u", "p"); !errors.Is(err, errDown) {
		t.Fatalf("another device = %v, want it to dial", err)
	}
}

// A refused login is not retried on the short steps: the same password will
// be refused again, and each attempt is a line in the device's log.
func TestLoginRefusedBacksOffToTheMaximum(t *testing.T) {
	dev := newTestDevice(t)
	dev.refuse.Store(true)
	p, clock, dials := scriptedPool(func(spec dialSpec) (*ros.Client, error) {
		return dialSession(spec.target, spec.username, spec.password)
	})
	_, err := p.EnsureConnection("d1", dev.target(), "u", "wrong")
	if !IsLoginRefused(err) {
		t.Fatalf("EnsureConnection = %v, want a refused login", err)
	}
	if got := p.backoff["d1"].next.Sub(clock.now()); got != ReconnectBackoffMax {
		t.Fatalf("backoff after a refused login = %s, want %s", got, ReconnectBackoffMax)
	}
	clock.advance(ReconnectBackoffMax - time.Second)
	p.GetLive("d1")
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
}

// Two pollers that find a device without a session cause one dial and get
// the same client — also when one of them started it through
// EnsureConnection and the other asked with GetLive.
func TestConcurrentCallersShareOneDial(t *testing.T) {
	dev := newTestDevice(t)
	entered, release := make(chan struct{}), make(chan struct{})
	p, _, dials := scriptedPool(func(spec dialSpec) (*ros.Client, error) {
		close(entered)
		<-release
		return dialSession(spec.target, spec.username, spec.password)
	})
	joined := make(chan struct{}, 16)
	p.joined = func() { joined <- struct{}{} }

	const waiters = 4
	results := make(chan *ros.Client, waiters+1)
	go func() {
		c, _ := p.EnsureConnection("d1", dev.target(), "u", "p")
		results <- c
	}()
	<-entered // the one dial is running
	for i := range waiters {
		go func() {
			if i%2 == 0 {
				results <- p.GetLive("d1")
				return
			}
			c, _ := p.EnsureConnection("d1", dev.target(), "u", "p")
			results <- c
		}()
	}
	for range waiters {
		<-joined // every other caller is waiting for it
	}
	close(release)

	first := <-results
	if first == nil {
		t.Fatal("the dial failed")
	}
	for range waiters {
		if c := <-results; c != first {
			t.Fatalf("a caller got %p, want the shared client %p", c, first)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
	if got := dev.conns.Load(); got != 1 {
		t.Fatalf("connections at the device = %d, want 1", got)
	}
}

// A failed dial is shared too: every waiter gets its error, nobody redials.
func TestConcurrentCallersShareOneFailure(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p, _, dials := scriptedPool(func(dialSpec) (*ros.Client, error) {
		close(entered)
		<-release
		return nil, errDown
	})
	joined := make(chan struct{}, 4)
	p.joined = func() { joined <- struct{}{} }
	tgt := Target{Address: "192.0.2.1", Port: 8728}

	errs := make(chan error, 3)
	go func() { _, err := p.EnsureConnection("d1", tgt, "u", "p"); errs <- err }()
	<-entered
	for range 2 {
		go func() { _, err := p.Dial("d1", tgt, "u", "p"); errs <- err }()
	}
	<-joined
	<-joined
	close(release)
	for range 3 {
		if err := <-errs; !errors.Is(err, errDown) {
			t.Fatalf("a caller got %v, want the shared dial error", err)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}
}

// The pool lock is not held while a dial runs: another device's session is
// handed out, and another device is dialled, while one dial hangs.
func TestDialDoesNotBlockOtherDevices(t *testing.T) {
	dev := newTestDevice(t)
	entered, release := make(chan struct{}), make(chan struct{})
	p, _, _ := scriptedPool(func(spec dialSpec) (*ros.Client, error) {
		if spec.target.Address == "192.0.2.99" {
			close(entered)
			<-release
			return nil, errDown
		}
		return dialSession(spec.target, spec.username, spec.password)
	})
	if _, err := p.Dial("good", dev.target(), "u", "p"); err != nil {
		t.Fatal(err)
	}

	hung := make(chan struct{})
	go func() {
		_, _ = p.Dial("hung", Target{Address: "192.0.2.99", Port: 8728}, "u", "p")
		close(hung)
	}()
	<-entered

	// With the lock held across the dial every one of these would block
	// until the test timed out.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if p.Get("good") == nil || p.GetLive("good") == nil {
			t.Error("the other device's session is not handed out during a dial")
		}
		if _, err := p.Dial("good2", dev.target(), "u", "p"); err != nil {
			t.Errorf("dialling another device during a dial: %v", err)
		}
		p.Close("good2")
		p.ResetBackoff("hung")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pool operations on other devices blocked behind a dial in flight")
	}
	close(release)
	<-hung
}

// A device edited while a dial to it was running: the session that dial
// opens belongs to the old parameters and is closed, not pooled.
func TestForgetSupersedesADialInFlight(t *testing.T) {
	dev := newTestDevice(t)
	entered, release := make(chan struct{}), make(chan struct{})
	p, _, _ := scriptedPool(func(spec dialSpec) (*ros.Client, error) {
		close(entered)
		<-release
		return dialSession(spec.target, spec.username, spec.password)
	})
	result := make(chan error, 1)
	go func() { _, err := p.EnsureConnection("d1", dev.target(), "u", "old"); result <- err }()
	<-entered
	p.Forget("d1")
	close(release)

	if err := <-result; !errors.Is(err, ErrDialSuperseded) {
		t.Fatalf("EnsureConnection = %v, want ErrDialSuperseded", err)
	}
	if p.Get("d1") != nil || p.GetLive("d1") != nil {
		t.Fatal("the superseded session was pooled (or its parameters kept)")
	}
	dev.waitClosed(t, 1)
}

// EnsureConnection replaces a session that was opened with other
// parameters: a changed address, port, TLS flag, pin or credential must not
// keep polling over the old session.
func TestEnsureConnectionReplacesASessionWithOtherParameters(t *testing.T) {
	dev := newTestDevice(t)
	p, _, _ := scriptedPool(func(spec dialSpec) (*ros.Client, error) {
		return dialSession(spec.target, spec.username, spec.password)
	})
	first, err := p.EnsureConnection("d1", dev.target(), "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	same, err := p.EnsureConnection("d1", dev.target(), "u", "p")
	if err != nil || same != first {
		t.Fatalf("unchanged parameters: got %p, %v; want the pooled session %p", same, err, first)
	}
	second, err := p.EnsureConnection("d1", dev.target(), "u", "new-password")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("a changed password kept the old session")
	}
	if got := dev.conns.Load(); got != 2 {
		t.Fatalf("connections = %d, want 2", got)
	}
	dev.waitClosed(t, 1)
	if _, err := RunCommand(first, "/system/identity/print"); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("command on the replaced session = %v, want ErrSessionClosed", err)
	}
}

// A dial that panics fails like any other dial: its waiters are released
// with an error, the backoff starts, and the device can be dialled again —
// nobody waits for ever on a dial that will never report back.
func TestPanickingDialDoesNotStrandItsWaiters(t *testing.T) {
	dev := newTestDevice(t)
	entered, release := make(chan struct{}), make(chan struct{})
	broken := true
	p, clock, dials := scriptedPool(func(spec dialSpec) (*ros.Client, error) {
		if broken {
			close(entered)
			<-release
			panic("slice bounds out of range")
		}
		return dialSession(spec.target, spec.username, spec.password)
	})
	joined := make(chan struct{}, 1)
	p.joined = func() { joined <- struct{}{} }

	errs := make(chan error, 2)
	go func() { _, err := p.EnsureConnection("d1", dev.target(), "u", "p"); errs <- err }()
	<-entered
	go func() { _, err := p.EnsureConnection("d1", dev.target(), "u", "p"); errs <- err }()
	<-joined
	close(release)
	for range 2 {
		select {
		case err := <-errs:
			if err == nil || !strings.Contains(err.Error(), "panic") {
				t.Fatalf("err = %v, want the contained panic", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a caller is still waiting for a dial that panicked")
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1", got)
	}

	// It counted as a failed dial, and the next one works.
	if p.GetLive("d1") != nil {
		t.Fatal("redialled inside the backoff")
	}
	broken = false
	clock.advance(ReconnectCooldown)
	if p.GetLive("d1") == nil {
		t.Fatal("the device cannot be dialled again after a panicking dial")
	}
}
