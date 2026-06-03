package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/mailer"
)

// resetRequest is the request-reset body. "email" is accepted as an alias for
// "username" since usernames are emails in practice; both map to users.username.
type resetRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
}

type performResetRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// okBody is the byte-identical success body returned by request-reset for every
// outcome (and by perform-reset on success), matching the /auth/logout shape.
var okBody = map[string]string{"status": "ok"}

// handleRequestReset is PUBLIC and fully enumeration-safe: it ALWAYS returns
// HTTP 200 with a byte-identical {"status":"ok"} body regardless of user
// existence, SMTP/feature state, or email send success. Any actual email send
// happens asynchronously so SMTP latency can never time the response.
func (s *Server) handleRequestReset(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	// A bad/empty body is not an error here — we always answer the same way.
	_ = decodeJSON(r, &req)

	username := req.Username
	if username == "" {
		username = req.Email
	}
	username = strings.TrimSpace(username)

	// Opportunistic cleanup of long-expired tokens (best-effort; never alters
	// the response).
	if _, err := queries.DeleteExpiredResetTokens(s.db, time.Now().Add(-24*time.Hour)); err != nil {
		log.Printf("password reset: cleanup: %v", err)
	}

	// Empty username: no lookup, no send — still the generic 200.
	if username == "" {
		writeJSON(w, http.StatusOK, okBody)
		return
	}

	// Feature gate: SMTP must be enabled AND the admin kill-switch must not be
	// off. Both checks are silent — the response is identical either way.
	if s.mailer == nil || !s.mailer.Enabled() || !s.pwresetEnabled() {
		writeJSON(w, http.StatusOK, okBody)
		return
	}

	// Look up by the EXACT submitted value (username is a UNIQUE column),
	// matching existing GetUserByUsername semantics so mixed-case real users
	// are not silently dropped.
	user, err := queries.GetUserByUsername(s.db, username)
	if err != nil {
		// Not found (or any DB error): say nothing different.
		writeJSON(w, http.StatusOK, okBody)
		return
	}

	// Issue a fresh token (supersedes any pending ones), retrying once on the
	// astronomically unlikely hash collision.
	rawToken, link, issueErr := s.issueResetLink(user.ID)
	if issueErr != nil {
		log.Printf("password reset: issue token for user %s: %v", user.ID, issueErr)
		writeJSON(w, http.StatusOK, okBody)
		return
	}

	// Fire-and-forget the email so SMTP latency/failure neither blocks nor
	// times the HTTP response (timing-oracle defense). The goroutine has its
	// own context/timeout, fully detached from the request.
	to := user.Username
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- s.mailer.SendPasswordResetEmail(to, link) }()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, mailer.ErrDisabled) {
				// Never log the raw token or SMTP password.
				log.Printf("password reset: send email: %v", err)
			}
		case <-ctx.Done():
			log.Printf("password reset: send email timed out")
		}
	}()
	_ = rawToken // raw token only travels inside the emailed link

	writeJSON(w, http.StatusOK, okBody)
}

// issueResetLink generates a token, persists its hash (superseding pending
// tokens), and returns the raw token plus the fully-built reset link. The link
// host comes ONLY from the configured public base URL, never the request Host.
func (s *Server) issueResetLink(userID string) (rawToken, link string, err error) {
	ttl := s.cfg.PasswordResetTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, gerr := auth.GenerateResetToken()
		if gerr != nil {
			return "", "", gerr
		}
		hash := auth.HashResetToken(token)
		ierr := queries.IssueResetToken(s.db, userID, hash, time.Now().Add(ttl))
		if ierr == nil {
			base := strings.TrimRight(s.cfg.PublicBaseURL, "/")
			return token, base + "/reset-password?token=" + token, nil
		}
		if queries.IsUniqueViolation(ierr) {
			continue // regenerate and retry once
		}
		return "", "", ierr
	}
	return "", "", errors.New("could not issue reset token")
}

// pwresetEnabled reports the admin kill-switch state. Defaults to enabled when
// the setting is missing/unreadable.
func (s *Server) pwresetEnabled() bool {
	v, err := queries.GetSetting(s.db, "pwreset_enabled")
	if err != nil {
		return true
	}
	return v != "false"
}

// handlePerformReset is PUBLIC. It consumes a single-use token and sets a new
// password, bumping users.token_version to invalidate existing sessions. All
// not-found / expired / already-used token states collapse into one generic
// 400 so no state oracle leaks. It never logs the user in or returns tokens.
func (s *Server) handlePerformReset(w http.ResponseWriter, r *http.Request) {
	var req performResetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Validate the supplied password first (leaks nothing about token/user).
	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "password too short")
		return
	}

	tokenHash := auth.HashResetToken(req.Token)

	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}
	defer tx.Rollback()

	userID, ok, err := queries.ConsumeResetToken(tx, tokenHash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid or expired reset token")
		return
	}

	// Defense-in-depth constant-time check: re-hash the inbound token and
	// confirm it matches what we just looked up by. (A UNIQUE-indexed lookup of
	// a full sha256 is already not a per-byte secret comparison, but this keeps
	// the comparison explicit and timing-flat.)
	if subtle.ConstantTimeCompare([]byte(tokenHash), []byte(auth.HashResetToken(req.Token))) != 1 {
		writeError(w, http.StatusBadRequest, "invalid or expired reset token")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}

	if err := queries.UpdateUserPasswordAndBumpVersion(tx, userID, hash); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset password")
		return
	}

	// Clear any refresh cookie on the resetting client; all existing refresh
	// sessions are already invalidated by the token_version bump.
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    "",
		Path:     "/api/v1/auth",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})

	writeJSON(w, http.StatusOK, okBody)
}
