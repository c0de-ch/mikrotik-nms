package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// GenerateResetToken returns a 256-bit (32 byte) cryptographically random token
// encoded as a 64-character hex string. The raw token is sent ONLY in the
// emailed reset link and is never persisted; only its sha256 hash is stored.
func GenerateResetToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashResetToken returns the lowercase hex sha256 digest of a reset token. The
// digest is what we store and look up by (token_hash); the raw token never
// touches the database.
func HashResetToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
