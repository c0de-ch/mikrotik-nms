package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	secret := "topsecret"
	good := sign(body, secret)

	if !verifySignature(body, good, secret) {
		t.Fatalf("expected good signature to verify")
	}
	if verifySignature(body, good, "wrongsecret") {
		t.Fatalf("expected wrong secret to fail")
	}
	if verifySignature([]byte(`{"hello":"WORLD"}`), good, secret) {
		t.Fatalf("expected tampered body to fail")
	}
	if verifySignature(body, "", secret) {
		t.Fatalf("expected empty signature to fail")
	}
	if verifySignature(body, "sha1="+good[7:], secret) {
		t.Fatalf("expected wrong scheme prefix to fail")
	}
	if verifySignature(body, "sha256=not-hex", secret) {
		t.Fatalf("expected non-hex signature to fail")
	}
}

func TestSeenCache(t *testing.T) {
	c := newSeenCache(time.Hour)
	if c.has("a") {
		t.Fatal("empty cache should not contain anything")
	}
	c.add("a")
	if !c.has("a") {
		t.Fatal("expected 'a' after add")
	}
	if c.has("b") {
		t.Fatal("expected miss for 'b'")
	}
	// empty id is a no-op
	c.add("")
	if c.has("") {
		t.Fatal("empty id should never be marked seen")
	}
}
