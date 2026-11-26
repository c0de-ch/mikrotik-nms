package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/crypto"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

func devicesRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Post("/devices", s.handleCreateDevice)
	r.Put("/devices/{id}", s.handleUpdateDevice)
	r.Delete("/devices/{id}", s.handleDeleteDevice)
	return r
}

func doDevice(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, &buf))
	return rec
}

func storedSecret(t *testing.T, s *Server, id string) string {
	t.Helper()
	var raw string
	if err := s.db.QueryRow(`SELECT password_enc FROM devices WHERE id=?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// installCipher sets the process cipher for one test.
func installCipher(t *testing.T, key string) {
	t.Helper()
	c, err := crypto.New(key)
	if err != nil {
		t.Fatal(err)
	}
	queries.SetCipher(c)
	t.Cleanup(func() {
		disabled, _ := crypto.New("")
		queries.SetCipher(disabled)
	})
}

// There are no default credentials any more: adding a device without a
// username or a password is refused before anything is dialled.
func TestCreateDeviceNeedsItsOwnCredentials(t *testing.T) {
	routeros.SetDialPolicy(nil)
	s := &Server{db: newTestDB(t), cfg: &config.Config{DefaultROSPort: 8728}}
	h := devicesRouter(s)
	for name, body := range map[string]map[string]any{
		"no credentials": {"address": "192.0.2.10"},
		"no password":    {"address": "192.0.2.10", "username": "admin"},
		"no username":    {"address": "192.0.2.10", "password": "secret"},
	} {
		rec := doDevice(t, h, http.MethodPost, "/devices", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
}

// Editing a device stores a password only when the edit carries one, and
// drops the pooled session whenever what it was dialled with changes — an
// edit of notes or tags leaves both alone.
func TestUpdateDeviceSecretsAndPooledSession(t *testing.T) {
	routeros.SetDialPolicy(nil)
	installCipher(t, "unit-test-key")
	db := newTestDB(t)
	dev := &queries.Device{ID: "d1", Address: "10.0.0.1", Identity: "sw", Username: "admin", PasswordEnc: "router-pass", APIPort: 8728, Status: "online", Tags: "[]"}
	if err := queries.CreateDevice(db, dev); err != nil {
		t.Fatal(err)
	}
	pool := routeros.NewPool(false)
	fake := routerostest.NewDevice(t)
	s := &Server{db: db, cfg: &config.Config{}, pool: pool}
	h := devicesRouter(s)
	original := storedSecret(t, s, "d1")

	// Notes only: the stored password and the pooled session stay.
	fake.Dial(t, pool, "d1")
	if rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"notes": "rack 3"}); rec.Code != http.StatusOK {
		t.Fatalf("edit notes = %d %s", rec.Code, rec.Body.String())
	}
	if got := storedSecret(t, s, "d1"); got != original {
		t.Fatalf("an edit without a password rewrote it: %q → %q", original, got)
	}
	if pool.Get("d1") == nil {
		t.Fatal("an edit of the notes dropped the pooled session")
	}

	// A new address: the session to the old one must go.
	if rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"address": "10.0.0.2"}); rec.Code != http.StatusOK {
		t.Fatalf("edit address = %d %s", rec.Code, rec.Body.String())
	}
	if pool.Get("d1") != nil || pool.GetLive("d1") != nil {
		t.Fatal("the pool kept the session (or the dial parameters) of the old address")
	}
	if got := storedSecret(t, s, "d1"); got != original {
		t.Fatalf("an address edit rewrote the password: %q → %q", original, got)
	}

	// A new password: stored encrypted, session dropped.
	fake.Dial(t, pool, "d1")
	if rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"password": "new-pass"}); rec.Code != http.StatusOK {
		t.Fatalf("edit password = %d %s", rec.Code, rec.Body.String())
	}
	raw := storedSecret(t, s, "d1")
	if raw == original || !crypto.IsEncrypted(raw) {
		t.Fatalf("new password not stored encrypted: %q", raw)
	}
	if got, _ := queries.GetDevice(db, "d1"); got.PasswordEnc != "new-pass" {
		t.Fatalf("new password reads back as %q", got.PasswordEnc)
	}
	if pool.Get("d1") != nil {
		t.Fatal("the pool kept the session opened with the old password")
	}

	// Deleting the device forgets it in the pool too.
	fake.Dial(t, pool, "d1")
	if rec := doDevice(t, h, http.MethodDelete, "/devices/d1", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if pool.Get("d1") != nil || pool.GetLive("d1") != nil {
		t.Fatal("the pool kept a session for a deleted device")
	}
}

// Under a wrong encryption key an edit must not destroy the stored
// password, and re-entering the password makes the device usable again.
func TestUpdateDeviceUnderAWrongKey(t *testing.T) {
	routeros.SetDialPolicy(nil)
	installCipher(t, "the-right-key")
	db := newTestDB(t)
	dev := &queries.Device{ID: "d1", Address: "10.0.0.1", Username: "admin", PasswordEnc: "router-pass", APIPort: 8728, Status: "online", Tags: "[]"}
	if err := queries.CreateDevice(db, dev); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, cfg: &config.Config{}}
	h := devicesRouter(s)
	original := storedSecret(t, s, "d1")

	installCipher(t, "a-different-key")
	if rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"notes": "edited"}); rec.Code != http.StatusOK {
		t.Fatalf("edit under the wrong key = %d %s", rec.Code, rec.Body.String())
	}
	if got := storedSecret(t, s, "d1"); got != original {
		t.Fatalf("an edit under the wrong key destroyed the stored password: %q → %q", original, got)
	}
	if rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"password": "typed-again"}); rec.Code != http.StatusOK {
		t.Fatalf("re-entering the password = %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := queries.GetDevice(db, "d1"); got.PasswordEnc != "typed-again" {
		t.Fatalf("after re-entering the password it reads as %q", got.PasswordEnc)
	}
}
