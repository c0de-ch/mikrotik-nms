// Package dbtest opens databases for tests.
package dbtest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mikrotik-nms/backend/internal/database"
)

// Open returns a new, empty, fully migrated database for one test, closed
// when the test ends. It is database.OpenFresh — one database is migrated
// per test binary and every test gets a copy of that file — with the path
// and the cleanup taken care of.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	db, err := database.OpenFresh(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("dbtest: open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
