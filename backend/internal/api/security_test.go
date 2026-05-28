package api

import (
	"strings"
	"testing"
	"time"
)

func TestRateLimiterAllowsUpToLimitThenBlocks(t *testing.T) {
	// Construct directly to avoid starting the janitor goroutine.
	rl := &rateLimiter{hits: map[string][]time.Time{}, limit: 3, window: time.Minute}
	now := time.Now()

	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4", now) {
			t.Fatalf("request %d within the limit should be allowed", i+1)
		}
	}
	if rl.allow("1.2.3.4", now) {
		t.Fatal("the 4th request should be blocked")
	}
	// A different key is independent.
	if !rl.allow("5.6.7.8", now) {
		t.Fatal("a different IP should not be rate-limited")
	}
	// The window slides: requests outside the window are forgotten.
	if !rl.allow("1.2.3.4", now.Add(2*time.Minute)) {
		t.Fatal("requests should be allowed again after the window passes")
	}
}

func TestRedactTokenHidesValueKeepsOthers(t *testing.T) {
	got := redactToken("token=supersecret123&topic=device.health")
	if strings.Contains(got, "supersecret123") {
		t.Fatalf("token value leaked: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("token not redacted: %q", got)
	}
	if !strings.Contains(got, "topic=device.health") {
		t.Fatalf("non-secret params should survive: %q", got)
	}
}

func TestRedactTokenNoToken(t *testing.T) {
	got := redactToken("a=1&b=2")
	if strings.Contains(got, "REDACTED") {
		t.Fatalf("nothing should be redacted: %q", got)
	}
}

func TestIsSecretSettingKey(t *testing.T) {
	secret := []string{"opnsense_api_secret", "opnsense_api_key", "some_password", "auth_token"}
	for _, k := range secret {
		if !isSecretSettingKey(k) {
			t.Errorf("%q should be treated as secret", k)
		}
	}
	notSecret := []string{"wifi_interval", "kea_url", "dark_mode", "opnsense_url"}
	for _, k := range notSecret {
		if isSecretSettingKey(k) {
			t.Errorf("%q should NOT be treated as secret", k)
		}
	}
}
