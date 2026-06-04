package mailer

import (
	"html"
	"strings"
	"testing"
)

func TestFromHeaderValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"nms@example.com", `"MikroTik NMS" <nms@example.com>`},               // bare -> add product name
		{"MikroTik NMS <nms@example.com>", `"MikroTik NMS" <nms@example.com>`}, // existing name preserved
		{`"Custom Name" <a@b.com>`, `"Custom Name" <a@b.com>`},               // custom name preserved
		{"not-an-address", "not-an-address"},                                // unparseable -> returned as-is
	}
	for _, c := range cases {
		if got := fromHeaderValue(c.in); got != c.want {
			t.Errorf("fromHeaderValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPasswordResetHTML(t *testing.T) {
	url := "https://nms.example.com/reset-password?token=abc123"
	body := strings.ReplaceAll(passwordResetTemplate, "{{URL}}", html.EscapeString(url))

	for _, want := range []string{
		`href="https://nms.example.com/reset-password?token=abc123"`, // link wired into href
		"Reset password",        // CTA button label
		"MikroTik",              // brand present
		"expires in",            // security note present
		"<!DOCTYPE html>",       // full HTML document
	} {
		if !strings.Contains(body, want) {
			t.Errorf("reset HTML missing %q", want)
		}
	}
	if strings.Contains(body, "{{URL}}") {
		t.Error("template placeholder {{URL}} was not replaced")
	}
}
