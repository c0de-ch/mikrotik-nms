package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type contextKey string

const UserContextKey contextKey = "user"

type ContextUser struct {
	ID       string
	Username string
	Role     string
	// AuthTime is when the session's user last logged in with the password
	// (zero when unknown). RequireFreshAuth judges by it.
	AuthTime time.Time
}

// UserLookup reports a user's current token version and role; ok is false
// when the user no longer exists (or cannot be read: fail closed).
type UserLookup func(userID string) (tokenVersion int, role string, ok bool)

// RequireAuth middleware validates the JWT access token and injects the user
// into context. The token must still match the user's current token version
// — a logout or a password reset bumps it, which ends every session issued
// before — and the role is the user's current one, not the one the token
// was issued with.
func RequireAuth(secret string, users UserLookup) func(http.Handler) http.Handler {
	if users == nil {
		panic("auth.RequireAuth: a UserLookup is required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenStr := extractToken(r)
			if tokenStr == "" {
				http.Error(w, `{"error":"missing authorization token"}`, http.StatusUnauthorized)
				return
			}

			claims, err := ValidateAccessToken(secret, tokenStr)
			if err != nil {
				http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}
			version, role, ok := users(claims.UserID)
			if !ok || version != claims.TokenVersion {
				http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}

			user := &ContextUser{
				ID:       claims.UserID,
				Username: claims.Username,
				Role:     role,
			}
			if claims.AuthTime > 0 {
				user.AuthTime = time.Unix(claims.AuthTime, 0)
			}
			ctx := context.WithValue(r.Context(), UserContextKey, user)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole middleware checks that the authenticated user has the required role.
func RequireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := UserFromContext(r.Context())
			if user == nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if user.Role != role {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CodeReauthRequired is the "code" of the 403 RequireFreshAuth answers with.
const CodeReauthRequired = "reauth_required"

// RequireFreshAuth lets a request through only when the session's user
// typed the password at most maxAge ago. A silent token refresh does not
// count: it carries the login time over. It answers 403 with code
// reauth_required (never 401, which clients treat as "refresh the token"),
// so the UI can ask for the password and retry.
func RequireFreshAuth(maxAge time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user := UserFromContext(r.Context())
			if user == nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			if age := time.Since(user.AuthTime); user.AuthTime.IsZero() || age > maxAge || age < -time.Minute {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":           "this action needs a fresh login: enter your password again",
					"code":            CodeReauthRequired,
					"max_age_seconds": int(maxAge / time.Second),
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func UserFromContext(ctx context.Context) *ContextUser {
	u, _ := ctx.Value(UserContextKey).(*ContextUser)
	return u
}

func extractToken(r *http.Request) string {
	// Authorization: Bearer <token>
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	// Query parameter: only for the WebSocket upgrade, where a browser cannot
	// set a header. On any other request a token in the URL would end up in
	// proxy logs, browser history and Referer headers.
	if isWebSocketUpgrade(r) {
		return r.URL.Query().Get("token")
	}
	return ""
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, v := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "upgrade") {
			return true
		}
	}
	return false
}
