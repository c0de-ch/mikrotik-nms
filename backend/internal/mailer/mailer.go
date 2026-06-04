// Package mailer sends transactional email (currently just self-service
// password-reset links) via a plain SMTP relay using only the Go standard
// library (net/smtp). It is modeled on internal/kea's small-client style.
//
// SAFE-DISABLE: when the relay is not configured (no host, no port, or no
// public base URL) the mailer is disabled — Send is a no-op that returns
// ErrDisabled and never opens a connection. Callers treat ErrDisabled exactly
// like a successful send so the password-reset flow stays enumeration-safe.
package mailer

import (
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// ErrDisabled is returned by Send when the mailer is not configured. It is a
// sentinel, not a failure: the password-reset handler treats it like success.
var ErrDisabled = errors.New("mailer: disabled (SMTP not configured)")

// Sender is the interface handlers depend on so tests can inject a fake.
type Sender interface {
	Send(to, subject, textBody, htmlBody string) error
	SendPasswordResetEmail(to, resetURL string) error
	Enabled() bool
}

// Config is the subset of app config the mailer needs. It mirrors the env-based
// fields on config.Config without importing that package (keeps the dependency
// direction clean and the package easy to unit-test).
type Config struct {
	SMTPHost          string
	SMTPPort          int
	SMTPUser          string
	SMTPPass          string
	SMTPFrom          string
	SMTPTLSMode       string // "starttls" | "tls" | "none"
	SMTPTLSSkipVerify bool
	PublicBaseURL     string
}

// Mailer is a stdlib net/smtp Sender.
type Mailer struct {
	host       string
	port       int
	user       string
	pass       string
	from       string
	tlsMode    string
	skipVerify bool
	timeout    time.Duration
	enabled    bool
}

// New builds a Mailer from cfg. It is enabled only when a host, a positive
// port, and a public base URL are all set — the base URL gate ensures a reset
// link is never constructed from an untrusted request Host.
func New(cfg Config) *Mailer {
	from := cfg.SMTPFrom
	if from == "" {
		from = cfg.SMTPUser
	}
	tlsMode := cfg.SMTPTLSMode
	if tlsMode == "" {
		tlsMode = "starttls"
	}
	return &Mailer{
		host:       cfg.SMTPHost,
		port:       cfg.SMTPPort,
		user:       cfg.SMTPUser,
		pass:       cfg.SMTPPass,
		from:       from,
		tlsMode:    strings.ToLower(tlsMode),
		skipVerify: cfg.SMTPTLSSkipVerify,
		timeout:    15 * time.Second,
		enabled:    cfg.SMTPHost != "" && cfg.SMTPPort > 0 && cfg.PublicBaseURL != "",
	}
}

// Enabled reports whether the mailer can actually send.
func (m *Mailer) Enabled() bool { return m.enabled }

// Send delivers a multipart/alternative (text + HTML) message. When disabled it
// is a no-op returning ErrDisabled. Errors are wrapped with a "mailer:" prefix;
// the SMTP password is never included in any returned error.
func (m *Mailer) Send(to, subject, textBody, htmlBody string) error {
	if !m.enabled {
		return ErrDisabled
	}

	fromAddr, err := mail.ParseAddress(m.from)
	if err != nil {
		return fmt.Errorf("mailer: invalid from address: %w", err)
	}
	toAddr, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("mailer: invalid to address: %w", err)
	}

	// The From header carries a friendly display name ("MikroTik NMS"); the SMTP
	// envelope (MAIL FROM) below still uses the bare address.
	msg, err := m.compose(fromHeaderValue(m.from), toAddr.Address, subject, textBody, htmlBody)
	if err != nil {
		return err
	}

	client, err := m.dial()
	if err != nil {
		return err
	}
	defer client.Close()

	if m.user != "" {
		auth := smtp.PlainAuth("", m.user, m.pass, m.host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}

	if err := client.Mail(fromAddr.Address); err != nil {
		return fmt.Errorf("mailer: MAIL FROM: %w", err)
	}
	if err := client.Rcpt(toAddr.Address); err != nil {
		return fmt.Errorf("mailer: RCPT TO: %w", err)
	}

	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	if _, err := wc.Write([]byte(msg)); err != nil {
		_ = wc.Close()
		return fmt.Errorf("mailer: write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("mailer: close body: %w", err)
	}

	return client.Quit()
}

// dial establishes an authenticated-ready smtp.Client honoring the TLS mode.
func (m *Mailer) dial() (*smtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", m.host, m.port)
	tlsCfg := &tls.Config{ServerName: m.host, InsecureSkipVerify: m.skipVerify} //nolint:gosec // skipVerify is opt-in for self-signed LAN relays

	switch m.tlsMode {
	case "tls":
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: m.timeout}, "tcp", addr, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("mailer: tls dial: %w", err)
		}
		client, err := smtp.NewClient(conn, m.host)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("mailer: new client: %w", err)
		}
		return client, nil

	case "none":
		client, err := smtp.Dial(addr)
		if err != nil {
			return nil, fmt.Errorf("mailer: dial: %w", err)
		}
		return client, nil

	default: // "starttls"
		client, err := smtp.Dial(addr)
		if err != nil {
			return nil, fmt.Errorf("mailer: dial: %w", err)
		}
		if err := client.StartTLS(tlsCfg); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("mailer: starttls: %w", err)
		}
		return client, nil
	}
}

// compose builds an RFC 5322 multipart/alternative message.
func (m *Mailer) compose(from, to, subject, textBody, htmlBody string) (string, error) {
	boundary := "mtnms-boundary-" + fmt.Sprintf("%d", time.Now().UnixNano())
	msgID := fmt.Sprintf("<%d.%s@mikrotik-nms>", time.Now().UnixNano(), sanitizeMsgIDHost(m.host))

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: %s\r\n", msgID)
	b.WriteString("MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n", boundary)
	b.WriteString("\r\n")

	// Plain text part
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(textBody)
	b.WriteString("\r\n")

	// HTML part (optional)
	if htmlBody != "" {
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		b.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(htmlBody)
		b.WriteString("\r\n")
	}

	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.String(), nil
}

// fromHeaderValue formats the From header with a friendly display name. If the
// configured From already carries a display name it is preserved; otherwise the
// product name "MikroTik NMS" is used so inboxes show "MikroTik NMS" instead of
// the bare address. The SMTP envelope still uses the bare address.
func fromHeaderValue(from string) string {
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return from
	}
	if strings.TrimSpace(addr.Name) == "" {
		addr.Name = "MikroTik NMS"
	}
	return addr.String()
}

// SendPasswordResetEmail composes and sends the reset email for resetURL.
func (m *Mailer) SendPasswordResetEmail(to, resetURL string) error {
	subject := "Reset your MikroTik NMS password"
	text := fmt.Sprintf(
		"Reset your MikroTik NMS password\r\n"+
			"================================\r\n\r\n"+
			"We received a request to reset the password for your MikroTik NMS account.\r\n"+
			"Open the link below to choose a new password:\r\n\r\n"+
			"%s\r\n\r\n"+
			"This link can be used once and expires in 1 hour. If you did not request a\r\n"+
			"password reset, you can safely ignore this email - your password will not change.\r\n\r\n"+
			"-- MikroTik NMS (automated message, please do not reply)\r\n",
		resetURL,
	)
	htmlBody := strings.ReplaceAll(passwordResetTemplate, "{{URL}}", html.EscapeString(resetURL))
	return m.Send(to, subject, text, htmlBody)
}

// passwordResetTemplate is a modern, responsive transactional email built with
// table layout + inline CSS for broad mail-client support. {{URL}} is replaced
// with the (HTML-escaped) reset link in the button, the fallback link, and its
// visible text.
const passwordResetTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta name="color-scheme" content="light only">
<title>Reset your password</title>
</head>
<body style="margin:0;padding:0;background-color:#f1f5f9;-webkit-font-smoothing:antialiased;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background-color:#f1f5f9;padding:28px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:520px;width:100%;background-color:#ffffff;border-radius:14px;overflow:hidden;border:1px solid #e2e8f0;box-shadow:0 1px 3px rgba(15,23,42,0.06);font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">
<tr><td style="background-color:#0f172a;padding:22px 32px;">
<span style="display:inline-block;width:9px;height:9px;border-radius:50%;background-color:#22c55e;margin-right:9px;vertical-align:middle;"></span>
<span style="color:#ffffff;font-size:17px;font-weight:600;letter-spacing:0.2px;vertical-align:middle;">MikroTik&nbsp;NMS</span>
</td></tr>
<tr><td style="padding:34px 32px 8px 32px;">
<h1 style="margin:0 0 14px 0;font-size:21px;line-height:28px;color:#0f172a;font-weight:600;">Reset your password</h1>
<p style="margin:0 0 24px 0;font-size:14px;line-height:23px;color:#475569;">We received a request to reset the password for your MikroTik&nbsp;NMS account. Click the button below to choose a new one.</p>
<table role="presentation" cellpadding="0" cellspacing="0" style="margin:0 0 26px 0;"><tr><td style="border-radius:9px;background-color:#2563eb;">
<a href="{{URL}}" target="_blank" style="display:inline-block;padding:13px 28px;font-size:14px;font-weight:600;color:#ffffff;text-decoration:none;border-radius:9px;">Reset password</a>
</td></tr></table>
<p style="margin:0 0 6px 0;font-size:12px;line-height:18px;color:#94a3b8;">Or paste this link into your browser:</p>
<p style="margin:0 0 26px 0;font-size:12px;line-height:18px;word-break:break-all;"><a href="{{URL}}" target="_blank" style="color:#2563eb;text-decoration:none;">{{URL}}</a></p>
</td></tr>
<tr><td style="padding:0 32px 30px 32px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td style="border-top:1px solid #e2e8f0;padding-top:18px;">
<p style="margin:0;font-size:12px;line-height:19px;color:#94a3b8;">This link can be used once and expires in <strong style="color:#64748b;">1&nbsp;hour</strong>. If you didn&rsquo;t request a password reset, you can safely ignore this email &mdash; your password won&rsquo;t change.</p>
</td></tr></table>
</td></tr>
</table>
<p style="max-width:520px;margin:16px auto 0 auto;font-size:11px;line-height:16px;color:#94a3b8;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">MikroTik&nbsp;NMS &middot; automated message, please do not reply</p>
</td></tr>
</table>
</body>
</html>`

// sanitizeMsgIDHost keeps the Message-ID right-hand side syntactically safe.
func sanitizeMsgIDHost(host string) string {
	if host == "" {
		return "localhost"
	}
	return host
}
