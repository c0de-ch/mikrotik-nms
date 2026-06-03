package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// securityHeaders sets conservative security headers on every response. These
// are safe on the JSON API surface (the SPA HTML is served by Next.js, which
// sets its own headers); HSTS is intentionally omitted because the backend
// listens on plain HTTP behind a TLS-terminating proxy.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// requestLogger is a slim replacement for chi's middleware.Logger that redacts
// the WebSocket auth token from the logged query string so it never lands in
// access logs.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		uri := r.URL.Path
		if r.URL.RawQuery != "" {
			uri += "?" + redactToken(r.URL.RawQuery)
		}
		log.Printf("%s %s %d %dB %s", r.Method, uri, ww.Status(), ww.BytesWritten(), time.Since(start).Round(time.Millisecond))
	})
}

// redactToken replaces the value of a "token" query parameter with REDACTED.
func redactToken(rawQuery string) string {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "[unparseable query]"
	}
	if values.Has("token") {
		values.Set("token", "REDACTED")
	}
	return values.Encode()
}

// rateLimiter is a small in-process per-key sliding-window limiter used to
// throttle unauthenticated auth endpoints against brute force. No external
// dependency; a janitor goroutine evicts idle keys.
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		hits:   make(map[string][]time.Time),
		limit:  limit,
		window: window,
	}
	go rl.janitor()
	return rl
}

func (rl *rateLimiter) allow(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := now.Add(-rl.window)
	kept := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, now)
	return true
}

func (rl *rateLimiter) janitor() {
	ticker := time.NewTicker(rl.window)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		rl.mu.Lock()
		for key, times := range rl.hits {
			fresh := false
			for _, t := range times {
				if t.After(now.Add(-rl.window)) {
					fresh = true
					break
				}
			}
			if !fresh {
				delete(rl.hits, key)
			}
		}
		rl.mu.Unlock()
	}
}

func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(r.RemoteAddr, time.Now()) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitResetPerUser throttles password-reset requests per submitted username so
// a single account can't be email-bombed (and the per-IP authLimiter still
// applies). It peeks at the JSON body — restoring it for the handler — and keys
// the limiter on the trimmed, lowercased username. An empty/missing username is
// NOT rate-limited here; the handler still returns the generic 200, preserving
// enumeration-safety (the limiter keys on the submitted string, never on whether
// the user exists). On breach it returns a generic 429 that reveals nothing.
func (s *Server) limitResetPerUser(rl *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		_ = r.Body.Close()
		if err != nil {
			b = nil
		}
		// Restore the body so the downstream handler can decode it.
		r.Body = io.NopCloser(bytes.NewReader(b))

		var peek struct {
			Username string `json:"username"`
			Email    string `json:"email"`
		}
		_ = json.Unmarshal(b, &peek)

		username := peek.Username
		if username == "" {
			username = peek.Email
		}
		username = strings.ToLower(strings.TrimSpace(username))

		if username != "" && !rl.allow("reset:"+username, time.Now()) {
			w.Header().Set("Retry-After", "900")
			http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	}
}
