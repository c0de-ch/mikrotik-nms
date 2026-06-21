package api

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/config"
)

// fakeSMTPListener speaks just enough plaintext SMTP (tls_mode="none") to accept
// one message, so handleTestMail's happy path can be exercised without a real
// relay. Mirrors the mailer package's own test harness. Returns the listener
// address and a stop func.
func fakeSMTPListener(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := bufio.NewWriter(conn)
		send := func(s string) {
			_, _ = w.WriteString(s + "\r\n")
			_ = w.Flush()
		}
		send("220 fake ESMTP")
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
					send("250 OK queued")
				}
				continue
			}
			switch up := strings.ToUpper(trimmed); {
			case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
				send("250 fake")
			case strings.HasPrefix(up, "DATA"):
				send("354 End data with <CR><LF>.<CR><LF>")
				inData = true
			case strings.HasPrefix(up, "QUIT"):
				send("221 Bye")
				return
			default:
				send("250 OK")
			}
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func TestHandleTestMail(t *testing.T) {
	// Empty cfg exercises the PublicBaseURL="test" force-satisfy branch; the empty
	// settings DB makes resolveMailerConfig fall back to (empty) env for every key.
	s := &Server{db: newTestDB(t), cfg: &config.Config{}}

	post := func(body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/settings/mail/test", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleTestMail(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return out
	}

	// Empty recipient -> ok=false, no send attempted.
	if out := post(`{"to":""}`); out["ok"] != false {
		t.Fatalf("empty recipient should be ok=false, got %+v", out)
	}

	// No host/port anywhere -> ok=false.
	if out := post(`{"to":"u@example.com"}`); out["ok"] != false {
		t.Fatalf("missing host/port should be ok=false, got %+v", out)
	}

	// Dead host:port -> ok=false with a non-empty error message.
	out := post(`{"to":"u@example.com","host":"127.0.0.1","port":"1","from":"nms@example.com","tls_mode":"none"}`)
	if out["ok"] != false {
		t.Fatalf("dead relay should be ok=false, got %+v", out)
	}
	if msg, _ := out["message"].(string); msg == "" {
		t.Fatalf("dead relay should include an error message, got %+v", out)
	}

	// Happy path against an in-process fake SMTP relay.
	addr, stop := fakeSMTPListener(t)
	defer stop()
	_, portStr, _ := net.SplitHostPort(addr)
	body := `{"to":"u@example.com","host":"127.0.0.1","port":"` + portStr +
		`","from":"nms@example.com","tls_mode":"none"}`
	if out := post(body); out["ok"] != true {
		t.Fatalf("happy path should be ok=true, got %+v", out)
	}
}
