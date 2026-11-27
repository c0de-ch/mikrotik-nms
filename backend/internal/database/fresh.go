package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// The bytes of a database migrated to the current schema, built once per
// process.
var (
	templateOnce sync.Once
	templateDB   []byte
	templateErr  error
)

// OpenFresh opens a new, empty, fully migrated database at dbPath, which must
// not exist yet. It is Open for tests: replaying every migration costs each
// test a noticeable fraction of a second under the race detector, and there
// are hundreds of them, so the first call in a process migrates one database
// and every call copies that file. The result is indistinguishable from
// Open(dbPath) on a new path; code that tests the migrations themselves, or
// opens an existing database, uses Open.
func OpenFresh(dbPath string) (*sql.DB, error) {
	templateOnce.Do(func() { templateDB, templateErr = buildTemplate() })
	if templateErr != nil {
		return nil, fmt.Errorf("migrated template: %w", templateErr)
	}
	// O_EXCL: never overwrite a database that is already there.
	f, err := os.OpenFile(dbPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	if _, err := f.Write(templateDB); err != nil {
		f.Close()
		return nil, fmt.Errorf("write database: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("write database: %w", err)
	}
	return Open(dbPath)
}

// buildTemplate migrates a database in a scratch directory and returns the
// file's bytes, with the write-ahead log folded back into it.
func buildTemplate() ([]byte, error) {
	dir, err := os.MkdirTemp("", "nms-db-template-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "template.db")
	db, err := Open(path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}
