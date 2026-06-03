package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

const prtSecret = "a-sufficiently-long-test-secret-value-1234567890"

// fakeMailer captures sends so tests can assert recipient/link, and signals via
// a buffered channel so the async goroutine send can be awaited deterministically.
type fakeMailer struct {
	enabled bool
	mu      sync.Mutex
	sends   []sentMail
	signal  chan struct{}
}

type sentMail struct {
	to  string
	url string
}

func newFakeMailer(enabled bool) *fakeMailer {
	return &fakeMailer{enabled: enabled, signal: make(chan struct{}, 16)}
}

func (f *fakeMailer) Enabled() bool { return f.enabled }

func (f *fakeMailer) Send(to, subject, textBody, htmlBody string) error {
	f.mu.Lock()
	f.sends = append(f.sends, sentMail{to: to, url: textBody})
	f.mu.Unlock()
	f.signal <- struct{}{}
	return nil
}

func (f *fakeMailer) SendPasswordResetEmail(to, resetURL string) error {
	f.mu.Lock()
	f.sends = append(f.sends, sentMail{to: to, url: resetURL})
	f.mu.Unlock()
	f.signal <- struct{}{}
	return nil
}

func (f *fakeMailer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

func (f *fakeMailer) last() (sentMail, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) == 0 {
		return sentMail{}, false
	}
	return f.sends[len(f.sends)-1], true
}

// waitForSend blocks until one email is captured or the timeout elapses.
func (f *fakeMailer) waitForSend(t *testing.T) {
	t.Helper()
	select {
	case <-f.signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async email send")
	}
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestServer(t *testing.T, db *sql.DB, m *fakeMailer) *Server {
	t.Helper()
	cfg := &config.Config{
		JWTSecret:        prtSecret,
		SMTPHost:         "smtp.example.com",
		SMTPPort:         587,
		PublicBaseURL:    "https://nms.example.com",
		PasswordResetTTL: time.Hour,
	}
	return &Server{db: db, cfg: cfg, mailer: m}
}

// resetRouter wires just the public reset endpoints (with the per-username
// limiter) plus refresh, so tests exercise the same middleware as production.
func resetRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	limiter := newRateLimiter(3, 15*time.Minute)
	r.Post("/api/v1/auth/request-reset", s.limitResetPerUser(limiter, s.handleRequestReset))
	r.Post("/api/v1/auth/perform-reset", s.handlePerformReset)
	r.Post("/api/v1/auth/refresh", s.handleRefresh)
	return r
}

func seedUser(t *testing.T, db *sql.DB, username, password string) *queries.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u := &queries.User{ID: uuid.NewString(), Username: username, PasswordHash: hash, Role: "admin"}
	if err := queries.CreateUser(db, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "10.0.0.1:1234"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// requestReset issues the most-recently emailed token for username by reaching
// into the fake mailer (mirrors the user clicking the link).
func tokenFromLink(t *testing.T, link string) string {
	t.Helper()
	i := strings.Index(link, "token=")
	if i < 0 {
		t.Fatalf("no token in link %q", link)
	}
	return link[i+len("token="):]
}

func TestRequestResetEnumerationSafe(t *testing.T) {
	db := newTestDB(t)
	mailer := newFakeMailer(true)
	s := newTestServer(t, db, mailer)
	h := resetRouter(s)

	seedUser(t, db, "real@example.com", "originalpw")

	rrReal := postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	rrFake := postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "ghost@example.com"})

	if rrReal.Code != http.StatusOK || rrFake.Code != http.StatusOK {
		t.Fatalf("status real=%d fake=%d, want 200/200", rrReal.Code, rrFake.Code)
	}
	if rrReal.Body.String() != rrFake.Body.String() {
		t.Fatalf("bodies differ: real=%q fake=%q", rrReal.Body.String(), rrFake.Body.String())
	}
	if got := strings.TrimSpace(rrReal.Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("body = %q, want %q", got, `{"status":"ok"}`)
	}

	// Exactly one email — for the real user — with a link off the public base URL.
	mailer.waitForSend(t)
	if mailer.count() != 1 {
		t.Fatalf("send count = %d, want 1 (only the real user)", mailer.count())
	}
	sent, _ := mailer.last()
	if sent.to != "real@example.com" {
		t.Errorf("email to = %q, want real@example.com", sent.to)
	}
	if !strings.HasPrefix(sent.url, "https://nms.example.com/reset-password?token=") {
		t.Errorf("link = %q, want public-base-url reset link", sent.url)
	}
}

func TestRequestResetSMTPDisabled(t *testing.T) {
	db := newTestDB(t)
	mailer := newFakeMailer(false) // disabled
	s := newTestServer(t, db, mailer)
	h := resetRouter(s)
	seedUser(t, db, "real@example.com", "originalpw")

	rr := postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	// Give any (erroneous) async send a moment; there should be none.
	time.Sleep(50 * time.Millisecond)
	if mailer.count() != 0 {
		t.Fatalf("send count = %d, want 0 when SMTP disabled", mailer.count())
	}
}

func TestRequestResetKillSwitch(t *testing.T) {
	db := newTestDB(t)
	mailer := newFakeMailer(true)
	s := newTestServer(t, db, mailer)
	h := resetRouter(s)
	seedUser(t, db, "real@example.com", "originalpw")

	if err := queries.SetSetting(db, "pwreset_enabled", "false"); err != nil {
		t.Fatalf("set setting: %v", err)
	}

	rr := postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if mailer.count() != 0 {
		t.Fatalf("send count = %d, want 0 when kill-switch off", mailer.count())
	}
}

func TestPerformResetHappyPath(t *testing.T) {
	db := newTestDB(t)
	mailer := newFakeMailer(true)
	s := newTestServer(t, db, mailer)
	h := resetRouter(s)
	seedUser(t, db, "real@example.com", "originalpw")

	postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	mailer.waitForSend(t)
	sent, _ := mailer.last()
	token := tokenFromLink(t, sent.url)

	rr := postJSON(t, h, "/api/v1/auth/perform-reset", map[string]string{"token": token, "new_password": "brandnewpw"})
	if rr.Code != http.StatusOK {
		t.Fatalf("perform-reset status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}

	// New password works, old one no longer does.
	u, err := queries.GetUserByUsername(db, "real@example.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if !auth.CheckPassword(u.PasswordHash, "brandnewpw") {
		t.Error("new password should authenticate")
	}
	if auth.CheckPassword(u.PasswordHash, "originalpw") {
		t.Error("old password should no longer authenticate")
	}
	if u.TokenVersion != 1 {
		t.Errorf("token_version = %d, want 1 after reset", u.TokenVersion)
	}
}

func TestPerformResetSingleUse(t *testing.T) {
	db := newTestDB(t)
	mailer := newFakeMailer(true)
	s := newTestServer(t, db, mailer)
	h := resetRouter(s)
	seedUser(t, db, "real@example.com", "originalpw")

	postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	mailer.waitForSend(t)
	sent, _ := mailer.last()
	token := tokenFromLink(t, sent.url)

	first := postJSON(t, h, "/api/v1/auth/perform-reset", map[string]string{"token": token, "new_password": "brandnewpw"})
	if first.Code != http.StatusOK {
		t.Fatalf("first perform status = %d, want 200", first.Code)
	}
	second := postJSON(t, h, "/api/v1/auth/perform-reset", map[string]string{"token": token, "new_password": "anotherpw1"})
	if second.Code != http.StatusBadRequest {
		t.Fatalf("second perform status = %d, want 400 (single-use)", second.Code)
	}
	assertGenericTokenError(t, second)
}

func TestPerformResetGarbageToken(t *testing.T) {
	db := newTestDB(t)
	s := newTestServer(t, db, newFakeMailer(true))
	h := resetRouter(s)

	rr := postJSON(t, h, "/api/v1/auth/perform-reset", map[string]string{"token": "not-a-real-token", "new_password": "brandnewpw"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	assertGenericTokenError(t, rr)
}

func TestPerformResetShortPassword(t *testing.T) {
	db := newTestDB(t)
	mailer := newFakeMailer(true)
	s := newTestServer(t, db, mailer)
	h := resetRouter(s)
	seedUser(t, db, "real@example.com", "originalpw")

	postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	mailer.waitForSend(t)
	sent, _ := mailer.last()
	token := tokenFromLink(t, sent.url)

	rr := postJSON(t, h, "/api/v1/auth/perform-reset", map[string]string{"token": token, "new_password": "short"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var body map[string]string
	json.Unmarshal(rr.Body.Bytes(), &body)
	if body["error"] != "password too short" {
		t.Fatalf("error = %q, want 'password too short'", body["error"])
	}
}

func TestPerformResetSupersede(t *testing.T) {
	db := newTestDB(t)
	mailer := newFakeMailer(true)
	s := newTestServer(t, db, mailer)
	h := resetRouter(s)
	seedUser(t, db, "real@example.com", "originalpw")

	// First request → first token.
	postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	mailer.waitForSend(t)
	firstSent, _ := mailer.last()
	firstToken := tokenFromLink(t, firstSent.url)

	// Second request supersedes the first.
	postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	mailer.waitForSend(t)
	secondSent, _ := mailer.last()
	secondToken := tokenFromLink(t, secondSent.url)

	if firstToken == secondToken {
		t.Fatal("expected distinct tokens for two requests")
	}

	// First token is now invalid.
	old := postJSON(t, h, "/api/v1/auth/perform-reset", map[string]string{"token": firstToken, "new_password": "brandnewpw"})
	if old.Code != http.StatusBadRequest {
		t.Fatalf("superseded token status = %d, want 400", old.Code)
	}
	// Newest token still works.
	cur := postJSON(t, h, "/api/v1/auth/perform-reset", map[string]string{"token": secondToken, "new_password": "brandnewpw"})
	if cur.Code != http.StatusOK {
		t.Fatalf("newest token status = %d, want 200", cur.Code)
	}
}

func TestPerformResetInvalidatesRefreshSession(t *testing.T) {
	db := newTestDB(t)
	mailer := newFakeMailer(true)
	s := newTestServer(t, db, mailer)
	h := resetRouter(s)
	u := seedUser(t, db, "real@example.com", "originalpw")

	// Mint a refresh token as login would (token_version 0).
	pair, err := auth.GenerateTokenPair(prtSecret, u.ID, u.Username, u.Role, u.TokenVersion)
	if err != nil {
		t.Fatalf("token pair: %v", err)
	}

	// Refresh works before the reset.
	pre := postJSON(t, h, "/api/v1/auth/refresh", map[string]string{"refresh_token": pair.RefreshToken})
	if pre.Code != http.StatusOK {
		t.Fatalf("pre-reset refresh status = %d, want 200", pre.Code)
	}

	// Perform a reset → bumps token_version.
	postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	mailer.waitForSend(t)
	sent, _ := mailer.last()
	token := tokenFromLink(t, sent.url)
	rr := postJSON(t, h, "/api/v1/auth/perform-reset", map[string]string{"token": token, "new_password": "brandnewpw"})
	if rr.Code != http.StatusOK {
		t.Fatalf("perform-reset status = %d, want 200", rr.Code)
	}

	// The pre-reset refresh token is now rejected.
	post := postJSON(t, h, "/api/v1/auth/refresh", map[string]string{"refresh_token": pair.RefreshToken})
	if post.Code != http.StatusUnauthorized {
		t.Fatalf("post-reset refresh status = %d, want 401", post.Code)
	}

	// A fresh login (new token_version) refreshes fine.
	u2, _ := queries.GetUserByUsername(db, "real@example.com")
	fresh, _ := auth.GenerateTokenPair(prtSecret, u2.ID, u2.Username, u2.Role, u2.TokenVersion)
	ok := postJSON(t, h, "/api/v1/auth/refresh", map[string]string{"refresh_token": fresh.RefreshToken})
	if ok.Code != http.StatusOK {
		t.Fatalf("fresh refresh status = %d, want 200", ok.Code)
	}
}

func TestRequestResetPerUsernameRateLimit(t *testing.T) {
	db := newTestDB(t)
	mailer := newFakeMailer(true)
	s := newTestServer(t, db, mailer)
	h := resetRouter(s)
	seedUser(t, db, "real@example.com", "originalpw")

	// 3 allowed within the window, the 4th is throttled.
	for i := 0; i < 3; i++ {
		rr := postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i+1, rr.Code)
		}
	}
	rr := postJSON(t, h, "/api/v1/auth/request-reset", map[string]string{"username": "real@example.com"})
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("4th request status = %d, want 429", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "real@example.com") {
		t.Error("429 body should not reveal the username")
	}
}

func assertGenericTokenError(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "invalid or expired reset token" {
		t.Fatalf("error = %q, want generic 'invalid or expired reset token'", body["error"])
	}
}
