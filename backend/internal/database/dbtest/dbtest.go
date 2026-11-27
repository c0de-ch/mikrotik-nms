// Package dbtest opens databases for tests.
//
// A test wants an empty database with every migration applied. Running the
// migrations for each test is most of what a database-backed test costs —
// over a second apiece under the race detector — so they are run once per
// test binary, and every test gets a copy of the result.
package dbtest

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mikrotik-nms/backend/internal/database"
)

// migrated is the bytes of an empty, fully migrated database file.
var migrated struct {
	once  sync.Once
	bytes []byte
	err   error
}

func template() ([]byte, error) {
	migrated.once.Do(func() {
		dir, err := os.MkdirTemp("", "nms-dbtest-")
		if err != nil {
			migrated.err = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "template.db")
		db, err := database.Open(path)
		if err != nil {
			migrated.err = err
			return
		}
		// Everything into the main file, so the copy needs nothing else.
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			db.Close()
			migrated.err = err
			return
		}
		if err := db.Close(); err != nil {
			migrated.err = err
			return
		}
		migrated.bytes, migrated.err = os.ReadFile(path)
	})
	return migrated.bytes, migrated.err
}

// Open returns a fresh, fully migrated database for one test, closed when
// the test ends. It is opened through database.Open like the real one — the
// same pragmas, and the migrations as a no-op.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	content, err := template()
	if err != nil {
		t.Fatalf("dbtest: migrate the template database: %v", err)
	}
	path := filepath.Join(t.TempDir(), "test.db")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("dbtest: copy the template database: %v", err)
	}
	db, err := database.Open(path)
	if err != nil {
		t.Fatalf("dbtest: open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
