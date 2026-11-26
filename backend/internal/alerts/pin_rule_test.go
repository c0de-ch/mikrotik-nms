package alerts

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// tls_pin_mismatch fires, critical, while a TLS device's sessions are being
// refused over its pin — and stops when the device is pinned to the
// certificate that was presented, when TLS is switched off, when the device
// is gone, or when no mismatch has been seen for the rule's window.
func TestTLSPinMismatchRule(t *testing.T) {
	db := testDB(t)
	pinned, presented, older := strings.Repeat("0a", 32), strings.Repeat("0b", 32), strings.Repeat("0c", 32)
	for _, d := range []queries.Device{
		{ID: "d1", Identity: "sw-tls", Address: "192.0.2.1", UseTLS: true, PinCertSHA256: pinned},
		{ID: "d2", Identity: "sw-repinned", Address: "192.0.2.2", UseTLS: true, PinCertSHA256: presented},
		{ID: "d3", Identity: "sw-plain-now", Address: "192.0.2.3", UseTLS: false, PinCertSHA256: pinned},
		{ID: "d4", Identity: "sw-long-ago", Address: "192.0.2.4", UseTLS: true, PinCertSHA256: pinned},
		{ID: "d5", Identity: "sw-other-finding", Address: "192.0.2.5", UseTLS: true, PinCertSHA256: pinned},
	} {
		d.Username, d.APIPort, d.Status, d.Tags = "u", 8729, "online", "[]"
		if err := queries.CreateDevice(db, &d); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for _, f := range []queries.DeviceFinding{
		{DeviceID: "d1", Kind: queries.FindingTLSPinMismatch, Severity: "critical", Detail: presented},
		{DeviceID: "d2", Kind: queries.FindingTLSPinMismatch, Severity: "critical", Detail: presented},
		{DeviceID: "d3", Kind: queries.FindingTLSPinMismatch, Severity: "critical", Detail: presented},
		{DeviceID: "d4", Kind: queries.FindingTLSPinMismatch, Severity: "critical", Detail: older},
		{DeviceID: "d5", Kind: RuleParallelAPIPoller, Severity: "warn", Detail: "user other from 192.0.2.50 via api"},
	} {
		if err := queries.UpsertDeviceFinding(db, &f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE device_findings SET last_seen = ? WHERE device_id = 'd4'`, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}

	rule := tlsPinMismatch{db}
	if rule.Name() != RuleTLSPinMismatch || rule.Name() != queries.FindingTLSPinMismatch {
		t.Fatalf("rule name %q, finding kind %q: they must be the same word", rule.Name(), queries.FindingTLSPinMismatch)
	}
	obs, err := rule.Evaluate(now.Add(time.Minute))
	if err != nil || len(obs) != 1 {
		t.Fatalf("observations = %+v (%v), want only the device still pinned to another certificate", obs, err)
	}
	o := obs[0]
	if o.DeviceID != "d1" || o.Severity != SeverityCritical || o.Key != "d1|"+presented {
		t.Fatalf("observation = %+v", o)
	}
	if !strings.Contains(o.Summary, "sw-tls") || !strings.Contains(o.Summary, "192.0.2.1") {
		t.Errorf("summary %q does not name the device and its address", o.Summary)
	}
	if o.Facts["presented"] != presented || o.Facts["pinned"] != pinned || o.Facts["device"] != "sw-tls" {
		t.Errorf("facts = %v, want the presented and the pinned fingerprint", o.Facts)
	}

	// The admin checked the new certificate on the device and pinned it: the
	// alert has nothing left to say.
	d1, _ := queries.GetDevice(db, "d1")
	d1.PinCertSHA256 = presented
	if err := queries.UpdateDevice(db, d1); err != nil {
		t.Fatal(err)
	}
	if obs, err := rule.Evaluate(now.Add(2 * time.Minute)); err != nil || len(obs) != 0 {
		t.Fatalf("after re-pinning: %+v (%v), want no observation", obs, err)
	}
}

// The rule is part of what a polling instance evaluates, and has a runbook.
func TestTLSPinMismatchRuleIsRegistered(t *testing.T) {
	if !slices.Contains(RuleNames(), RuleTLSPinMismatch) {
		t.Errorf("RuleNames() = %v, want %s in it", RuleNames(), RuleTLSPinMismatch)
	}
	found := false
	for _, r := range DefaultRules(testDB(t)) {
		found = found || r.Name() == RuleTLSPinMismatch
	}
	if !found {
		t.Errorf("DefaultRules lacks %s", RuleTLSPinMismatch)
	}
	if got := RunbookURL(RuleTLSPinMismatch); !strings.HasSuffix(got, "RUNBOOKS.md#tls_pin_mismatch") {
		t.Errorf("runbook URL = %q", got)
	}
	if len(RuleNames()) != len(DefaultRules(testDB(t))) {
		t.Errorf("RuleNames() and DefaultRules() disagree: %d names, %d rules", len(RuleNames()), len(DefaultRules(testDB(t))))
	}
}
