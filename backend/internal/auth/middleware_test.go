package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const mwSecret = "a-sufficiently-long-test-secret-value"

func TestRequireAuthValidToken(t *testing.T) {
	tokens, err := GenerateTokenPair(mwSecret, "uid-1", "alice", "admin", 0)
	if err != nil {
		t.Fatalf("token pair: %v", err)
	}

	var gotUser *ContextUser
	h := RequireAuth(mwSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if gotUser == nil || gotUser.ID != "uid-1" || gotUser.Role != "admin" {
		t.Fatalf("context user not injected correctly: %+v", gotUser)
	}
}

func TestRequireAuthRejectsMissingAndBadTokens(t *testing.T) {
	h := RequireAuth(mwSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := map[string]string{
		"missing": "",
		"garbage": "Bearer not-a-jwt",
		"wrongsecret": func() string {
			toks, _ := GenerateTokenPair("some-other-secret", "u", "u", "viewer", 0)
			return "Bearer " + toks.AccessToken
		}(),
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
		})
	}
}

func TestRequireRole(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// Viewer hitting an admin-gated handler → 403.
	viewer, _ := GenerateTokenPair(mwSecret, "v", "viewer", "viewer", 0)
	chain := RequireAuth(mwSecret)(RequireRole("admin")(ok))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+viewer.AccessToken)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("viewer on admin route: status = %d, want 403", rr.Code)
	}

	// Admin passes.
	admin, _ := GenerateTokenPair(mwSecret, "a", "admin", "admin", 0)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer "+admin.AccessToken)
	rr2 := httptest.NewRecorder()
	chain.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("admin on admin route: status = %d, want 200", rr2.Code)
	}
}
