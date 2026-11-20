package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const mwSecret = "a-sufficiently-long-test-secret-value"

// users is a UserLookup over a fixed table: id → {token version, role}.
type users map[string]struct {
	version int
	role    string
}

func (u users) lookup(id string) (int, string, bool) {
	row, ok := u[id]
	return row.version, row.role, ok
}

func bearer(t *testing.T, userID, role string, version int, authTime time.Time) string {
	t.Helper()
	tokens, err := GenerateTokenPair(mwSecret, userID, userID, role, version, authTime)
	if err != nil {
		t.Fatalf("token pair: %v", err)
	}
	return "Bearer " + tokens.AccessToken
}

func serve(h http.Handler, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestRequireAuthValidToken(t *testing.T) {
	login := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	var gotUser *ContextUser
	table := users{"uid-1": {0, "admin"}}
	h := RequireAuth(mwSecret, table.lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	rr := serve(h, bearer(t, "uid-1", "admin", 0, login))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if gotUser == nil || gotUser.ID != "uid-1" || gotUser.Role != "admin" || !gotUser.AuthTime.Equal(login) {
		t.Fatalf("context user not injected correctly: %+v", gotUser)
	}
}

func TestRequireAuthRejectsMissingAndBadTokens(t *testing.T) {
	table := users{"u": {0, "viewer"}}
	h := RequireAuth(mwSecret, table.lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	refresh, err := GenerateTokenPair(mwSecret, "u", "u", "viewer", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing": "",
		"garbage": "Bearer not-a-jwt",
		"wrongsecret": func() string {
			toks, _ := GenerateTokenPair("some-other-secret", "u", "u", "viewer", 0, time.Now())
			return "Bearer " + toks.AccessToken
		}(),
		"a refresh token": "Bearer " + refresh.RefreshToken,
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			if rr := serve(h, header); rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
		})
	}
}

// A token stops working the moment the user's token version moves (logout,
// password reset) or the user is deleted — not when it expires.
func TestRequireAuthEndsSessionsOnTokenVersionChange(t *testing.T) {
	table := users{"u": {3, "admin"}}
	h := RequireAuth(mwSecret, table.lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	token := bearer(t, "u", "admin", 3, time.Now())

	if rr := serve(h, token); rr.Code != http.StatusOK {
		t.Fatalf("current token version: status = %d, want 200", rr.Code)
	}
	table["u"] = struct {
		version int
		role    string
	}{4, "admin"}
	if rr := serve(h, token); rr.Code != http.StatusUnauthorized {
		t.Fatalf("after the version moved: status = %d, want 401", rr.Code)
	}
	if rr := serve(h, bearer(t, "u", "admin", 4, time.Now())); rr.Code != http.StatusOK {
		t.Fatalf("a token of the new version: status = %d, want 200", rr.Code)
	}
	delete(table, "u")
	if rr := serve(h, bearer(t, "u", "admin", 4, time.Now())); rr.Code != http.StatusUnauthorized {
		t.Fatalf("deleted user: status = %d, want 401", rr.Code)
	}
}

// The role is the user's current one, whatever the token was issued with.
func TestRequireAuthUsesTheCurrentRole(t *testing.T) {
	table := users{"u": {0, "viewer"}}
	chain := RequireAuth(mwSecret, table.lookup)(RequireRole("admin")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	// Issued as admin, demoted since.
	if rr := serve(chain, bearer(t, "u", "admin", 0, time.Now())); rr.Code != http.StatusForbidden {
		t.Fatalf("demoted user on an admin route: status = %d, want 403", rr.Code)
	}
}

func TestRequireRole(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	table := users{"v": {0, "viewer"}, "a": {0, "admin"}}
	chain := RequireAuth(mwSecret, table.lookup)(RequireRole("admin")(ok))

	// Viewer hitting an admin-gated handler → 403.
	if rr := serve(chain, bearer(t, "v", "viewer", 0, time.Now())); rr.Code != http.StatusForbidden {
		t.Fatalf("viewer on admin route: status = %d, want 403", rr.Code)
	}
	// Admin passes.
	if rr := serve(chain, bearer(t, "a", "admin", 0, time.Now())); rr.Code != http.StatusOK {
		t.Fatalf("admin on admin route: status = %d, want 200", rr.Code)
	}
}

// A side-effecting action needs a password login at most maxAge old; a
// session kept alive by refreshes is told to log in again — with 403 and a
// code, never 401 (which clients answer with a silent refresh).
func TestRequireFreshAuth(t *testing.T) {
	table := users{"a": {0, "admin"}}
	chain := RequireAuth(mwSecret, table.lookup)(RequireFreshAuth(5 * time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	if rr := serve(chain, bearer(t, "a", "admin", 0, time.Now().Add(-4*time.Minute))); rr.Code != http.StatusOK {
		t.Fatalf("login 4 min ago: status = %d, want 200", rr.Code)
	}
	for name, authTime := range map[string]time.Time{
		"login 6 min ago":     time.Now().Add(-6 * time.Minute),
		"no login time":       time.Unix(0, 0),
		"login in the future": time.Now().Add(10 * time.Minute),
	} {
		rr := serve(chain, bearer(t, "a", "admin", 0, authTime))
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", name, rr.Code)
		}
		var body struct {
			Error  string `json:"error"`
			Code   string `json:"code"`
			MaxAge int    `json:"max_age_seconds"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body %q: %v", name, rr.Body.String(), err)
		}
		if body.Code != CodeReauthRequired || body.Error == "" || body.MaxAge != 300 {
			t.Fatalf("%s: body = %+v, want code %q and max_age_seconds 300", name, body, CodeReauthRequired)
		}
	}
}

// A token in the query string is only read on a WebSocket upgrade (where a
// browser cannot set a header): on any other request it would end up in
// logs, history and Referer headers.
func TestQueryTokenOnlyOnWebSocketUpgrade(t *testing.T) {
	table := users{"u": {0, "viewer"}}
	h := RequireAuth(mwSecret, table.lookup)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	tokens, err := GenerateTokenPair(mwSecret, "u", "u", "viewer", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	plain := httptest.NewRequest(http.MethodGet, "/devices?token="+tokens.AccessToken, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, plain)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("?token= on a plain request: status = %d, want 401", rr.Code)
	}

	upgrade := httptest.NewRequest(http.MethodGet, "/ws?token="+tokens.AccessToken, nil)
	upgrade.Header.Set("Connection", "keep-alive, Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, upgrade)
	if rr.Code != http.StatusOK {
		t.Fatalf("?token= on a WebSocket upgrade: status = %d, want 200", rr.Code)
	}
}

func TestRequireAuthNeedsALookup(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("RequireAuth(secret, nil) must panic: without a lookup nothing is ever revoked")
		}
	}()
	RequireAuth(mwSecret, nil)
}
