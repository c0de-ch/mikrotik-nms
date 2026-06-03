package mailer

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

func TestDisabledMailer(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no host", Config{SMTPPort: 587, PublicBaseURL: "https://nms.example.com"}},
		{"no base url", Config{SMTPHost: "smtp.example.com", SMTPPort: 587}},
		{"no port", Config{SMTPHost: "smtp.example.com", PublicBaseURL: "https://nms.example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.cfg)
			if m.Enabled() {
				t.Fatal("mailer should be disabled")
			}
			// Send must be a no-op that returns ErrDisabled WITHOUT dialing.
			// (A bogus host guarantees a dial would fail loudly if attempted.)
			if err := m.Send("to@example.com", "s", "t", ""); !errors.Is(err, ErrDisabled) {
				t.Fatalf("Send err = %v, want ErrDisabled", err)
			}
			if err := m.SendPasswordResetEmail("to@example.com", "https://nms.example.com/reset-password?token=x"); !errors.Is(err, ErrDisabled) {
				t.Fatalf("SendPasswordResetEmail err = %v, want ErrDisabled", err)
			}
		})
	}
}

func TestEnabledRequiresAllThree(t *testing.T) {
	m := New(Config{SMTPHost: "smtp.example.com", SMTPPort: 587, PublicBaseURL: "https://nms.example.com"})
	if !m.Enabled() {
		t.Fatal("mailer should be enabled when host+port+base url are all set")
	}
}

func TestComposeIncludesHeadersAndBody(t *testing.T) {
	m := New(Config{SMTPHost: "smtp.example.com", SMTPPort: 587, PublicBaseURL: "https://nms.example.com", SMTPFrom: "nms@example.com"})
	msg, err := m.compose("nms@example.com", "user@example.com", "Reset your MikroTik NMS password", "plain link: https://nms.example.com/reset-password?token=abc", "<a>html</a>")
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	for _, want := range []string{
		"From: nms@example.com",
		"To: user@example.com",
		"Subject: Reset your MikroTik NMS password",
		"MIME-Version: 1.0",
		"Content-Type: multipart/alternative",
		"text/plain",
		"text/html",
		"https://nms.example.com/reset-password?token=abc",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("composed message missing %q\n---\n%s", want, msg)
		}
	}
}

func TestSendRejectsInvalidAddresses(t *testing.T) {
	m := New(Config{SMTPHost: "smtp.example.com", SMTPPort: 587, PublicBaseURL: "https://nms.example.com", SMTPFrom: "nms@example.com"})
	if err := m.Send("not-an-address", "s", "t", ""); err == nil {
		t.Fatal("expected error for invalid to address")
	}
}

// TestSendHappyPath drives Send against a tiny in-process SMTP server (TLS mode
// "none") and asserts the DATA payload carries the reset link.
func TestSendHappyPath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var (
		mu      sync.Mutex
		dataBuf strings.Builder
	)
	go fakeSMTPServer(t, ln, &mu, &dataBuf)

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port := atoiPort(t, portStr)

	m := New(Config{
		SMTPHost:      "127.0.0.1",
		SMTPPort:      port,
		SMTPFrom:      "nms@example.com",
		SMTPTLSMode:   "none",
		PublicBaseURL: "https://nms.example.com",
	})
	if !m.Enabled() {
		t.Fatal("mailer should be enabled")
	}
	resetURL := "https://nms.example.com/reset-password?token=happypath123"
	if err := m.SendPasswordResetEmail("user@example.com", resetURL); err != nil {
		t.Fatalf("SendPasswordResetEmail: %v", err)
	}

	mu.Lock()
	got := dataBuf.String()
	mu.Unlock()
	if !strings.Contains(got, resetURL) {
		t.Errorf("DATA payload missing reset URL\n---\n%s", got)
	}
	if !strings.Contains(got, "Subject: Reset your MikroTik NMS password") {
		t.Errorf("DATA payload missing subject\n---\n%s", got)
	}
}

// fakeSMTPServer speaks just enough SMTP to accept one message and capture DATA.
func fakeSMTPServer(t *testing.T, ln net.Listener, mu *sync.Mutex, dataBuf *strings.Builder) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	writeLine := func(s string) {
		w.WriteString(s + "\r\n")
		w.Flush()
	}

	writeLine("220 fake ESMTP")
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")

		if inData {
			if trimmed == "." {
				inData = false
				writeLine("250 OK queued")
				continue
			}
			mu.Lock()
			dataBuf.WriteString(line)
			mu.Unlock()
			continue
		}

		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			writeLine("250 fake")
		case strings.HasPrefix(upper, "MAIL FROM"):
			writeLine("250 OK")
		case strings.HasPrefix(upper, "RCPT TO"):
			writeLine("250 OK")
		case strings.HasPrefix(upper, "DATA"):
			writeLine("354 End data with <CR><LF>.<CR><LF>")
			inData = true
		case strings.HasPrefix(upper, "QUIT"):
			writeLine("221 Bye")
			return
		default:
			writeLine("250 OK")
		}
	}
}

func atoiPort(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("bad port %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}
