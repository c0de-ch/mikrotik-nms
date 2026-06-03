package api

import (
	"database/sql"
	"net/http"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, err := queries.GetUserByUsername(s.db, req.Username)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	if !auth.CheckPassword(user.PasswordHash, req.Password) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	tokens, err := auth.GenerateTokenPair(s.cfg.JWTSecret, user.ID, user.Username, user.Role, user.TokenVersion)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate tokens")
		return
	}

	// Set refresh token as httpOnly cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    tokens.RefreshToken,
		Path:     "/api/v1/auth",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   7 * 24 * 60 * 60, // 7 days
	})

	writeJSON(w, http.StatusOK, tokens)
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password required")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	user := &queries.User{
		ID:           uuid.NewString(),
		Username:     req.Username,
		PasswordHash: hash,
		Role:         "admin",
	}

	// Atomic guard: only creates the admin if the users table is still empty,
	// so two concurrent setup requests can't both succeed.
	created, err := queries.CreateFirstUser(s.db, user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}
	if !created {
		writeError(w, http.StatusConflict, "setup already completed")
		return
	}

	// A freshly-created user always starts at token_version 0.
	tokens, err := auth.GenerateTokenPair(s.cfg.JWTSecret, user.ID, user.Username, user.Role, user.TokenVersion)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate tokens")
		return
	}

	writeJSON(w, http.StatusCreated, tokens)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	// Get refresh token from cookie or body
	var refreshToken string
	if cookie, err := r.Cookie("refresh_token"); err == nil {
		refreshToken = cookie.Value
	}
	if refreshToken == "" {
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := decodeJSON(r, &req); err == nil {
			refreshToken = req.RefreshToken
		}
	}
	if refreshToken == "" {
		writeError(w, http.StatusBadRequest, "refresh token required")
		return
	}

	userID, tokenVersion, err := auth.ValidateRefreshToken(s.cfg.JWTSecret, refreshToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}

	user, err := queries.GetUserByID(s.db, userID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "user not found")
		return
	}

	// Session invalidation: a password reset bumps users.token_version, so a
	// refresh token minted before the reset no longer matches and is rejected.
	if tokenVersion != user.TokenVersion {
		writeError(w, http.StatusUnauthorized, "invalid refresh token")
		return
	}

	tokens, err := auth.GenerateTokenPair(s.cfg.JWTSecret, user.ID, user.Username, user.Role, user.TokenVersion)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate tokens")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    tokens.RefreshToken,
		Path:     "/api/v1/auth",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   7 * 24 * 60 * 60,
	})

	writeJSON(w, http.StatusOK, tokens)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "refresh_token",
		Value:    "",
		Path:     "/api/v1/auth",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":       user.ID,
		"username": user.Username,
		"role":     user.Role,
	})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := queries.ListUsers(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password required")
		return
	}
	if req.Role != "admin" && req.Role != "viewer" {
		writeError(w, http.StatusBadRequest, "role must be 'admin' or 'viewer'")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	user := &queries.User{
		ID:           uuid.NewString(),
		Username:     req.Username,
		PasswordHash: hash,
		Role:         req.Role,
	}

	if err := queries.CreateUser(s.db, user); err != nil {
		writeError(w, http.StatusConflict, "username already exists")
		return
	}

	writeJSON(w, http.StatusCreated, user)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Prevent self-deletion
	currentUser := auth.UserFromContext(r.Context())
	if currentUser != nil && currentUser.ID == id {
		writeError(w, http.StatusBadRequest, "cannot delete yourself")
		return
	}

	if err := queries.DeleteUser(s.db, id); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
