package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleTestOpnsense(t *testing.T) {
	// Fake OPNsense Kea wrapper: one active v4 lease, empty v6.
	opn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "leases6") {
			_, _ = w.Write([]byte(`{"rows":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"rows":[
			{"address":"192.168.80.5","hwaddr":"aa:bb:cc:dd:ee:01","hostname":"h","state":0,"type":""}
		]}`))
	}))
	defer opn.Close()

	s := &Server{}

	post := func(body string) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/settings/opnsense/test", strings.NewReader(body))
		rec := httptest.NewRecorder()
		s.handleTestOpnsense(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return out
	}

	// Valid credentials -> ok with the active-lease count.
	out := post(`{"url":"` + opn.URL + `","api_key":"k","api_secret":"s","verify_tls":false}`)
	if out["ok"] != true {
		t.Fatalf("want ok=true, got %+v", out)
	}
	if n, _ := out["leases"].(float64); n != 1 {
		t.Fatalf("want 1 lease, got %v", out["leases"])
	}

	// Missing secret -> ok=false, no panic.
	if out := post(`{"url":"http://x","api_key":"k"}`); out["ok"] != false {
		t.Fatalf("missing secret should be ok=false, got %+v", out)
	}

	// Unreachable URL -> ok=false with an error message.
	if out := post(`{"url":"http://127.0.0.1:1","api_key":"k","api_secret":"s"}`); out["ok"] != false {
		t.Fatalf("dead URL should be ok=false, got %+v", out)
	}
}
