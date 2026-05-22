package opnsense

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetLeases_MergesV4AndV6_FiltersInactive_UppercasesMAC(t *testing.T) {
	gotPaths := map[string]bool{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/kea/leases4/search", func(w http.ResponseWriter, r *http.Request) {
		gotPaths[r.URL.Path] = true
		if got := r.Header.Get("Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("k:s")) {
			t.Errorf("unexpected auth header: %q", got)
		}
		_, _ = w.Write([]byte(`{"total":2,"rows":[
			{"address":"192.0.2.10","hwaddr":"aa:bb:cc:dd:ee:01","hostname":"a","state":"active","subnet_id":1,"type":"v4"},
			{"address":"192.0.2.11","hwaddr":"aa:bb:cc:dd:ee:02","hostname":"expired","state":"expired","subnet_id":1,"type":"v4"}
		]}`))
	})
	mux.HandleFunc("/api/kea/leases6/search", func(w http.ResponseWriter, r *http.Request) {
		gotPaths[r.URL.Path] = true
		_, _ = w.Write([]byte(`{"total":1,"rows":[
			{"address":"2001:db8::1","hwaddr":"aa:bb:cc:dd:ee:03","hostname":"v6host","state":"active","subnet_id":2,"type":"v6"}
		]}`))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Config{URL: srv.URL, APIKey: "k", APISecret: "s"})
	if c == nil {
		t.Fatal("New returned nil with full config")
	}

	leases, err := c.GetLeases()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gotPaths["/api/kea/leases4/search"] || !gotPaths["/api/kea/leases6/search"] {
		t.Errorf("expected both endpoints hit, got %v", gotPaths)
	}
	if len(leases) != 2 {
		t.Fatalf("expected 2 active leases (1 v4 active + 1 v6 active, 1 expired dropped); got %d: %+v", len(leases), leases)
	}
	for _, l := range leases {
		if l.HWAddress != strings.ToUpper(l.HWAddress) {
			t.Errorf("MAC not uppercased: %q", l.HWAddress)
		}
	}
}

func TestNew_ReturnsNilOnMissingConfig(t *testing.T) {
	cases := []Config{
		{},
		{URL: "http://x"},
		{URL: "http://x", APIKey: "k"},
		{APIKey: "k", APISecret: "s"},
	}
	for i, cfg := range cases {
		if New(cfg) != nil {
			t.Errorf("case %d: expected nil, got non-nil for %+v", i, cfg)
		}
	}
}

func TestGetLeases_PartialFailureReturnsWhatWeGot(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/kea/leases4/search", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total":1,"rows":[
			{"address":"192.0.2.10","hwaddr":"AA:BB:CC:DD:EE:01","hostname":"a","state":"active","type":"v4"}
		]}`))
	})
	mux.HandleFunc("/api/kea/leases6/search", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Config{URL: srv.URL, APIKey: "k", APISecret: "s"})
	leases, err := c.GetLeases()
	if err == nil {
		t.Fatalf("expected error reporting the v6 failure")
	}
	if !strings.Contains(err.Error(), "leases6") {
		t.Errorf("error should mention which endpoint failed: %v", err)
	}
	if len(leases) != 1 {
		t.Errorf("expected 1 v4 lease preserved despite v6 failure, got %d", len(leases))
	}
}
