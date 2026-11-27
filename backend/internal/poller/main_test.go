package poller

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Tests create devices with passwords but run without an encryption key.
// Production refuses that (secrets fail closed); the suite opts into
// plaintext storage the way a development instance does, and the tests of
// the refusal switch it off for themselves.
func TestMain(m *testing.M) {
	queries.AllowPlaintextSecrets(true)
	code := m.Run()
	if migrated.dir != "" {
		_ = os.RemoveAll(migrated.dir)
	}
	os.Exit(code)
}

// migrated is an empty database with every migration applied, made once per
// test binary. Each test gets a copy: running the migrations for every test
// is most of what a test of this package costs under the race detector.
var migrated struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func migratedTemplate() (string, error) {
	migrated.once.Do(func() {
		dir, err := os.MkdirTemp("", "poller-testdb-")
		if err != nil {
			migrated.err = err
			return
		}
		migrated.dir = dir
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
		migrated.path = path
	})
	return migrated.path, migrated.err
}

// pollerTestDB opens a fresh, fully migrated database for one test.
func pollerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	template, err := migratedTemplate()
	if err != nil {
		t.Fatalf("migrate the template database: %v", err)
	}
	path := filepath.Join(t.TempDir(), "test.db")
	if err := copyFile(template, path); err != nil {
		t.Fatalf("copy the template database: %v", err)
	}
	db, err := database.Open(path) // the migrations are a no-op here
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
