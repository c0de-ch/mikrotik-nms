package routeros

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	ros "github.com/go-routeros/routeros/v3"
)

const testPassword = "hunter2-CORRECT-horse"

// lockedBuffer is a log sink several goroutines may write to.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func debugLog() (*lockedBuffer, slog.Handler) {
	buf := &lockedBuffer{}
	return buf, slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
}

// The leak this guards against is real: the library logs the login sentence,
// password included, to whatever Debug handler it is given.
func TestLibraryLogsThePasswordAtDebug(t *testing.T) {
	dev := newTestDevice(t)
	conn, err := net.Dial("tcp", JoinHostPort(dev.host, dev.port))
	if err != nil {
		t.Fatal(err)
	}
	client, err := ros.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	buf, handler := debugLog()
	client.SetLogHandler(handler)
	if err := client.Login("nms", testPassword); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), testPassword) {
		t.Fatalf("the library no longer logs the login sentence at Debug — this control and redact.go need another look:\n%s", buf.String())
	}
}

// With the process logger at Debug, a login and a command through this
// package leave no trace of the password — on every dial path.
func TestPasswordIsNeverLogged(t *testing.T) {
	buf, handler := debugLog()
	old := clientLogBase
	clientLogBase = func() slog.Handler { return handler }
	t.Cleanup(func() { clientLogBase = old })

	dev := newTestDevice(t)
	pool := NewPool()
	t.Cleanup(pool.CloseAll)

	pooled, err := pool.Dial("d1", dev.target(), "nms", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	once, err := DialOnce(dev.target(), "nms", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	defer once.Close()
	with, err := DialOnceWith(dev.target(), NewCredentials("nms", testPassword))
	if err != nil {
		t.Fatal(err)
	}
	defer with.Close()
	for _, c := range []*ros.Client{pooled, once, with} {
		if _, err := RunCommand(c, "/system/identity/print"); err != nil {
			t.Fatal(err)
		}
	}
	// A refused login must not print the password it refused either.
	dev.refuse.Store(true)
	if _, err := DialOnce(dev.target(), "nms", testPassword); !IsLoginRefused(err) {
		t.Fatalf("err = %v, want a refused login", err)
	} else if strings.Contains(err.Error(), testPassword) {
		t.Fatalf("the dial error carries the password: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, testPassword) || strings.Contains(out, "=password=") {
		t.Fatalf("the client log contains the login secret:\n%s", out)
	}
}

// The handler itself: nothing below Info, and no attribute that carries a
// login secret at any level, however it is wrapped.
func TestClientLogHandlerRedacts(t *testing.T) {
	buf, handler := debugLog()
	old := clientLogBase
	clientLogBase = func() slog.Handler { return handler }
	t.Cleanup(func() { clientLogBase = old })

	h := clientLogHandler{}
	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("the client log handler is enabled at Debug")
	}
	if !h.Enabled(context.Background(), slog.LevelInfo) || !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("the client log handler drops Info or Error")
	}

	login := []string{"/login", "=name=nms", "=password=" + testPassword}
	logger := slog.New(h)
	logger.Debug("RunArgsContext", slog.Any("sentences", login))
	logger.Info("RunArgsContext", slog.Any("sentences", login), slog.String("kept", "yes-1"))
	logger.Warn("as a string", slog.String("sentence", strings.Join(login, " ")), slog.String("kept", "yes-2"))
	logger.Error("nested", slog.Group("req", slog.Any("words", login)), slog.String("kept", "yes-3"))
	logger.Error("old login", slog.Any("sentences", []string{"/login", "=name=nms", "=response=00" + testPassword}))
	logger.With(slog.Any("sentences", login), slog.String("kept", "yes-4")).Info("with attrs")
	logger.WithGroup("g").Info("grouped", slog.Any("sentences", login), slog.String("kept", "yes-5"))

	out := buf.String()
	if strings.Contains(out, testPassword) || strings.Contains(out, "=password=") || strings.Contains(out, "=response=") {
		t.Fatalf("a login secret got through the handler:\n%s", out)
	}
	for _, want := range []string{"yes-1", "yes-2", "yes-3", "yes-4", "yes-5", "with attrs", "grouped", "old login"} {
		if !strings.Contains(out, want) {
			t.Errorf("the handler dropped %q, which carries no secret:\n%s", want, out)
		}
	}
	if strings.Contains(out, "level=DEBUG") {
		t.Errorf("a Debug record got through:\n%s", out)
	}
}
