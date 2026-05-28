package crypto

import "testing"

func TestRoundTrip(t *testing.T) {
	c, err := New("a-test-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !c.Enabled() {
		t.Fatal("cipher should be enabled with a key")
	}
	tok, err := c.Encrypt("s3cret-pass")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !IsEncrypted(tok) {
		t.Fatalf("ciphertext missing token prefix: %q", tok)
	}
	if tok == "s3cret-pass" {
		t.Fatal("ciphertext equals plaintext")
	}
	got, err := c.Decrypt(tok)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "s3cret-pass" {
		t.Fatalf("round-trip mismatch: got %q", got)
	}
}

func TestEncryptIsNonDeterministic(t *testing.T) {
	c, _ := New("k")
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if a == b {
		t.Fatal("two encryptions of the same plaintext should differ (random nonce)")
	}
}

func TestDisabledPassthrough(t *testing.T) {
	c, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if c.Enabled() {
		t.Fatal("empty key must yield a disabled cipher")
	}
	out, _ := c.Encrypt("plain")
	if out != "plain" {
		t.Fatalf("disabled Encrypt should pass through, got %q", out)
	}
	back, _ := c.Decrypt("plain")
	if back != "plain" {
		t.Fatalf("disabled Decrypt should pass through, got %q", back)
	}
}

func TestLegacyPlaintextDecryptsToItself(t *testing.T) {
	c, _ := New("k")
	out, err := c.Decrypt("legacy-plaintext")
	if err != nil {
		t.Fatalf("Decrypt of non-token value should not error: %v", err)
	}
	if out != "legacy-plaintext" {
		t.Fatalf("got %q", out)
	}
}

func TestWrongKeyFails(t *testing.T) {
	c1, _ := New("key-one")
	tok, _ := c1.Encrypt("secret")
	c2, _ := New("key-two")
	if _, err := c2.Decrypt(tok); err == nil {
		t.Fatal("decrypting with the wrong key should fail")
	}
}

func TestEmptyAndIdempotent(t *testing.T) {
	c, _ := New("k")
	if out, _ := c.Encrypt(""); out != "" {
		t.Fatalf("empty plaintext should stay empty, got %q", out)
	}
	tok, _ := c.Encrypt("x")
	if again, _ := c.Encrypt(tok); again != tok {
		t.Fatal("encrypting an already-encrypted token should be idempotent")
	}
}
