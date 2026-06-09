package leasesource

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// keaServer answers lease4-get-all with one active + one expired lease.
func keaServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"result":0,"text":"ok","arguments":{"leases":[
			{"ip-address":"10.0.0.5","hw-address":"aa:bb:cc:dd:ee:01","hostname":"kea-host","state":0,"subnet-id":1},
			{"ip-address":"10.0.0.6","hw-address":"aa:bb:cc:dd:ee:99","hostname":"expired","state":2,"subnet-id":1}
		]}}]`))
	}))
}

// opnsenseServer answers /api/kea/leases4/search with one active lease and
// leases6 with an empty set.
func opnsenseServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "leases6") {
			_, _ = w.Write([]byte(`{"rows":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"rows":[
			{"address":"192.168.78.107","hwaddr":"b8:37:4a:f0:aa:6a","hostname":"kim-ipadpro11m1","state":0,"type":""}
		]}`))
	}))
}

func TestFromSettings_UnionAndNormalisation(t *testing.T) {
	db := testDB(t)
	kea := keaServer(t)
	defer kea.Close()
	opn := opnsenseServer(t)
	defer opn.Close()

	mustSet(t, db, "kea_url", kea.URL)
	mustSet(t, db, "opnsense_url", opn.URL)
	mustSet(t, db, "opnsense_api_key", "key")
	mustSet(t, db, "opnsense_api_secret", "secret")

	leases := FromSettings(db)
	sort.Slice(leases, func(i, j int) bool { return leases[i].MAC < leases[j].MAC })

	// Expect exactly the two ACTIVE leases (kea active + opnsense active); the
	// expired kea lease is filtered by the kea client.
	if len(leases) != 2 {
		t.Fatalf("want 2 active leases, got %d: %+v", len(leases), leases)
	}
	// MAC uppercased, hostname + origin carried through.
	want := map[string]Lease{
		"AA:BB:CC:DD:EE:01": {MAC: "AA:BB:CC:DD:EE:01", IP: "10.0.0.5", Hostname: "kea-host", Origin: "kea"},
		"B8:37:4A:F0:AA:6A": {MAC: "B8:37:4A:F0:AA:6A", IP: "192.168.78.107", Hostname: "kim-ipadpro11m1", Origin: "opnsense"},
	}
	for _, l := range leases {
		w, ok := want[l.MAC]
		if !ok {
			t.Fatalf("unexpected lease %+v", l)
		}
		if l != w {
			t.Fatalf("lease mismatch:\n got %+v\nwant %+v", l, w)
		}
	}
}

func TestFromSettings_NoSourcesConfigured(t *testing.T) {
	db := testDB(t)
	if leases := FromSettings(db); len(leases) != 0 {
		t.Fatalf("want 0 leases with nothing configured, got %d", len(leases))
	}
}

func TestFromSettings_OneSourceErrorsDoesNotBlockOther(t *testing.T) {
	db := testDB(t)
	opn := opnsenseServer(t)
	defer opn.Close()

	// kea_url points at a dead address; opnsense still configured and works.
	mustSet(t, db, "kea_url", "http://127.0.0.1:1") // connection refused
	mustSet(t, db, "opnsense_url", opn.URL)
	mustSet(t, db, "opnsense_api_key", "key")
	mustSet(t, db, "opnsense_api_secret", "secret")

	leases := FromSettings(db)
	if len(leases) != 1 || leases[0].Origin != "opnsense" {
		t.Fatalf("want only the opnsense lease, got %+v", leases)
	}
}

func mustSet(t *testing.T, db *sql.DB, k, v string) {
	t.Helper()
	if err := queries.SetSetting(db, k, v); err != nil {
		t.Fatalf("set %s: %v", k, err)
	}
}
