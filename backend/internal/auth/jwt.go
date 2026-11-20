package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	AccessTokenDuration  = 15 * time.Minute
	RefreshTokenDuration = 7 * 24 * time.Hour

	// Audience is the "aud" every token is issued for and checked against.
	Audience = "mikrotik-nms"

	// Token types ("typ"). An access token is refused where a refresh token
	// is expected and the other way round — and each kind is signed with a
	// key of its own (tokenKey), so one can never verify as the other.
	typAccess  = "access"
	typRefresh = "refresh"
)

type Claims struct {
	UserID       string `json:"uid"`
	Username     string `json:"usr"`
	Role         string `json:"role"`
	TokenVersion int    `json:"tv"`
	Typ          string `json:"typ"`
	// AuthTime is when the user last proved the password (unix seconds). A
	// refresh carries it over unchanged, so it only moves on a real login.
	AuthTime int64 `json:"auth_time"`
	jwt.RegisteredClaims
}

// refreshClaims carries the user id and the session-invalidation token version
// so /auth/refresh can reject sessions superseded by a logout or a password
// reset, and the login time the refreshed access token inherits.
type refreshClaims struct {
	UserID       string `json:"uid"`
	TokenVersion int    `json:"tv"`
	Typ          string `json:"typ"`
	AuthTime     int64  `json:"auth_time"`
	jwt.RegisteredClaims
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	// AuthTime is the password login the pair descends from (unix seconds).
	AuthTime int64 `json:"auth_time"`
}

// RefreshSession is what a valid refresh token says about its session.
type RefreshSession struct {
	UserID       string
	TokenVersion int
	AuthTime     time.Time
}

// tokenKey derives the signing key of one token type from the configured
// secret, so access and refresh tokens never share a key.
func tokenKey(secret, typ string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("mikrotik-nms/jwt/" + typ + "/v1"))
	return mac.Sum(nil)
}

// GenerateTokenPair issues an access and a refresh token. authTime is the
// password login they descend from: now for a login, the refresh token's own
// for a refresh.
func GenerateTokenPair(secret, userID, username, role string, tokenVersion int, authTime time.Time) (*TokenPair, error) {
	now := time.Now()
	registered := func(ttl time.Duration) jwt.RegisteredClaims {
		return jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   userID,
			Audience:  jwt.ClaimStrings{Audience},
		}
	}

	accessClaims := &Claims{
		UserID:           userID,
		Username:         username,
		Role:             role,
		TokenVersion:     tokenVersion,
		Typ:              typAccess,
		AuthTime:         authTime.Unix(),
		RegisteredClaims: registered(AccessTokenDuration),
	}
	accessToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims).SignedString(tokenKey(secret, typAccess))
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}

	// Refresh token (minimal claims + token version + login time)
	refresh := &refreshClaims{
		UserID:           userID,
		TokenVersion:     tokenVersion,
		Typ:              typRefresh,
		AuthTime:         authTime.Unix(),
		RegisteredClaims: registered(RefreshTokenDuration),
	}
	refreshToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, refresh).SignedString(tokenKey(secret, typRefresh))
	if err != nil {
		return nil, fmt.Errorf("sign refresh token: %w", err)
	}

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    accessClaims.ExpiresAt.Unix(),
		AuthTime:     accessClaims.AuthTime,
	}, nil
}

// parse verifies tokenStr as a token of typ: HS256 under that type's key,
// unexpired, issued for Audience.
func parse(secret, typ, tokenStr string, claims jwt.Claims) error {
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		return tokenKey(secret, typ), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithAudience(Audience), jwt.WithExpirationRequired())
	if err != nil {
		return err
	}
	if !token.Valid {
		return fmt.Errorf("invalid token")
	}
	return nil
}

func ValidateAccessToken(secret, tokenStr string) (*Claims, error) {
	claims := &Claims{}
	if err := parse(secret, typAccess, tokenStr, claims); err != nil {
		return nil, err
	}
	if claims.Typ != typAccess {
		return nil, fmt.Errorf("not an access token")
	}
	return claims, nil
}

// ValidateRefreshToken parses a refresh token and returns its session: the
// user id, the token version embedded at issue time and the login time. The
// caller compares the version against the user's current token_version to
// enforce logout- and reset-driven session invalidation.
func ValidateRefreshToken(secret, tokenStr string) (RefreshSession, error) {
	claims := &refreshClaims{}
	if err := parse(secret, typRefresh, tokenStr, claims); err != nil {
		return RefreshSession{}, err
	}
	if claims.Typ != typRefresh {
		return RefreshSession{}, fmt.Errorf("not a refresh token")
	}
	// Subject is the canonical user id; uid mirrors it for forward compat.
	userID := claims.Subject
	if userID == "" {
		userID = claims.UserID
	}
	if userID == "" {
		return RefreshSession{}, fmt.Errorf("refresh token names no user")
	}
	return RefreshSession{UserID: userID, TokenVersion: claims.TokenVersion, AuthTime: time.Unix(claims.AuthTime, 0)}, nil
}
