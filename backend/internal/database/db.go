package database

import (
	"database/sql"
	"embed"
	"fmt"
	"strings"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

func Open(dbPath string) (*sql.DB, error) {
	// Pragmas like foreign_keys and busy_timeout are PER-CONNECTION, and
	// database/sql pools connections — the Exec pragmas below only ever hit the
	// one connection that happens to serve them. The modernc driver applies
	// _pragma DSN params to EVERY new connection, so FK enforcement (which the
	// ON DELETE CASCADE tables rely on) and the busy timeout hold pool-wide.
	sep := "?"
	if strings.Contains(dbPath, "?") {
		sep = "&"
	}
	dsn := dbPath + sep + "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite pragmas for performance (kept for cache_size/temp_store, which the
	// DSN doesn't carry; the rest harmlessly re-applies on the first conn)
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA cache_size=-20000", // 20MB
		"PRAGMA foreign_keys=ON",
		"PRAGMA temp_store=MEMORY",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("exec pragma %q: %w", p, err)
		}
	}

	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return db, nil
}

func runMigrations(db *sql.DB) error {
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.Up(db, "migrations")
}
