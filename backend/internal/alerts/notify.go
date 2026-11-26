package alerts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/crypto"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/mailer"
)

// Channel kinds.
const (
	ChannelSMTP    = "smtp"
	ChannelWebhook = "webhook"
)

// webhookTimeout bounds one webhook delivery.
const webhookTimeout = 10 * time.Second

// ValidateTarget checks a channel target: an e-mail address for smtp, an
// http(s) URL with a host for webhook.
func ValidateTarget(kind, target string) error {
	target = strings.TrimSpace(target)
	switch kind {
	case ChannelSMTP:
		if _, err := mail.ParseAddress(target); err != nil || !strings.Contains(target, "@") {
			return errors.New("an smtp channel needs an e-mail address")
		}
	case ChannelWebhook:
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("a webhook channel needs an http:// or https:// URL")
		}
	default:
		return errors.New("kind must be smtp or webhook")
	}
	return nil
}

// TargetHint is what the API shows of a channel target: enough to recognise
// it, never enough to use it (a webhook URL is a bearer secret).
func TargetHint(kind, target string) string {
	if target == crypto.Unreadable {
		return "(cannot be decrypted)"
	}
	switch kind {
	case ChannelSMTP:
		local, domain, ok := strings.Cut(target, "@")
		if !ok || local == "" {
			return "…"
		}
		return local[:1] + "…@" + domain
	case ChannelWebhook:
		if u, err := url.Parse(target); err == nil && u.Host != "" {
			return u.Scheme + "://" + u.Host + "/…"
		}
	}
	return "…"
}

// Notifier delivers alert notifications to the configured channels.
type Notifier struct {
	db *sql.DB
	// mailBase is the env-based SMTP configuration; the Settings page's
	// values override it field by field at every send.
	mailBase mailer.Config

	// Seams for tests.
	sendMail func(cfg mailer.Config, to, subject, text, htmlBody string) error
	client   *http.Client
}

// NewNotifier returns a notifier reading its channels and SMTP settings
// from db.
func NewNotifier(db *sql.DB, mailBase mailer.Config) *Notifier {
	return &Notifier{
		db:       db,
		mailBase: mailBase,
		sendMail: func(cfg mailer.Config, to, subject, text, htmlBody string) error {
			return mailer.New(cfg).Send(to, subject, text, htmlBody)
		},
		client: &http.Client{Timeout: webhookTimeout},
	}
}

func (n *Notifier) mailConfig() mailer.Config {
	return mailer.ConfigFromSettings(func(key string) string {
		v, err := queries.GetSetting(n.db, key)
		if err != nil {
			return ""
		}
		return v
	}, n.mailBase)
}

// Deliver sends the alert to every enabled channel whose minimum severity
// it reaches, and reports how many deliveries succeeded. A failing channel
// does not stop the others; their errors are joined.
func (n *Notifier) Deliver(alert queries.Alert, event Event) (int, error) {
	channels, err := queries.ListNotificationChannels(n.db)
	if err != nil {
		return 0, err
	}
	delivered := 0
	var errs []error
	for _, ch := range channels {
		if !ch.Enabled || SeverityRank(alert.Severity) < SeverityRank(ch.MinSeverity) {
			continue
		}
		if err := n.DeliverTo(ch, alert, event); err != nil {
			errs = append(errs, fmt.Errorf("channel %d (%s): %w", ch.ID, ch.Kind, err))
			continue
		}
		delivered++
	}
	return delivered, errors.Join(errs...)
}

// DeliverTo sends one notification to one channel.
func (n *Notifier) DeliverTo(ch queries.NotificationChannel, alert queries.Alert, event Event) error {
	if ch.Target == crypto.Unreadable {
		return errors.New("the channel's target cannot be decrypted — enter it again")
	}
	msg := n.compose(alert, event)
	switch ch.Kind {
	case ChannelSMTP:
		cfg := n.mailConfig()
		if cfg.SMTPHost == "" || cfg.SMTPPort <= 0 {
			return errors.New("no SMTP relay is configured (Settings → Email (SMTP))")
		}
		return n.sendMail(cfg, ch.Target, msg.Title, msg.Text, msg.HTML)
	case ChannelWebhook:
		return n.postWebhook(ch.Target, msg)
	}
	return fmt.Errorf("unknown channel kind %q", ch.Kind)
}

// message is one notification, rendered for both transports.
type message struct {
	Title string
	Text  string
	HTML  string
	Body  webhookBody
}

// webhookBody is the JSON a webhook receives. title/message/priority are the
// names Gotify reads, text the one Slack-compatible endpoints read; alert
// carries the structured facts for anything that wants them.
type webhookBody struct {
	Title    string       `json:"title"`
	Message  string       `json:"message"`
	Text     string       `json:"text"`
	Priority int          `json:"priority"`
	Alert    webhookAlert `json:"alert"`
}

type webhookAlert struct {
	ID         int64          `json:"id,omitempty"`
	Event      Event          `json:"event"`
	Rule       string         `json:"rule"`
	Key        string         `json:"key"`
	DeviceID   string         `json:"device_id,omitempty"`
	Severity   string         `json:"severity"`
	State      string         `json:"state"`
	Summary    string         `json:"summary"`
	FirstAt    string         `json:"first_at"`
	ResolvedAt string         `json:"resolved_at,omitempty"`
	RunbookURL string         `json:"runbook_url"`
	URL        string         `json:"url,omitempty"`
	Facts      map[string]any `json:"facts,omitempty"`
}

func (n *Notifier) compose(a queries.Alert, event Event) message {
	p := ParsePayload(a.Payload)
	summary := p.Summary
	if summary == "" {
		summary = a.Rule + " " + a.Key
	}
	label := strings.ToUpper(a.Severity)
	priority := map[string]int{SeverityInfo: 2, SeverityWarning: 5, SeverityCritical: 8}[a.Severity]
	switch event {
	case EventResolved:
		label, priority = "RESOLVED", 2
	case EventTest:
		label, priority = "TEST", 2
	}
	title := fmt.Sprintf("[NMS] %s: %s", label, summary)
	runbook := RunbookURL(a.Rule)
	link := ""
	if base := strings.TrimRight(strings.TrimSpace(n.mailBase.PublicBaseURL), "/"); base != "" {
		link = base + "/alerts"
	}

	lines := []string{summary, ""}
	lines = append(lines, "Rule:     "+a.Rule, "Severity: "+a.Severity)
	if !a.FirstAt.IsZero() {
		lines = append(lines, "Since:    "+a.FirstAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if event == EventResolved && a.ResolvedAt != nil {
		lines = append(lines, "Resolved: "+a.ResolvedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if event != EventTest {
		lines = append(lines, "Runbook:  "+runbook)
	}
	if link != "" {
		lines = append(lines, "Alerts:   "+link)
	}
	// A firing alert whose rule attached rollback steps carries them in
	// the mail: whoever is paged has them without opening the NMS.
	var rollback []string
	if event == EventFiring {
		rollback = factLines(p.Facts["rollback"])
	}
	text := strings.Join(lines, "\r\n")
	if len(rollback) > 0 {
		text += "\r\n\r\nRollback:\r\n" + strings.Join(rollback, "\r\n")
	}
	text += "\r\n\r\n-- MikroTik NMS (automated message, please do not reply)\r\n"

	var hb strings.Builder
	hb.WriteString("<p><strong>" + html.EscapeString(summary) + "</strong></p><table cellpadding=\"3\">")
	for _, l := range lines[2:] {
		k, v, _ := strings.Cut(l, ":")
		v = strings.TrimSpace(v)
		cell := html.EscapeString(v)
		if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
			cell = `<a href="` + html.EscapeString(v) + `">` + cell + `</a>`
		}
		hb.WriteString("<tr><td>" + html.EscapeString(k) + "</td><td>" + cell + "</td></tr>")
	}
	hb.WriteString("</table>")
	if len(rollback) > 0 {
		hb.WriteString("<p><strong>Rollback</strong></p><pre>" + html.EscapeString(strings.Join(rollback, "\n")) + "</pre>")
	}

	body := webhookBody{
		Title: title, Message: summary, Text: title, Priority: priority,
		Alert: webhookAlert{
			ID: a.ID, Event: event, Rule: a.Rule, Key: a.Key, DeviceID: a.DeviceID, Severity: a.Severity,
			State: a.State, Summary: summary, RunbookURL: runbook, URL: link, Facts: p.Facts,
		},
	}
	if !a.FirstAt.IsZero() {
		body.Alert.FirstAt = a.FirstAt.UTC().Format(time.RFC3339)
	}
	if a.ResolvedAt != nil {
		body.Alert.ResolvedAt = a.ResolvedAt.UTC().Format(time.RFC3339)
	}
	return message{Title: title, Text: text, HTML: hb.String(), Body: body}
}

// factLines reads a fact that is a list of text lines (stored as a JSON
// array of strings); anything else is no lines.
func factLines(v any) []string {
	var out []string
	switch list := v.(type) {
	case []string:
		out = list
	case []any:
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// postWebhook POSTs the notification as JSON. Errors never contain the URL:
// it is a secret, and errors end up in logs and API responses.
func (n *Notifier) postWebhook(target string, msg message) error {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return errors.New("the webhook URL is not valid")
	}
	payload, err := json.Marshal(msg.Body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return errors.New("the webhook URL is not valid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mikrotik-nms-alerts")
	resp, err := n.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL the client put in front
		}
		return fmt.Errorf("webhook %s: %v", u.Host, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook %s answered HTTP %d", u.Host, resp.StatusCode)
	}
	return nil
}

// TestAlert is the alert a channel test sends.
func TestAlert(now time.Time) queries.Alert {
	payload, _ := json.Marshal(Payload{Summary: "Test notification — this channel works"})
	return queries.Alert{
		Rule: "test", Key: "test", Severity: SeverityInfo, State: queries.AlertFiring,
		FirstAt: now, LastAt: now, Confirmed: true, Payload: string(payload),
	}
}
