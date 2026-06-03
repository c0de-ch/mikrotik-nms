package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	AccessTokenDuration  = 15 * time.Minute
	RefreshTokenDuration = 7 * 24 * time.Hour
)

type Claims struct {
	UserID       string `json:"uid"`
	Username     string `json:"usr"`
	Role         string `json:"role"`
	TokenVersion int    `json:"tv"`
	jwt.RegisteredClaims
}

// refreshClaims carries the user id and the session-invalidation token version
// so /auth/refresh can reject sessions superseded by a password reset.
type refreshClaims struct {
	UserID       string `json:"uid"`
	TokenVersion int    `json:"tv"`
	jwt.RegisteredClaims
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
}

func GenerateTokenPair(secret, userID, username, role string, tokenVersion int) (*TokenPair, error) {
	now := time.Now()

	// Access token
	accessClaims := &Claims{
		UserID:       userID,
		Username:     username,
		Role:         role,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenDuration)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   userID,
		},
	}
	accessToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims).SignedString([]byte(secret))
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	// Refresh token (minimal claims + token version)
	refreshClaims := &refreshClaims{
		UserID:       userID,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(RefreshTokenDuration)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   userID,
		},
	}
	refreshToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, refreshClaims).SignedString([]byte(secret))
	if err != nil {
		return nil, fmt.Errorf("sign refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    accessClaims.ExpiresAt.Unix(),
	}, nil
}

func ValidateAccessToken(secret, tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	return claims, nil
}

// ValidateRefreshToken parses a refresh token and returns the user id and the
// token version embedded at issue time. The caller compares the returned
// version against the user's current token_version to enforce reset-driven
// session invalidation. Pre-existing tokens without a "tv" claim decode to 0,
// which matches the migration default.
func ValidateRefreshToken(secret, tokenStr string) (string, int, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &refreshClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return "", 0, err
	}

	claims, ok := token.Claims.(*refreshClaims)
	if !ok || !token.Valid {
		return "", 0, fmt.Errorf("invalid refresh token")
	}
	// Subject is the canonical user id; uid mirrors it for forward compat.
	userID := claims.Subject
	if userID == "" {
		userID = claims.UserID
	}
	return userID, claims.TokenVersion, nil
}
