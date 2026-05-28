// Package crypto provides authenticated encryption for secrets stored at rest
// (currently RouterOS device passwords). It uses AES-256-GCM with a key derived
// from the configured MIKROTIK_NMS_ENCRYPTION_KEY.
//
// A Cipher built from an empty key is "disabled" and passes values through
// unchanged, preserving the pre-encryption behaviour for deployments that have
// not configured a key. Stored ciphertext carries a versioned prefix so plaintext
// (legacy) values are detected and migrated transparently.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// tokenPrefix marks a value as one of our ciphertext tokens.
const tokenPrefix = "enc:v1:"

// Cipher encrypts and decrypts short secrets with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from key. An empty key returns a disabled (pass-through)
// Cipher and no error. Any non-empty key is accepted and hashed to a 32-byte
// AES-256 key; a long, high-entropy key is still recommended.
func New(key string) (*Cipher, error) {
	if key == "" {
		return &Cipher{}, nil
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("crypto: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: new gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Enabled reports whether a key was configured (encryption is active).
func (c *Cipher) Enabled() bool { return c != nil && c.aead != nil }

// IsEncrypted reports whether stored is one of our ciphertext tokens.
func IsEncrypted(stored string) bool { return strings.HasPrefix(stored, tokenPrefix) }

// Encrypt returns a versioned ciphertext token for plaintext. It returns the
// input unchanged when the cipher is disabled, the input is empty, or the input
// is already a ciphertext token (idempotent).
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if !c.Enabled() || plaintext == "" || IsEncrypted(plaintext) {
		return plaintext, nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return tokenPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. Values without our token prefix are treated as
// legacy plaintext and returned unchanged, so existing rows keep working until
// they are re-saved. Decrypting a token without a configured key is an error.
func (c *Cipher) Decrypt(stored string) (string, error) {
	if !IsEncrypted(stored) {
		return stored, nil
	}
	if !c.Enabled() {
		return "", errors.New("crypto: value is encrypted but no encryption key is configured")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, tokenPrefix))
	if err != nil {
		return "", fmt.Errorf("crypto: decode: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("crypto: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	pt, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: open: %w", err)
	}
	return string(pt), nil
}
