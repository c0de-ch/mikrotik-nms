package kea

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetLeases4(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		status      int
		wantErr     bool
		wantCount   int
		wantFirstIP string
		wantFirstMAC string
	}{
		{
			name: "two active leases, one expired",
			body: `[{"result":0,"text":"ok","arguments":{"leases":[
				{"ip-address":"10.0.0.5","hw-address":"aa:bb:cc:dd:ee:01","hostname":"a","state":0,"subnet-id":1},
				{"ip-address":"10.0.0.6","hw-address":"aa:bb:cc:dd:ee:02","hostname":"b","state":2,"subnet-id":1},
				{"ip-address":"10.0.0.7","hw-address":"aa:bb:cc:dd:ee:03","hostname":"c","state":0,"subnet-id":1}
			]}}]`,
			status:       200,
			wantCount:    2,
			wantFirstIP:  "10.0.0.5",
			wantFirstMAC: "AA:BB:CC:DD:EE:01",
		},
		{
			name:    "kea reports error",
			body:    `[{"result":1,"text":"command not supported"}]`,
			status:  200,
			wantErr: true,
		},
		{
			name:    "kea returns 500",
			body:    `internal error`,
			status:  500,
			wantErr: true,
		},
		{
			name:    "empty response array",
			body:    `[]`,
			status:  200,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					t.Errorf("expected POST, got %s", r.Method)
				}
				if ct := r.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("expected JSON content-type, got %q", ct)
				}
				body, _ := io.ReadAll(r.Body)
				var cmd command
				if err := json.Unmarshal(body, &cmd); err != nil {
					t.Errorf("invalid command JSON: %v", err)
				}
				if cmd.Command != "lease4-get-all" {
					t.Errorf("expected lease4-get-all, got %s", cmd.Command)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			leases, err := New(srv.URL).GetLeases4()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(leases) != tc.wantCount {
				t.Fatalf("count: want %d got %d", tc.wantCount, len(leases))
			}
			if leases[0].IPAddress != tc.wantFirstIP {
				t.Errorf("first IP: want %q got %q", tc.wantFirstIP, leases[0].IPAddress)
			}
			if leases[0].HWAddress != tc.wantFirstMAC {
				t.Errorf("first MAC: want %q got %q", tc.wantFirstMAC, leases[0].HWAddress)
			}
		})
	}
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	c := New("http://example/")
	if !strings.HasSuffix(c.url, "example") {
		t.Errorf("trailing slash not stripped: %q", c.url)
	}
}
