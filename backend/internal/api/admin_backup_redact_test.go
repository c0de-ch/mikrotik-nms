package api

import "testing"

func TestRedactExport_AppSettingsSecrets(t *testing.T) {
	rows := []map[string]any{
		{"key": "smtp_host", "value": "mail.example.com"},
		{"key": "smtp_password", "value": "s3cr3t"},
		{"key": "opnsense_api_secret", "value": "abc"},
		{"key": "opnsense_api_key", "value": "k"},
		{"key": "kea_url", "value": "http://kea:8000"},
		{"key": "auto_follow_ip", "value": "true"},
	}
	redactExport("app_settings", rows)

	want := map[string]string{
		"smtp_host":           "mail.example.com", // not secret — kept
		"smtp_password":       "",                 // secret — blanked
		"opnsense_api_secret": "",                 // secret — blanked
		"opnsense_api_key":    "",                 // secret — blanked
		"kea_url":             "http://kea:8000",  // not secret — kept
		"auto_follow_ip":      "true",             // not secret — kept
	}
	for _, row := range rows {
		k := row["key"].(string)
		if got := row["value"].(string); got != want[k] {
			t.Fatalf("key %q: value = %q, want %q", k, got, want[k])
		}
	}
}
