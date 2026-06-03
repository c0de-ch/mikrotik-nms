package auth

import (
	"encoding/hex"
	"testing"
)

func TestGenerateResetToken(t *testing.T) {
	a, err := GenerateResetToken()
	if err != nil {
		t.Fatalf("GenerateResetToken: %v", err)
	}
	if len(a) != 64 {
		t.Errorf("token length = %d, want 64", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("token is not valid hex: %v", err)
	}

	b, err := GenerateResetToken()
	if err != nil {
		t.Fatalf("GenerateResetToken: %v", err)
	}
	if a == b {
		t.Error("two GenerateResetToken calls returned the same token")
	}
}

func TestHashResetToken(t *testing.T) {
	const token = "deadbeef"
	h1 := HashResetToken(token)
	h2 := HashResetToken(token)

	if h1 != h2 {
		t.Errorf("HashResetToken is not deterministic: %q != %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash length = %d, want 64 (sha256 hex)", len(h1))
	}
	if _, err := hex.DecodeString(h1); err != nil {
		t.Errorf("hash is not valid hex: %v", err)
	}
	if h1 == token {
		t.Error("hash must not equal the input token")
	}
	// Different inputs hash differently.
	if HashResetToken("other") == h1 {
		t.Error("distinct tokens produced the same hash")
	}
}
