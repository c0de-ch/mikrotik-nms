package queries

import "database/sql"

// DBTX is what a query function needs of the database. *sql.DB and *sql.Tx
// both satisfy it, so the same write runs on its own or inside a caller's
// transaction: internal/ingest applies a whole batch of observations in one.
// The query functions take it as they are converted (M1); the others still
// take *sql.DB.
type DBTX interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

var (
	_ DBTX = (*sql.DB)(nil)
	_ DBTX = (*sql.Tx)(nil)
)
