package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

const sessSecret = "session-test-secret-session-test-secret"

type sessionFixture struct {
	t  *testing.T
	db *sql.DB
	h  http.Handler
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	db := newTestDB(t)
	seedUser(t, db, "admin", "correct horse battery")
	if _, err := db.Exec(`UPDATE users SET role='admin' WHERE username='admin'`); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{JWTSecret: sessSecret, AllowedOrigins: []string{"http://x"}}
	return &sessionFixture{t: t, db: db, h: NewRouter(db, nil, cfg, nil, nil, nil, nil, nil, nil)}
}

func (f *sessionFixture) do(method, path, bearer string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, "/api/v1"+path, &buf)
	req.RemoteAddr = "203.0.113.7:40000"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *sessionFixture) login() auth.TokenPair {
	f.t.Helper()
	rec := f.do(http.MethodPost, "/auth/login", "", map[string]string{"username": "admin", "password": "correct horse battery"})
	if rec.Code != http.StatusOK {
		f.t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	var pair auth.TokenPair
	if err := json.Unmarshal(rec.Body.Bytes(), &pair); err != nil {
		f.t.Fatal(err)
	}
	return pair
}

// Logging out ends the session on the server: the access token and the
// refresh token of that login stop working at once, on every device.
func TestLogoutRevokesTheSessionServerSide(t *testing.T) {
	f := newSessionFixture(t)
	first, other := f.login(), f.login() // two browsers

	if rec := f.do(http.MethodGet, "/auth/me", first.AccessToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("me before logout = %d", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/auth/logout", first.AccessToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("logout = %d %s", rec.Code, rec.Body.String())
	}
	for name, pair := range map[string]auth.TokenPair{"the browser that logged out": first, "the other browser": other} {
		if rec := f.do(http.MethodGet, "/auth/me", pair.AccessToken, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: access token after logout = %d, want 401", name, rec.Code)
		}
		if rec := f.do(http.MethodPost, "/auth/refresh", "", map[string]string{"refresh_token": pair.RefreshToken}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: refresh after logout = %d, want 401", name, rec.Code)
		}
	}
	// Logging in again works.
	if rec := f.do(http.MethodGet, "/auth/me", f.login().AccessToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("me after a new login = %d", rec.Code)
	}
}

// A refresh keeps the session alive but is not a login: auth_time stays
// where the password put it. An access token is not accepted as a refresh
// token, and a refresh token not as an access token.
func TestRefreshKeepsTheLoginTime(t *testing.T) {
	f := newSessionFixture(t)
	pair := f.login()
	if pair.AuthTime == 0 || time.Since(time.Unix(pair.AuthTime, 0)) > time.Minute {
		t.Fatalf("login auth_time = %d, want now", pair.AuthTime)
	}

	// A session whose password login was an hour ago.
	u, err := queries.GetUserByUsername(f.db, "admin")
	if err != nil {
		t.Fatal(err)
	}
	hourAgo := time.Now().Add(-time.Hour)
	old, err := auth.GenerateTokenPair(sessSecret, u.ID, u.Username, u.Role, u.TokenVersion, hourAgo)
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(http.MethodPost, "/auth/refresh", "", map[string]string{"refresh_token": old.RefreshToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
	var refreshed auth.TokenPair
	if err := json.Unmarshal(rec.Body.Bytes(), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.AuthTime != hourAgo.Unix() {
		t.Fatalf("auth_time after a refresh = %d, want the original login %d", refreshed.AuthTime, hourAgo.Unix())
	}

	if rec := f.do(http.MethodPost, "/auth/refresh", "", map[string]string{"refresh_token": pair.AccessToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an access token used as a refresh token = %d, want 401", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/auth/me", pair.RefreshToken, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a refresh token used as an access token = %d, want 401", rec.Code)
	}
}

// Firmware actions change a device: they need the password typed in the last
// five minutes. A session an hour old gets 403 reauth_required before the
// handler runs; reads and the read-only "check" are not gated.
func TestFirmwareActionsNeedAFreshLogin(t *testing.T) {
	f := newSessionFixture(t)
	u, err := queries.GetUserByUsername(f.db, "admin")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := auth.GenerateTokenPair(sessSecret, u.ID, u.Username, u.Role, u.TokenVersion, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	fresh := f.login()

	for _, path := range []string{"/firmware/upgrade", "/firmware/channel", "/firmware/routerboard"} {
		rec := f.do(http.MethodPost, path, stale.AccessToken, map[string]any{})
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusForbidden || body.Code != auth.CodeReauthRequired {
			t.Errorf("POST %s with an hour-old login = %d %s, want 403 reauth_required", path, rec.Code, rec.Body.String())
		}
		// A fresh login gets past the gate, to the handler's own validation.
		rec = f.do(http.MethodPost, path, fresh.AccessToken, map[string]any{})
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("POST %s right after a login = %d %s, want the handler's answer", path, rec.Code, rec.Body.String())
		}
	}
	if rec := f.do(http.MethodGet, "/firmware", stale.AccessToken, nil); rec.Code != http.StatusOK {
		t.Errorf("GET /firmware with an hour-old login = %d, want 200", rec.Code)
	}
}

func TestClientIP(t *testing.T) {
	loopback := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	ingress := append([]netip.Prefix{netip.MustParsePrefix("10.42.0.0/16")}, loopback...)
	cases := []struct {
		name    string
		remote  string
		xff     []string
		realIP  string
		trusted []netip.Prefix
		want    string
	}{
		{"direct client", "203.0.113.7:5000", nil, "", loopback, "203.0.113.7"},
		{"direct client forging headers", "203.0.113.7:5000", []string{"198.51.100.1"}, "198.51.100.2", loopback, "203.0.113.7"},
		{"behind the local proxy", "127.0.0.1:5000", []string{"203.0.113.7"}, "", loopback, "203.0.113.7"},
		{"client prepends a forged entry", "127.0.0.1:5000", []string{"198.51.100.1, 203.0.113.7"}, "", loopback, "203.0.113.7"},
		{"forged entry in a header of its own", "127.0.0.1:5000", []string{"198.51.100.1", "203.0.113.7"}, "", loopback, "203.0.113.7"},
		{"two trusted hops", "127.0.0.1:5000", []string{"203.0.113.7, 10.42.3.9"}, "", ingress, "203.0.113.7"},
		{"X-Real-IP is never believed", "127.0.0.1:5000", nil, "198.51.100.2", loopback, "127.0.0.1"},
		{"proxy sent nothing", "127.0.0.1:5000", nil, "", loopback, "127.0.0.1"},
		{"garbage to the right", "127.0.0.1:5000", []string{"203.0.113.7, not-an-ip"}, "", loopback, "127.0.0.1"},
		{"ipv6 loopback proxy", "[::1]:5000", []string{"2001:db8::7"}, "", loopback, "2001:db8::7"},
		{"v4-mapped peer", "[::ffff:127.0.0.1]:5000", []string{"203.0.113.7"}, "", loopback, "203.0.113.7"},
		{"untrusted proxy address", "10.42.3.9:5000", []string{"203.0.113.7"}, "", loopback, "10.42.3.9"},
		{"no trusted proxies at all", "127.0.0.1:5000", []string{"203.0.113.7"}, "", nil, "127.0.0.1"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = tc.remote
		for _, v := range tc.xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		if tc.realIP != "" {
			req.Header.Set("X-Real-IP", tc.realIP)
		}
		if got := clientIP(req, tc.trusted); got != tc.want {
			t.Errorf("%s: clientIP = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The login limiter counts per real client: forged forwarding headers do not
// buy a fresh budget.
func TestLoginLimiterIgnoresForgedForwardingHeaders(t *testing.T) {
	f := newSessionFixture(t)
	last := 0
	for i := 0; i < 12; i++ {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(map[string]string{"username": "admin", "password": "wrong"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", &buf)
		req.RemoteAddr = "203.0.113.99:40000"
		req.Header.Set("X-Forwarded-For", netip.AddrFrom4([4]byte{198, 51, 100, byte(i)}).String())
		req.Header.Set("X-Real-IP", netip.AddrFrom4([4]byte{198, 51, 100, byte(100 + i)}).String())
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("12th failed login from one address = %d, want 429 whatever headers it sends", last)
	}
}
