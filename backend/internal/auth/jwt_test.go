package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-key-for-unit-tests"

func mustPair(t *testing.T, secret, userID, role string, version int, authTime time.Time) *TokenPair {
	t.Helper()
	pair, err := GenerateTokenPair(secret, userID, role, role, version, authTime)
	if err != nil {
		t.Fatalf("GenerateTokenPair returned error: %v", err)
	}
	return pair
}

func TestGenerateTokenPair(t *testing.T) {
	login := time.Now().Add(-3 * time.Minute)
	pair := mustPair(t, testSecret, "user-123", "admin", 0, login)
	if pair.AccessToken == "" {
		t.Error("AccessToken is empty")
	}
	if pair.RefreshToken == "" {
		t.Error("RefreshToken is empty")
	}
	// ExpiresAt should be in the future
	if pair.ExpiresAt <= time.Now().Unix() {
		t.Error("ExpiresAt should be in the future")
	}
	if pair.AuthTime != login.Unix() {
		t.Errorf("AuthTime = %d, want the login time %d", pair.AuthTime, login.Unix())
	}
}

func TestValidateAccessToken_Valid(t *testing.T) {
	login := time.Now().Add(-time.Hour)
	pair := mustPair(t, testSecret, "user-123", "admin", 0, login)

	claims, err := ValidateAccessToken(testSecret, pair.AccessToken)
	if err != nil {
		t.Fatalf("ValidateAccessToken returned error: %v", err)
	}
	if claims.UserID != "user-123" {
		t.Errorf("UserID = %q, want %q", claims.UserID, "user-123")
	}
	if claims.Username != "admin" {
		t.Errorf("Username = %q, want %q", claims.Username, "admin")
	}
	if claims.Role != "admin" {
		t.Errorf("Role = %q, want %q", claims.Role, "admin")
	}
	if claims.Subject != "user-123" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "user-123")
	}
	if claims.Typ != "access" || len(claims.Audience) != 1 || claims.Audience[0] != Audience {
		t.Errorf("typ = %q aud = %v, want access / %s", claims.Typ, claims.Audience, Audience)
	}
	if claims.AuthTime != login.Unix() {
		t.Errorf("auth_time = %d, want %d", claims.AuthTime, login.Unix())
	}
}

func TestValidateAccessToken_WrongSecret(t *testing.T) {
	pair := mustPair(t, testSecret, "user-123", "admin", 0, time.Now())
	if _, err := ValidateAccessToken("wrong-secret", pair.AccessToken); err == nil {
		t.Error("expected error when validating with wrong secret, got nil")
	}
}

// sign builds a token with arbitrary claims under an arbitrary key, for the
// cases GenerateTokenPair cannot produce.
func sign(t *testing.T, key []byte, claims jwt.Claims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}

func accessClaims(mod func(*Claims)) *Claims {
	now := time.Now()
	c := &Claims{
		UserID: "user-123", Username: "admin", Role: "admin", Typ: typAccess, AuthTime: now.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(AccessTokenDuration)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   "user-123",
			Audience:  jwt.ClaimStrings{Audience},
		},
	}
	if mod != nil {
		mod(c)
	}
	return c
}

func TestValidateAccessToken_Rejects(t *testing.T) {
	accessKey := tokenKey(testSecret, typAccess)
	if _, err := ValidateAccessToken(testSecret, sign(t, accessKey, accessClaims(nil))); err != nil {
		t.Fatalf("the reference token must validate: %v", err)
	}
	cases := map[string]string{
		"expired": sign(t, accessKey, accessClaims(func(c *Claims) {
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		})),
		"no expiry": sign(t, accessKey, accessClaims(func(c *Claims) { c.ExpiresAt = nil })),
		// A token from before the keys were split: signed with the secret itself.
		"signed with the raw secret":  sign(t, []byte(testSecret), accessClaims(nil)),
		"signed with the refresh key": sign(t, tokenKey(testSecret, typRefresh), accessClaims(nil)),
		"wrong typ":                   sign(t, accessKey, accessClaims(func(c *Claims) { c.Typ = typRefresh })),
		"no typ":                      sign(t, accessKey, accessClaims(func(c *Claims) { c.Typ = "" })),
		"no audience":                 sign(t, accessKey, accessClaims(func(c *Claims) { c.Audience = nil })),
		"another audience": sign(t, accessKey, accessClaims(func(c *Claims) {
			c.Audience = jwt.ClaimStrings{"some-other-service"}
		})),
		"alg none": func() string {
			tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, accessClaims(nil)).SignedString(jwt.UnsafeAllowNoneSignatureType)
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}(),
		"HS512 under the same key": func() string {
			tok, err := jwt.NewWithClaims(jwt.SigningMethodHS512, accessClaims(nil)).SignedString(accessKey)
			if err != nil {
				t.Fatal(err)
			}
			return tok
		}(),
	}
	for name, token := range cases {
		if _, err := ValidateAccessToken(testSecret, token); err == nil {
			t.Errorf("%s: token was accepted", name)
		}
	}
}

// An access token is never a refresh token and the other way round: the two
// are signed with different keys and carry different types.
func TestAccessAndRefreshTokensAreNotInterchangeable(t *testing.T) {
	pair := mustPair(t, testSecret, "user-123", "admin", 0, time.Now())
	if _, err := ValidateAccessToken(testSecret, pair.RefreshToken); err == nil {
		t.Error("a refresh token was accepted as an access token")
	}
	if _, err := ValidateRefreshToken(testSecret, pair.AccessToken); err == nil {
		t.Error("an access token was accepted as a refresh token")
	}
}

func TestValidateRefreshToken_Valid(t *testing.T) {
	login := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	pair := mustPair(t, testSecret, "user-456", "viewer", 0, login)

	sess, err := ValidateRefreshToken(testSecret, pair.RefreshToken)
	if err != nil {
		t.Fatalf("ValidateRefreshToken returned error: %v", err)
	}
	if sess.UserID != "user-456" {
		t.Errorf("userID = %q, want %q", sess.UserID, "user-456")
	}
	if sess.TokenVersion != 0 {
		t.Errorf("token version = %d, want 0", sess.TokenVersion)
	}
	if !sess.AuthTime.Equal(login) {
		t.Errorf("auth time = %v, want the login time %v", sess.AuthTime, login)
	}
}

func TestValidateRefreshToken_WrongSecret(t *testing.T) {
	pair := mustPair(t, testSecret, "user-456", "viewer", 0, time.Now())
	if _, err := ValidateRefreshToken("wrong-secret", pair.RefreshToken); err == nil {
		t.Error("expected error when validating with wrong secret, got nil")
	}
}

func TestValidateRefreshToken_Rejects(t *testing.T) {
	refreshKey := tokenKey(testSecret, typRefresh)
	claims := func(mod func(*refreshClaims)) *refreshClaims {
		now := time.Now()
		c := &refreshClaims{
			UserID: "user-456", Typ: typRefresh, AuthTime: now.Unix(),
			RegisteredClaims: jwt.RegisteredClaims{
				ExpiresAt: jwt.NewNumericDate(now.Add(RefreshTokenDuration)),
				IssuedAt:  jwt.NewNumericDate(now),
				Subject:   "user-456",
				Audience:  jwt.ClaimStrings{Audience},
			},
		}
		if mod != nil {
			mod(c)
		}
		return c
	}
	if _, err := ValidateRefreshToken(testSecret, sign(t, refreshKey, claims(nil))); err != nil {
		t.Fatalf("the reference token must validate: %v", err)
	}
	cases := map[string]string{
		"expired": sign(t, refreshKey, claims(func(c *refreshClaims) {
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
		})),
		"signed with the raw secret": sign(t, []byte(testSecret), claims(nil)),
		"wrong typ":                  sign(t, refreshKey, claims(func(c *refreshClaims) { c.Typ = typAccess })),
		"no audience":                sign(t, refreshKey, claims(func(c *refreshClaims) { c.Audience = nil })),
		"no user": sign(t, refreshKey, claims(func(c *refreshClaims) {
			c.Subject, c.UserID = "", ""
		})),
	}
	for name, token := range cases {
		if _, err := ValidateRefreshToken(testSecret, token); err == nil {
			t.Errorf("%s: token was accepted", name)
		}
	}
}

func TestTokenVersionRoundTrips(t *testing.T) {
	const wantVersion = 7
	pair := mustPair(t, testSecret, "user-789", "admin", wantVersion, time.Now())

	// Access token carries tv.
	claims, err := ValidateAccessToken(testSecret, pair.AccessToken)
	if err != nil {
		t.Fatalf("ValidateAccessToken returned error: %v", err)
	}
	if claims.TokenVersion != wantVersion {
		t.Errorf("access token tv = %d, want %d", claims.TokenVersion, wantVersion)
	}

	// Refresh token carries the same tv and resolves the user id.
	sess, err := ValidateRefreshToken(testSecret, pair.RefreshToken)
	if err != nil {
		t.Fatalf("ValidateRefreshToken returned error: %v", err)
	}
	if sess.UserID != "user-789" {
		t.Errorf("userID = %q, want %q", sess.UserID, "user-789")
	}
	if sess.TokenVersion != wantVersion {
		t.Errorf("refresh token tv = %d, want %d", sess.TokenVersion, wantVersion)
	}
}
