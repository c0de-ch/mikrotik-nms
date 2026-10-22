package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

func TestWriteDialPolicyError(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		handled bool
		status  int
	}{
		{"nil", nil, false, 0},
		{"other", errors.New("dial tcp: connection refused"), false, 0},
		{"pollers off", routeros.ErrPollersOff, true, http.StatusServiceUnavailable},
		{"wrapped pollers off", errors.Join(errors.New("ctx"), routeros.ErrPollersOff), true, http.StatusServiceUnavailable},
		{"target not allowed", &routeros.TargetNotAllowedError{Host: "203.0.113.9", Allowed: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}, true, http.StatusForbidden},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if got := writeDialPolicyError(rec, c.err); got != c.handled {
			t.Errorf("%s: handled = %v, want %v", c.name, got, c.handled)
		}
		if c.handled && rec.Code != c.status {
			t.Errorf("%s: status = %d, want %d", c.name, rec.Code, c.status)
		}
		if !c.handled && rec.Body.Len() != 0 {
			t.Errorf("%s: wrote a body for an unhandled error", c.name)
		}
	}
}

func TestPollersModeNilSafe(t *testing.T) {
	if got := (&Server{}).pollersMode(); got != config.PollersOff {
		t.Errorf("pollersMode without a config = %q, want off", got)
	}
	s := &Server{cfg: &config.Config{Pollers: config.PollersLocal}}
	if got := s.pollersMode(); got != config.PollersLocal {
		t.Errorf("pollersMode = %q, want local", got)
	}
}
