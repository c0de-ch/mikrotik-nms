package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

// asPasted is a fingerprint the way an admin copies it out of WinBox: upper
// case, a colon between the bytes.
func asPasted(fp string) string {
	var bytes []string
	for i := 0; i < len(fp); i += 2 {
		bytes = append(bytes, strings.ToUpper(fp[i:i+2]))
	}
	return strings.Join(bytes, ":")
}

func decodeDevice(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

func deviceCount(t *testing.T, s *Server) int {
	t.Helper()
	devices, err := queries.ListDevices(s.db)
	if err != nil {
		t.Fatal(err)
	}
	return len(devices)
}

// Adding a TLS device needs its certificate fingerprint first: without one,
// or with something that is not a fingerprint, nothing is dialled and
// nothing is stored.
func TestCreateTLSDeviceNeedsAPin(t *testing.T) {
	routeros.SetDialPolicy(nil)
	fake := routerostest.NewTLSDevice(t)
	s := &Server{db: newTestDB(t), cfg: &config.Config{DefaultROSPort: 8728}}
	h := devicesRouter(s)

	base := func(pin any) map[string]any {
		body := map[string]any{"address": fake.Host, "api_port": fake.Port, "username": "nms", "password": "secret", "use_tls": true}
		if pin != nil {
			body["pin_cert_sha256"] = pin
		}
		return body
	}
	for name, pin := range map[string]any{
		"no pin":        nil,
		"empty pin":     "",
		"not hex":       strings.Repeat("zz", 32),
		"too short":     fake.Fingerprint()[:40],
		"with a prefix": "SHA256:" + fake.Fingerprint(),
	} {
		rec := doDevice(t, h, http.MethodPost, "/devices", base(pin))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	if fake.Conns() != 0 {
		t.Fatalf("the device was contacted %d time(s) before a valid pin was given", fake.Conns())
	}
	if n := deviceCount(t, s); n != 0 {
		t.Fatalf("%d device(s) stored, want none", n)
	}
}

// With the right fingerprint — pasted in upper case with colons — the device
// is added, the pin stored in its normal form and returned.
func TestCreateTLSDeviceWithItsPin(t *testing.T) {
	routeros.SetDialPolicy(nil)
	fake := routerostest.NewTLSDevice(t)
	fake.SetReply("/system/resource/print", map[string]string{"board-name": "CRS326", "version": "7.20"})
	fake.SetReply("/system/identity/print", map[string]string{"name": "sw-tls"})
	s := &Server{db: newTestDB(t), cfg: &config.Config{DefaultROSPort: 8728}}
	h := devicesRouter(s)

	rec := doDevice(t, h, http.MethodPost, "/devices", map[string]any{
		"address": fake.Host, "api_port": fake.Port, "username": "nms", "password": "secret",
		"use_tls": true, "pin_cert_sha256": "  " + asPasted(fake.Fingerprint()) + " ",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d %s, want 201", rec.Code, rec.Body.String())
	}
	created := decodeDevice(t, rec)
	if created["pin_cert_sha256"] != fake.Fingerprint() || created["use_tls"] != true || created["identity"] != "sw-tls" {
		t.Fatalf("created = %v, want the normalised pin %s", created, fake.Fingerprint())
	}
	stored, err := queries.GetDevice(s.db, created["id"].(string))
	if err != nil || stored.PinCertSHA256 != fake.Fingerprint() {
		t.Fatalf("stored pin = %q, %v", stored.PinCertSHA256, err)
	}
	if fake.Logins() != 1 {
		t.Errorf("logins = %d, want the one connection test", fake.Logins())
	}
}

// A device that presents another certificate than the fingerprint typed for
// it is not added, and never sees the password.
func TestCreateTLSDeviceWithTheWrongPin(t *testing.T) {
	routeros.SetDialPolicy(nil)
	fake := routerostest.NewTLSDevice(t)
	s := &Server{db: newTestDB(t), cfg: &config.Config{DefaultROSPort: 8728}}
	h := devicesRouter(s)

	rec := doDevice(t, h, http.MethodPost, "/devices", map[string]any{
		"address": fake.Host, "api_port": fake.Port, "username": "nms", "password": "secret",
		"use_tls": true, "pin_cert_sha256": strings.Repeat("ab", 32),
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "pin mismatch") {
		t.Fatalf("status = %d %s, want 400 naming the pin mismatch", rec.Code, rec.Body.String())
	}
	if fake.Logins() != 0 {
		t.Fatalf("logins = %d: the password was sent to a device with the wrong certificate", fake.Logins())
	}
	if n := deviceCount(t, s); n != 0 {
		t.Fatalf("%d device(s) stored, want none", n)
	}
}

// A plain-API device is added as before; a fingerprint given with it is kept
// for the day TLS is switched on.
func TestCreatePlainDeviceNeedsNoPin(t *testing.T) {
	routeros.SetDialPolicy(nil)
	fake := routerostest.NewDevice(t)
	s := &Server{db: newTestDB(t), cfg: &config.Config{DefaultROSPort: 8728}}
	h := devicesRouter(s)

	rec := doDevice(t, h, http.MethodPost, "/devices", map[string]any{
		"address": fake.Host, "api_port": fake.Port, "username": "nms", "password": "secret",
	})
	if rec.Code != http.StatusCreated || decodeDevice(t, rec)["pin_cert_sha256"] != "" {
		t.Fatalf("plain device: %d %s, want 201 without a pin", rec.Code, rec.Body.String())
	}
}

// Editing the pin: stored normalised, refused when malformed, cleared by an
// empty value, left alone when the edit does not mention it — and whenever
// it changes the pooled session and its dial parameters go, like on an
// address change.
func TestUpdateDevicePin(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := newTestDB(t)
	dev := &queries.Device{ID: "d1", Address: "192.0.2.10", Identity: "sw", Username: "nms", PasswordEnc: "p", APIPort: 8728, Status: "online", Tags: "[]"}
	if err := queries.CreateDevice(db, dev); err != nil {
		t.Fatal(err)
	}
	pool := routeros.NewPool()
	fake := routerostest.NewDevice(t)
	s := &Server{db: db, cfg: &config.Config{}, pool: pool}
	h := devicesRouter(s)
	pinA, pinB := strings.Repeat("0a", 32), strings.Repeat("0b", 32)
	stored := func() string {
		t.Helper()
		d, err := queries.GetDevice(db, "d1")
		if err != nil {
			t.Fatal(err)
		}
		return d.PinCertSHA256
	}

	// A pin is set: stored in lower case without separators, session dropped.
	fake.Dial(t, pool, "d1")
	rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"pin_cert_sha256": asPasted(pinA)})
	if rec.Code != http.StatusOK || decodeDevice(t, rec)["pin_cert_sha256"] != pinA || stored() != pinA {
		t.Fatalf("set pin: %d %s (stored %q), want %s", rec.Code, rec.Body.String(), stored(), pinA)
	}
	if pool.Get("d1") != nil || pool.GetLive("d1") != nil {
		t.Fatal("a new pin kept the pooled session (or its dial parameters)")
	}

	// The same pin again, or an edit that does not mention it: nothing to drop.
	fake.Dial(t, pool, "d1")
	for name, body := range map[string]map[string]any{
		"same pin":      {"pin_cert_sha256": strings.ToUpper(pinA)},
		"notes only":    {"notes": "rack 3"},
		"explicit null": {"pin_cert_sha256": nil, "notes": "rack 4"},
	} {
		if rec := doDevice(t, h, http.MethodPut, "/devices/d1", body); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
		if stored() != pinA {
			t.Fatalf("%s: the pin became %q", name, stored())
		}
		if pool.Get("d1") == nil {
			t.Fatalf("%s: an edit that did not change the pin dropped the pooled session", name)
		}
	}

	// A malformed pin is refused and changes nothing.
	for _, bad := range []string{"abc", strings.Repeat("zz", 32), pinA + "00"} {
		rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"pin_cert_sha256": bad, "notes": "must not be saved"})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("pin %q: %d %s, want 400", bad, rec.Code, rec.Body.String())
		}
	}
	if d, _ := queries.GetDevice(db, "d1"); d.PinCertSHA256 != pinA || d.Notes == "must not be saved" || pool.Get("d1") == nil {
		t.Fatalf("a refused edit changed the device: pin %q notes %q", d.PinCertSHA256, d.Notes)
	}

	// Another pin, then none: each change drops the session.
	if rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"pin_cert_sha256": pinB}); rec.Code != http.StatusOK || stored() != pinB {
		t.Fatalf("change pin: %d %s (stored %q)", rec.Code, rec.Body.String(), stored())
	}
	if pool.Get("d1") != nil {
		t.Fatal("a changed pin kept the pooled session")
	}
	fake.Dial(t, pool, "d1")
	if rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"pin_cert_sha256": ""}); rec.Code != http.StatusOK || stored() != "" {
		t.Fatalf("clear pin: %d %s (stored %q)", rec.Code, rec.Body.String(), stored())
	}
	if pool.Get("d1") != nil {
		t.Fatal("a cleared pin kept the pooled session")
	}
}

// An edit that does not mention use_tls leaves the flag alone: omitting it
// must not quietly turn a TLS device into one that is logged in to in the
// clear.
func TestUpdateDeviceKeepsTLSWhenNotMentioned(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := newTestDB(t)
	dev := &queries.Device{ID: "d1", Address: "192.0.2.10", Username: "nms", PasswordEnc: "p", UseTLS: true,
		PinCertSHA256: strings.Repeat("0a", 32), APIPort: 8729, Status: "online", Tags: "[]"}
	if err := queries.CreateDevice(db, dev); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, cfg: &config.Config{}, pool: routeros.NewPool()}
	h := devicesRouter(s)

	if rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"notes": "rack 3"}); rec.Code != http.StatusOK {
		t.Fatalf("edit notes = %d %s", rec.Code, rec.Body.String())
	}
	if d, _ := queries.GetDevice(db, "d1"); !d.UseTLS || d.Notes != "rack 3" {
		t.Fatalf("after an edit of the notes: use_tls=%v notes=%q, want TLS kept", d.UseTLS, d.Notes)
	}
	if rec := doDevice(t, h, http.MethodPut, "/devices/d1", map[string]any{"use_tls": false}); rec.Code != http.StatusOK {
		t.Fatalf("switch TLS off = %d %s", rec.Code, rec.Body.String())
	}
	if d, _ := queries.GetDevice(db, "d1"); d.UseTLS {
		t.Fatal("an explicit use_tls=false was not stored")
	}
}

// A pin refusal reaches the caller of an on-demand action as a 409 that
// says what to do, not as a generic failure.
func TestWriteDialPolicyErrorPinMissing(t *testing.T) {
	rec := httptest.NewRecorder()
	if !writeDialPolicyError(rec, routeros.ErrPinMissing) {
		t.Fatal("ErrPinMissing was not handled")
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "TLS pin missing") {
		t.Fatalf("status = %d %s, want 409 naming the missing pin", rec.Code, rec.Body.String())
	}
}
