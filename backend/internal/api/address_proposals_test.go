package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// proposalFixture: a device polled at 192.0.2.56 on the fake endpoint's port
// and a pending proposal to move it to the fake endpoint's address, served by
// the proposal handlers behind an admin user.
type proposalFixture struct {
	t    *testing.T
	s    *Server
	h    http.Handler
	fake *routerostest.Device
	id   int64
}

func newProposalFixture(t *testing.T) *proposalFixture {
	t.Helper()
	db := newTestDB(t)
	fake := routerostest.NewDevice(t)
	if err := queries.CreateDevice(db, &queries.Device{
		ID: "d1", Identity: "ap1", Address: "192.0.2.56", APIPort: fake.Port,
		Username: "nms", PasswordEnc: "secret", Board: "cAP ax", ROSVersion: "7.16", Status: "offline",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertAddressProposal(db, "d1", "192.0.2.56", fake.Host, "04:F4:1C:85:97:B2", time.Now()); err != nil {
		t.Fatal(err)
	}
	ps, err := queries.ListAddressProposals(db, queries.ProposalPending, 0)
	if err != nil || len(ps) != 1 {
		t.Fatalf("seed proposal: %v %+v", err, ps)
	}
	hub := ws.NewHub()
	go hub.Run()
	s := &Server{db: db, pool: routeros.NewPool(), hub: hub, cfg: &config.Config{Pollers: config.PollersLocal}}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			u := &auth.ContextUser{ID: "u-admin", Username: "admin", Role: "admin"}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, u)))
		})
	})
	r.Get("/devices/address-proposals", s.handleListAddressProposals)
	r.Post("/devices/address-proposals/{id}/approve", s.handleApproveAddressProposal)
	r.Post("/devices/address-proposals/{id}/dismiss", s.handleDismissAddressProposal)
	return &proposalFixture{t: t, s: s, h: r, fake: fake, id: ps[0].ID}
}

func (f *proposalFixture) post(action string) (*httptest.ResponseRecorder, queries.AddressProposal) {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/devices/address-proposals/"+strconv.FormatInt(f.id, 10)+"/"+action, nil)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	var p queries.AddressProposal
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return rec, p
}

func (f *proposalFixture) address() string {
	f.t.Helper()
	d, err := queries.GetDevice(f.s.db, "d1")
	if err != nil {
		f.t.Fatal(err)
	}
	return d.Address
}

func (f *proposalFixture) auditActions() []string {
	f.t.Helper()
	es, err := queries.ListAuditLog(f.s.db, "d1", 10)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Action)
	}
	return out
}

// Approving logs in at the proposed address, verifies the device and
// re-points it; the proposal records who approved it and the audit log has it.
func TestApproveAddressProposal_VerifiesAndCommits(t *testing.T) {
	f := newProposalFixture(t)
	f.fake.Reply("/system/resource/print", map[string]string{"board-name": "cAP ax", "version": "7.16"})
	f.fake.Reply("/interface/print", map[string]string{"name": "ether1", "type": "ether", "mac-address": "04:F4:1C:85:97:B2"})

	rec, p := f.post("approve")
	if rec.Code != http.StatusOK || p.Status != queries.ProposalApproved || p.DecidedBy != "admin" || p.DecidedAt == nil {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.address(); got != f.fake.Host {
		t.Fatalf("address = %s, want %s", got, f.fake.Host)
	}
	if a := f.auditActions(); len(a) != 1 || a[0] != "address_move_approved" {
		t.Fatalf("audit = %v", a)
	}
	// Decided: a second click is a conflict, and no second login happens.
	conns := f.fake.Conns()
	if rec, _ := f.post("approve"); rec.Code != http.StatusConflict {
		t.Fatalf("second approve = %d, want 409", rec.Code)
	}
	if f.fake.Conns() != conns {
		t.Fatal("a decided proposal reached the device again")
	}
}

// A verification that fails (another board answers) is an answer, not an
// error: 200 with status failed, the address untouched.
func TestApproveAddressProposal_FailedVerification(t *testing.T) {
	f := newProposalFixture(t)
	f.fake.Reply("/system/resource/print", map[string]string{"board-name": "CRS326", "version": "7.16"})

	rec, p := f.post("approve")
	if rec.Code != http.StatusOK || p.Status != queries.ProposalFailed || p.Result == "" {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.address(); got != "192.0.2.56" {
		t.Fatalf("address changed to %s on a failed verification", got)
	}
	if a := f.auditActions(); len(a) != 1 || a[0] != "address_move_refused" {
		t.Fatalf("audit = %v", a)
	}
}

// The device's address changed since the sighting: no login, the proposal
// is superseded.
func TestApproveAddressProposal_StaleAddress(t *testing.T) {
	f := newProposalFixture(t)
	if _, err := f.s.db.Exec(`UPDATE devices SET address = '192.0.2.99' WHERE id = 'd1'`); err != nil {
		t.Fatal(err)
	}
	rec, _ := f.post("approve")
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if f.fake.Conns() != 0 {
		t.Fatal("a stale proposal reached the device")
	}
	p, _ := queries.GetAddressProposal(f.s.db, f.id)
	if p.Status != queries.ProposalSuperseded {
		t.Fatalf("status = %s, want superseded", p.Status)
	}
}

// A paused device is not contacted; the proposal stays pending.
func TestApproveAddressProposal_PausedDevice(t *testing.T) {
	f := newProposalFixture(t)
	if err := queries.PauseDevicePolling(f.s.db, "d1", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	rec, _ := f.post("approve")
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if f.fake.Conns() != 0 {
		t.Fatal("a paused device was contacted")
	}
	p, _ := queries.GetAddressProposal(f.s.db, f.id)
	if p.Status != queries.ProposalPending {
		t.Fatalf("status = %s, want pending", p.Status)
	}
}

// Dismissing contacts nothing and closes the proposal; the list then has no
// pending rows, while ?status=all still shows it.
func TestDismissAddressProposal(t *testing.T) {
	f := newProposalFixture(t)
	rec, p := f.post("dismiss")
	if rec.Code != http.StatusOK || p.Status != queries.ProposalDismissed {
		t.Fatalf("dismiss = %d %s", rec.Code, rec.Body.String())
	}
	if f.fake.Conns() != 0 {
		t.Fatal("dismiss contacted the device")
	}
	if a := f.auditActions(); len(a) != 1 || a[0] != "address_move_dismissed" {
		t.Fatalf("audit = %v", a)
	}
	for q, want := range map[string]int{"": 0, "?status=all": 1} {
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/devices/address-proposals"+q, nil))
		var ps []queries.AddressProposal
		if err := json.Unmarshal(rec.Body.Bytes(), &ps); err != nil || len(ps) != want {
			t.Errorf("GET %q = %d %s, want %d rows", q, rec.Code, rec.Body.String(), want)
		}
	}
}

// Through the real router: every user lists, only admins decide, and approve
// — the step that sends a credential to an address learned from MNDP —
// needs a password typed in the last few minutes.
func TestAddressProposalRoutesGates(t *testing.T) {
	f := newSessionFixture(t)
	if err := queries.CreateDevice(f.db, &queries.Device{ID: "d1", Identity: "ap1", Address: "192.0.2.56", APIPort: 8728,
		Username: "nms", PasswordEnc: "secret", Status: "offline"}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertAddressProposal(f.db, "d1", "192.0.2.56", "192.0.2.232", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	ps, _ := queries.ListAddressProposals(f.db, queries.ProposalPending, 0)
	base := "/devices/address-proposals/" + strconv.FormatInt(ps[0].ID, 10)

	viewer := seedUser(t, f.db, "viewer", "viewer password 123")
	if _, err := f.db.Exec(`UPDATE users SET role='viewer' WHERE id=?`, viewer.ID); err != nil {
		t.Fatal(err)
	}
	vt, err := auth.GenerateTokenPair(sessSecret, viewer.ID, viewer.Username, "viewer", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(http.MethodGet, "/devices/address-proposals", vt.AccessToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("viewer list = %d %s", rec.Code, rec.Body.String())
	}
	for _, a := range []string{"/approve", "/dismiss"} {
		if rec := f.do(http.MethodPost, base+a, vt.AccessToken, nil); rec.Code != http.StatusForbidden {
			t.Errorf("viewer POST %s = %d, want 403", a, rec.Code)
		}
	}

	u, _ := queries.GetUserByUsername(f.db, "admin")
	stale, err := auth.GenerateTokenPair(sessSecret, u.ID, u.Username, u.Role, u.TokenVersion, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(http.MethodPost, base+"/approve", stale.AccessToken, nil)
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body.Code != auth.CodeReauthRequired {
		t.Fatalf("approve with an hour-old login = %d %s, want 403 reauth_required", rec.Code, rec.Body.String())
	}
	// Dismiss contacts nothing: an older login is enough.
	if rec := f.do(http.MethodPost, base+"/dismiss", stale.AccessToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("admin dismiss = %d %s", rec.Code, rec.Body.String())
	}
}
