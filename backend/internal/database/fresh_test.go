package database

import (
	"database/sql"
	"os"
	"sort"
	"testing"
)

// schemaOf lists every table, index and trigger with its SQL, and the goose
// version: what "the same database" means here.
func schemaOf(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type || ' ' || name || ': ' || COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	var version int64
	if err := db.QueryRow(`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return append(out, "goose version "+string(rune('0'+version/10))+string(rune('0'+version%10)))
}

// A database from OpenFresh is the database Open migrates: same schema, same
// seeded settings, same version, foreign keys enforced, and independent of
// every other one.
func TestOpenFreshEqualsOpen(t *testing.T) {
	migrated, err := Open(t.TempDir() + "/migrated.db")
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	fresh, err := OpenFresh(t.TempDir() + "/fresh.db")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	want, got := schemaOf(t, migrated), schemaOf(t, fresh)
	if len(want) != len(got) {
		t.Fatalf("schema objects: Open has %d, OpenFresh %d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("schema differs:\n Open:      %s\n OpenFresh: %s", want[i], got[i])
		}
	}
	settings := func(db *sql.DB) (n int, sum string) {
		if err := db.QueryRow(`SELECT COUNT(*), COALESCE(group_concat(key || '=' || value, ';'), '') FROM (SELECT key, value FROM app_settings ORDER BY key)`).Scan(&n, &sum); err != nil {
			t.Fatal(err)
		}
		return
	}
	wn, ws := settings(migrated)
	gn, gs := settings(fresh)
	if wn != gn || ws != gs {
		t.Errorf("seeded settings differ: Open has %d (%s), OpenFresh %d (%s)", wn, ws, gn, gs)
	}

	var fk int
	if err := fresh.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Errorf("foreign_keys on a fresh database = %d, %v; want 1", fk, err)
	}

	// Two fresh databases share nothing.
	other, err := OpenFresh(t.TempDir() + "/other.db")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := fresh.Exec(`INSERT INTO app_settings (key, value) VALUES ('only_here', '1')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := other.QueryRow(`SELECT COUNT(*) FROM app_settings WHERE key = 'only_here'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a write to one fresh database is visible in another (%d, %v)", n, err)
	}
}

// OpenFresh never overwrites a database that is already there.
func TestOpenFreshRefusesAnExistingFile(t *testing.T) {
	path := t.TempDir() + "/there.db"
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if db, err := OpenFresh(path); err == nil {
		db.Close()
		t.Fatal("OpenFresh opened over an existing file")
	}
	if b, _ := os.ReadFile(path); string(b) != "precious" {
		t.Errorf("the existing file was changed: %q", b)
	}
}
