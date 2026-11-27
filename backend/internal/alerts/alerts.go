// Package alerts is the minimal alerting of the NMS: rules that look at what
// the pollers already store, an engine that turns what they see into alerts
// (one open alert per rule and key), and a notifier that delivers them by
// e-mail or to a webhook. It reads the database only — an alert never costs
// a device a command.
package alerts

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Severities, in rising order.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Topic is the WebSocket topic that says the alerts changed (opened,
// resolved, acknowledged, silenced): subscribers refetch GET /alerts.
const Topic = "alerts.update"

// SeverityRank orders severities (unknown = 0, below info).
func SeverityRank(s string) int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityWarning:
		return 2
	case SeverityCritical:
		return 3
	}
	return 0
}

// ValidSeverity reports whether s is one of the three severities.
func ValidSeverity(s string) bool { return SeverityRank(s) > 0 }

const (
	// EvalInterval is how often the rules are evaluated.
	EvalInterval = 30 * time.Second
	// RenotifyEvery is how often a firing alert nobody acknowledged is sent
	// again.
	RenotifyEvery = 4 * time.Hour
	// ResolvedRetention is how long resolved alerts are kept.
	ResolvedRetention = 30 * 24 * time.Hour
)

// Observation is one condition a rule currently sees.
type Observation struct {
	// Key identifies what the rule fired for (usually the device id): one
	// open alert exists per rule and key.
	Key      string
	DeviceID string
	Severity string
	// Summary is the one-line, human-readable statement of the condition.
	Summary string
	// Facts are rule-specific details stored with the alert.
	Facts map[string]any
	// Hold says the rule cannot tell whether the condition holds — the
	// device was not measured, or the measurements disagree — which is
	// neither "firing" nor "healthy": an open alert stays exactly as it is
	// and no alert is opened. With a Key it holds that one alert; with only
	// a DeviceID, every alert of the rule for that device. Severity and
	// Summary are not needed.
	Hold bool
}

// Rule looks at the stored state and reports every condition that holds
// now. An error means "could not tell": the rule's open alerts are left as
// they are (never resolved on a failed evaluation).
type Rule interface {
	Name() string
	Evaluate(now time.Time) ([]Observation, error)
}

// RunbookBase is where a rule's runbook lives; the rule name is the anchor.
const RunbookBase = "https://github.com/c0de-ch/mikrotik-nms/blob/main/docs/RUNBOOKS.md"

// RunbookURL is the runbook section for a rule.
func RunbookURL(rule string) string { return RunbookBase + "#" + rule }

// Payload is what an alert stores as JSON.
type Payload struct {
	Summary string         `json:"summary"`
	Facts   map[string]any `json:"facts,omitempty"`
}

// ParsePayload reads an alert's stored payload (a damaged one is empty).
func ParsePayload(raw string) Payload {
	var p Payload
	_ = json.Unmarshal([]byte(raw), &p)
	return p
}

func encodePayload(o Observation) string {
	b, err := json.Marshal(Payload{Summary: o.Summary, Facts: o.Facts})
	if err != nil {
		return `{}`
	}
	return string(b)
}

// Event is what a notification is about.
type Event string

const (
	EventFiring   Event = "firing"
	EventResolved Event = "resolved"
	EventTest     Event = "test"
)

// Deliverer sends a notification about an alert to the channels that want
// it and reports how many deliveries succeeded.
type Deliverer interface {
	Deliver(alert queries.Alert, event Event) (delivered int, err error)
}

// Engine evaluates the rules and keeps the alerts table in step with what
// they see.
type Engine struct {
	db       *sql.DB
	rules    []Rule
	notifier Deliverer
	// OnChange, when set, is called after a tick that opened, resolved or
	// re-notified an alert (the manager publishes alerts.update from it).
	OnChange func()

	now func() time.Time
	mu  sync.Mutex // one tick at a time
}

// NewEngine returns an engine over the given rules. notifier may be nil
// (alerts are recorded, nothing is sent).
func NewEngine(db *sql.DB, notifier Deliverer, rules ...Rule) *Engine {
	return &Engine{db: db, rules: rules, notifier: notifier, now: time.Now}
}

// Run evaluates every EvalInterval until stop is closed.
func (e *Engine) Run(stop <-chan struct{}) {
	// The first evaluation waits one interval: right after a start the
	// pollers have not refreshed anything yet, and device status is whatever
	// the last run left behind.
	ticker := time.NewTicker(EvalInterval)
	defer ticker.Stop()
	var lastPrune time.Time
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		e.safeTick()
		if now := e.now(); now.Sub(lastPrune) >= time.Hour {
			lastPrune = now
			if _, err := queries.DeleteResolvedAlertsBefore(e.db, now.Add(-ResolvedRetention)); err != nil {
				log.Printf("alerts: prune: %v", err)
			}
			_ = queries.DeleteExpiredAlertSilences(e.db, now.Add(-ResolvedRetention))
		}
	}
}

func (e *Engine) safeTick() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("alerts: panic: %v", r)
		}
	}()
	if err := e.Tick(); err != nil {
		log.Printf("alerts: %v", err)
	}
}

type ruleKey struct{ rule, key string }

// Tick is one evaluation: every rule is asked what holds now; a condition
// without an open alert opens one, an open alert whose condition is gone is
// resolved, and whatever is due is notified.
func (e *Engine) Tick() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()

	seen := make(map[ruleKey]Observation)
	// What a rule could not tell: by (rule, key), and by (rule, device).
	heldKeys := make(map[ruleKey]bool)
	heldDevices := make(map[ruleKey]bool)
	failed := make(map[string]bool)
	var errs []error
	for _, r := range e.rules {
		obs, err := r.Evaluate(now)
		if err != nil {
			failed[r.Name()] = true
			errs = append(errs, err)
			continue
		}
		for _, o := range obs {
			if o.Hold {
				switch {
				case o.Key != "":
					heldKeys[ruleKey{r.Name(), o.Key}] = true
				case o.DeviceID != "":
					heldDevices[ruleKey{r.Name(), o.DeviceID}] = true
				}
				continue
			}
			if o.Key == "" || !ValidSeverity(o.Severity) {
				continue
			}
			seen[ruleKey{r.Name(), o.Key}] = o
		}
	}
	known := make(map[string]bool, len(e.rules))
	for _, r := range e.rules {
		known[r.Name()] = true
	}

	open, err := queries.ListOpenAlerts(e.db)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	changed := false
	var resolved []queries.Alert
	for _, a := range open {
		// A rule that could not be evaluated, or one this build no longer
		// has, says nothing about its alerts: they stay.
		if failed[a.Rule] || !known[a.Rule] {
			continue
		}
		k := ruleKey{a.Rule, a.Key}
		o, still := seen[k]
		if !still {
			// The rule could not tell for this alert: unknown is not
			// healthy, so it neither resolves nor counts as seen.
			if heldKeys[k] || (a.DeviceID != "" && heldDevices[ruleKey{a.Rule, a.DeviceID}]) {
				continue
			}
			if err := queries.ResolveAlert(e.db, a.ID, now); err != nil {
				errs = append(errs, err)
				continue
			}
			changed = true
			resolved = append(resolved, a)
			continue
		}
		delete(seen, k)
		if err := queries.TouchAlert(e.db, a.ID, o.Severity, encodePayload(o), now); err != nil {
			errs = append(errs, err)
			continue
		}
		// It got worse: announce it again now rather than at the next
		// four-hourly reminder (an acknowledged alert stays quiet).
		if a.State == queries.AlertFiring && SeverityRank(o.Severity) > SeverityRank(a.Severity) {
			if err := queries.RenotifyAlert(e.db, a.ID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for k, o := range seen {
		if _, err := queries.InsertAlert(e.db, &queries.Alert{
			Rule: k.rule, Key: k.key, DeviceID: o.DeviceID, Severity: o.Severity,
			FirstAt: now, LastAt: now, Confirmed: true, Payload: encodePayload(o),
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		changed = true
	}

	if e.notifier != nil {
		if e.notify(now, resolved) {
			changed = true
		}
	}
	if changed && e.OnChange != nil {
		e.OnChange()
	}
	return errors.Join(errs...)
}

// notify sends what is due: a firing alert that was never delivered or was
// last delivered RenotifyEvery ago, and the resolution of an alert that had
// been delivered. Silenced alerts are skipped — and picked up once the
// silence ends, since nothing was recorded as delivered. It reports whether
// anything was delivered.
func (e *Engine) notify(now time.Time, resolved []queries.Alert) bool {
	silences, err := queries.ListAlertSilences(e.db, now)
	if err != nil {
		log.Printf("alerts: silences: %v", err)
		return false
	}
	sent := false
	for _, a := range resolved {
		if a.LastNotifiedAt == nil || Silenced(a, silences) {
			continue // nobody was told it fired
		}
		a.State, a.ResolvedAt = queries.AlertResolved, &now
		if n, err := e.notifier.Deliver(a, EventResolved); err != nil {
			log.Printf("alerts: notify %s/%s resolved: %v", a.Rule, a.Key, err)
		} else if n > 0 {
			sent = true
		}
	}
	open, err := queries.ListOpenAlerts(e.db)
	if err != nil {
		log.Printf("alerts: list: %v", err)
		return sent
	}
	for _, a := range open {
		if a.State != queries.AlertFiring || Silenced(a, silences) {
			continue
		}
		if a.LastNotifiedAt != nil && now.Sub(*a.LastNotifiedAt) < RenotifyEvery {
			continue
		}
		n, err := e.notifier.Deliver(a, EventFiring)
		if err != nil {
			log.Printf("alerts: notify %s/%s: %v", a.Rule, a.Key, err)
		}
		if n == 0 {
			continue // nothing delivered: try again next tick
		}
		if err := queries.MarkAlertNotified(e.db, a.ID, now); err != nil {
			log.Printf("alerts: mark notified: %v", err)
		}
		sent = true
	}
	return sent
}

// Silenced reports whether a silence in force covers the alert.
func Silenced(a queries.Alert, silences []queries.AlertSilence) bool {
	for _, s := range silences {
		switch s.Scope {
		case "site":
			return true
		case "device":
			if a.DeviceID != "" && s.Target == a.DeviceID {
				return true
			}
		case "rule":
			if s.Target == a.Rule {
				return true
			}
		}
	}
	return false
}
