package api

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/alerts"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// alertsResolvedWindow is how far back GET /alerts lists resolved alerts.
const alertsResolvedWindow = 7 * 24 * time.Hour

// maxSilenceHours bounds a silence: a forgotten one must end by itself.
const maxSilenceHours = 30 * 24

type alertView struct {
	queries.Alert
	DeviceName string         `json:"device_name,omitempty"`
	Summary    string         `json:"summary"`
	Facts      map[string]any `json:"facts,omitempty"`
	RunbookURL string         `json:"runbook_url"`
	// Silenced: a silence in force keeps this alert from being notified.
	Silenced bool `json:"silenced"`
}

type channelView struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// TargetHint is enough of the target to recognise it; the address or
	// URL itself is never returned.
	TargetHint  string `json:"target_hint"`
	MinSeverity string `json:"min_severity"`
	Enabled     bool   `json:"enabled"`
}

func toChannelView(c queries.NotificationChannel) channelView {
	return channelView{ID: c.ID, Name: c.Name, Kind: c.Kind, TargetHint: alerts.TargetHint(c.Kind, c.Target),
		MinSeverity: c.MinSeverity, Enabled: c.Enabled}
}

// mountAlertRoutes mounts the alert endpoints. Reads are open to every
// logged-in user; acknowledging, silencing and the notification channels
// are admin-only (a channel target is a secret).
func (s *Server) mountAlertRoutes(r chi.Router) {
	r.Get("/alerts", s.handleListAlerts)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole("admin"))
		r.Post("/alerts/{id}/ack", s.handleAckAlert)
		r.Post("/alerts/silences", s.handleCreateSilence)
		r.Delete("/alerts/silences/{id}", s.handleDeleteSilence)
		r.Get("/alerts/channels", s.handleListChannels)
		r.Post("/alerts/channels", s.handleCreateChannel)
		r.Put("/alerts/channels/{id}", s.handleUpdateChannel)
		r.Delete("/alerts/channels/{id}", s.handleDeleteChannel)
		r.Post("/alerts/channels/{id}/test", s.handleTestChannel)
	})
}

// alertsChanged tells open pages to refetch.
func (s *Server) alertsChanged() {
	if s.hub != nil {
		s.hub.Publish(alerts.Topic, map[string]any{"at": time.Now().UTC().Format(time.RFC3339)})
	}
}

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	rows, err := queries.ListRecentAlerts(s.db, now.Add(-alertsResolvedWindow), 500)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list alerts")
		return
	}
	silences, err := queries.ListAlertSilences(s.db, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list silences")
		return
	}
	names := map[string]string{}
	if devices, err := queries.ListDevices(s.db); err == nil {
		for _, d := range devices {
			names[d.ID] = deviceDisplayName(d)
		}
	}
	firing, acked := 0, 0
	views := make([]alertView, 0, len(rows))
	for _, a := range rows {
		switch a.State {
		case queries.AlertFiring:
			firing++
		case queries.AlertAcked:
			acked++
		}
		p := alerts.ParsePayload(a.Payload)
		views = append(views, alertView{
			Alert: a, DeviceName: names[a.DeviceID], Summary: p.Summary, Facts: p.Facts,
			RunbookURL: alerts.RunbookURL(a.Rule),
			Silenced:   a.State != queries.AlertResolved && alerts.Silenced(a, silences),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"alerts":   views,
		"firing":   firing,
		"acked":    acked,
		"silences": silences,
		"rules":    alerts.RuleNames(),
		// Alerts are only evaluated on an instance that polls.
		"evaluating": s.pollersMode() == "local",
	})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (s *Server) handleAckAlert(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid alert id")
		return
	}
	who := "admin"
	if u := auth.UserFromContext(r.Context()); u != nil {
		who = u.Username
	}
	if err := queries.AckAlert(s.db, id, who, time.Now()); err != nil {
		if errors.Is(err, queries.ErrAlertNotOpen) {
			writeError(w, http.StatusConflict, "the alert is not open (it may have resolved)")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to acknowledge the alert")
		return
	}
	s.alertsChanged()
	a, err := queries.GetAlert(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the alert")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) handleCreateSilence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope  string `json:"scope"`
		Target string `json:"target"`
		Hours  int    `json:"hours"`
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Target = strings.TrimSpace(req.Target)
	switch req.Scope {
	case "site":
		req.Target = ""
	case "device":
		if _, err := queries.GetDevice(s.db, req.Target); err != nil {
			writeError(w, http.StatusBadRequest, "unknown device")
			return
		}
	case "rule":
		known := false
		for _, name := range alerts.RuleNames() {
			known = known || name == req.Target
		}
		if !known {
			writeError(w, http.StatusBadRequest, "unknown rule")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "scope must be site, device or rule")
		return
	}
	if req.Hours < 1 || req.Hours > maxSilenceHours {
		writeError(w, http.StatusBadRequest, "hours must be between 1 and "+strconv.Itoa(maxSilenceHours))
		return
	}
	now := time.Now()
	sil := &queries.AlertSilence{
		Scope: req.Scope, Target: req.Target, Until: now.Add(time.Duration(req.Hours) * time.Hour),
		Reason: strings.TrimSpace(req.Reason), CreatedBy: "admin", CreatedAt: now,
	}
	if u := auth.UserFromContext(r.Context()); u != nil {
		sil.CreatedBy = u.Username
	}
	id, err := queries.InsertAlertSilence(s.db, sil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create the silence")
		return
	}
	sil.ID = id
	sil.Until, sil.CreatedAt = sil.Until.UTC().Truncate(time.Second), sil.CreatedAt.UTC().Truncate(time.Second)
	s.alertsChanged()
	writeJSON(w, http.StatusCreated, sil)
}

func (s *Server) handleDeleteSilence(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid silence id")
		return
	}
	if err := queries.DeleteAlertSilence(s.db, id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete the silence")
		return
	}
	s.alertsChanged()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := queries.ListNotificationChannels(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list channels")
		return
	}
	out := make([]channelView, 0, len(channels))
	for _, c := range channels {
		out = append(out, toChannelView(c))
	}
	writeJSON(w, http.StatusOK, out)
}

type channelRequest struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Target      string `json:"target"`
	MinSeverity string `json:"min_severity"`
	Enabled     *bool  `json:"enabled"`
}

// writeChannelStoreError answers a failed channel write; a target that
// cannot be encrypted is reported as such (it is never stored in plaintext).
func writeChannelStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, queries.ErrSecretStorage) {
		writeError(w, http.StatusInternalServerError, strings.Replace(err.Error(), "device credentials", "the channel target", 1))
		return
	}
	writeError(w, http.StatusInternalServerError, "failed to save the channel")
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req channelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Target = strings.TrimSpace(req.Target)
	if err := alerts.ValidateTarget(req.Kind, req.Target); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.MinSeverity == "" {
		req.MinSeverity = alerts.SeverityWarning
	}
	if !alerts.ValidSeverity(req.MinSeverity) {
		writeError(w, http.StatusBadRequest, "min_severity must be info, warning or critical")
		return
	}
	c := &queries.NotificationChannel{
		Name: strings.TrimSpace(req.Name), Kind: req.Kind, Target: req.Target, MinSeverity: req.MinSeverity,
		Enabled: req.Enabled == nil || *req.Enabled, CreatedAt: time.Now(),
	}
	id, err := queries.InsertNotificationChannel(s.db, c)
	if err != nil {
		log.Printf("alerts: add channel: %v", err)
		writeChannelStoreError(w, err)
		return
	}
	c.ID = id
	writeJSON(w, http.StatusCreated, toChannelView(*c))
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	c, err := queries.GetNotificationChannel(s.db, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "channel not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to read the channel")
		return
	}
	var req channelRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// The kind is fixed; a new target is validated against it. An omitted
	// target keeps the stored one (it is never sent back to be re-posted).
	req.Target = strings.TrimSpace(req.Target)
	if req.Target != "" {
		if err := alerts.ValidateTarget(c.Kind, req.Target); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.MinSeverity != "" {
		if !alerts.ValidSeverity(req.MinSeverity) {
			writeError(w, http.StatusBadRequest, "min_severity must be info, warning or critical")
			return
		}
		c.MinSeverity = req.MinSeverity
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		c.Name = name
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	if err := queries.UpdateNotificationChannel(s.db, &c, req.Target); err != nil {
		log.Printf("alerts: update channel %d: %v", id, err)
		writeChannelStoreError(w, err)
		return
	}
	if req.Target != "" {
		c.Target = req.Target
	}
	writeJSON(w, http.StatusOK, toChannelView(c))
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	if err := queries.DeleteNotificationChannel(s.db, id); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete the channel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestChannel sends a test notification to one channel, whatever its
// minimum severity and whether or not it is enabled, and reports the result:
// {ok, message}. A delivery failure is an answer, not an HTTP error.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid channel id")
		return
	}
	c, err := queries.GetNotificationChannel(s.db, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "channel not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to read the channel")
		return
	}
	notifier := s.alertNotifier
	if notifier == nil {
		notifier = alerts.NewNotifier(s.db, s.cfg.Mailer())
	}
	if err := notifier.DeliverTo(c, alerts.TestAlert(time.Now()), alerts.EventTest); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Test notification sent to " + alerts.TargetHint(c.Kind, c.Target)})
}
