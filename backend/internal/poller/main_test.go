package poller

import (
	"database/sql"
	"os"
	"testing"

	"github.com/mikrotik-nms/backend/internal/database/dbtest"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Tests create devices with passwords but run without an encryption key.
// Production refuses that (secrets fail closed); the suite opts into
// plaintext storage the way a development instance does, and the tests of
// the refusal switch it off for themselves.
func TestMain(m *testing.M) {
	queries.AllowPlaintextSecrets(true)
	os.Exit(m.Run())
}

// pollerTestDB opens a fresh, fully migrated database for one test (a copy
// of one migrated per test binary: see dbtest).
func pollerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return dbtest.Open(t)
}
