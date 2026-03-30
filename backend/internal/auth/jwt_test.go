package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-key-for-unit-tests"

func TestGenerateTokenPair(t *testing.T) {
	pair, err := GenerateTokenPair(testSecret, "user-123", "admin", "admin")
	if err != nil {
		t.Fatalf("GenerateTokenPair returned error: %v", err)
	}
	if pair.AccessToken == "" {
		t.Error("AccessToken is empty")
	}
	if pair.RefreshToken == "" {
		t.Error("RefreshToken is empty")
	}
	if pair.ExpiresAt == 0 {
		t.Error("ExpiresAt is zero")
	}
	// ExpiresAt should be in the future
	if pair.ExpiresAt <= time.Now().Unix() {
		t.Error("ExpiresAt should be in the future")
	}
}

func TestValidateAccessToken_Valid(t *testing.T) {
	pair, err := GenerateTokenPair(testSecret, "user-123", "admin", "admin")
	if err != nil {
		t.Fatalf("GenerateTokenPair returned error: %v", err)
	}

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
}

func TestValidateAccessToken_WrongSecret(t *testing.T) {
	pair, err := GenerateTokenPair(testSecret, "user-123", "admin", "admin")
	if err != nil {
		t.Fatalf("GenerateTokenPair returned error: %v", err)
	}

	_, err = ValidateAccessToken("wrong-secret", pair.AccessToken)
	if err == nil {
		t.Error("expected error when validating with wrong secret, got nil")
	}
}

func TestValidateAccessToken_Expired(t *testing.T) {
	// Manually create an expired token
	now := time.Now().Add(-1 * time.Hour)
	claims := &Claims{
		UserID:   "user-123",
		Username: "admin",
		Role:     "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(-30 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   "user-123",
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("failed to create expired token: %v", err)
	}

	_, err = ValidateAccessToken(testSecret, token)
	if err == nil {
		t.Error("expected error for expired token, got nil")
	}
}

func TestValidateRefreshToken_Valid(t *testing.T) {
	pair, err := GenerateTokenPair(testSecret, "user-456", "viewer", "viewer")
	if err != nil {
		t.Fatalf("GenerateTokenPair returned error: %v", err)
	}

	userID, err := ValidateRefreshToken(testSecret, pair.RefreshToken)
	if err != nil {
		t.Fatalf("ValidateRefreshToken returned error: %v", err)
	}
	if userID != "user-456" {
		t.Errorf("userID = %q, want %q", userID, "user-456")
	}
}

func TestValidateRefreshToken_WrongSecret(t *testing.T) {
	pair, err := GenerateTokenPair(testSecret, "user-456", "viewer", "viewer")
	if err != nil {
		t.Fatalf("GenerateTokenPair returned error: %v", err)
	}

	_, err = ValidateRefreshToken("wrong-secret", pair.RefreshToken)
	if err == nil {
		t.Error("expected error when validating with wrong secret, got nil")
	}
}

func TestValidateRefreshToken_Expired(t *testing.T) {
	// Manually create an expired refresh token
	now := time.Now().Add(-8 * 24 * time.Hour)
	claims := &jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(now.Add(-1 * time.Hour)),
		IssuedAt:  jwt.NewNumericDate(now),
		Subject:   "user-456",
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("failed to create expired refresh token: %v", err)
	}

	_, err = ValidateRefreshToken(testSecret, token)
	if err == nil {
		t.Error("expected error for expired refresh token, got nil")
	}
}
