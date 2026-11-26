package firmware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	order := []string{"7.15", "7.16beta1", "7.16beta2", "7.16rc1", "7.16rc3", "7.16", "7.16.1", "7.16.2", "7.17beta2", "7.17rc3", "7.17", "8.0"}
	for i := range order {
		for j := range order {
			a, ok1 := ParseVersion(order[i])
			b, ok2 := ParseVersion(order[j])
			if !ok1 || !ok2 {
				t.Fatalf("parse %q / %q", order[i], order[j])
			}
			want := sign(i - j)
			if got := a.Compare(b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
	a, _ := ParseVersion("7.17")
	b, _ := ParseVersion("7.17.0")
	if a.Compare(b) != 0 {
		t.Error("7.17 and 7.17.0 must be equal")
	}
	if v, ok := ParseVersion("7.16.2 (stable)"); !ok || v.Compare(Version{Nums: []int{7, 16, 2}, Pre: 2}) != 0 {
		t.Error("the /system/resource form must parse")
	}
	for _, bad := range []string{"", "x", "7.", "7.16-rc1", "7.16rc", "v7.16", "7.16alpha1"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
}

func TestUpdateAvailable(t *testing.T) {
	for _, c := range []struct {
		installed, newest string
		want              bool
	}{
		{"7.16.2", "7.17", true},
		{"7.17rc3", "7.17", true},
		{"7.17", "7.17", false},
		{"7.18beta2", "7.17", false}, // newer than the channel: no downgrade offered
		{"", "7.17", false},
		{"7.16", "", false},
		{"garbage", "7.17", false},
	} {
		if got := UpdateAvailable(c.installed, c.newest); got != c.want {
			t.Errorf("UpdateAvailable(%q, %q) = %v, want %v", c.installed, c.newest, got, c.want)
		}
	}
}

func TestParseNewest(t *testing.T) {
	v, built, err := ParseNewest("7.16.2 1732700000\n")
	if err != nil || v != "7.16.2" || built.Unix() != 1732700000 {
		t.Errorf("got %q %v %v", v, built, err)
	}
	if v, built, err := ParseNewest("7.17rc3"); err != nil || v != "7.17rc3" || !built.IsZero() {
		t.Errorf("version only: %q %v %v", v, built, err)
	}
	for _, bad := range []string{"", "   ", "<html>", "7.16 abc", "7.16 1 2", "7.16 -5"} {
		if _, _, err := ParseNewest(bad); err == nil {
			t.Errorf("ParseNewest(%q) accepted", bad)
		}
	}
}

func TestFeedFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/routeros/NEWESTa7.stable":
			_, _ = w.Write([]byte("7.16.2 1732700000"))
		case "/routeros/NEWESTa7.testing":
			_, _ = w.Write([]byte(strings.Repeat("7", 1000)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := Feed{BaseURL: srv.URL + "/routeros/"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rel, err := f.Fetch(ctx, "stable")
	if err != nil || rel.Version != "7.16.2" || rel.Channel != "stable" {
		t.Fatalf("stable: %+v %v", rel, err)
	}
	if _, err := f.Fetch(ctx, "long-term"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("404 not reported: %v", err)
	}
	if _, err := f.Fetch(ctx, "testing"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("oversized body accepted: %v", err)
	}
	if _, err := f.Fetch(ctx, "../etc"); err == nil {
		t.Error("an unknown channel must never be put in the URL")
	}
}
