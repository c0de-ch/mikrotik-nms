package queries

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedUser inserts a user row so reset tokens satisfy the FK, returning its id.
func seedUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	id := uuid.NewString()
	if err := CreateUser(db, &User{ID: id, Username: id + "@example.com", PasswordHash: "h", Role: "viewer"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// issue is a small helper that issues a token with the given TTL and returns its hash.
func issue(t *testing.T, db *sql.DB, userID string, ttl time.Duration) string {
	t.Helper()
	hash := "hash-" + uuid.NewString()
	if err := IssueResetToken(db, userID, hash, time.Now().Add(ttl)); err != nil {
		t.Fatalf("IssueResetToken: %v", err)
	}
	return hash
}

// consume runs ConsumeResetToken inside a transaction (the production caller
// always uses a tx) and returns its result.
func consume(t *testing.T, db *sql.DB, hash string) (string, bool) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	uid, ok, err := ConsumeResetToken(tx, hash)
	if err != nil {
		tx.Rollback()
		t.Fatalf("ConsumeResetToken: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return uid, ok
}

func TestConsumeResetTokenSucceedsOnce(t *testing.T) {
	db := testDB(t)
	userID := seedUser(t, db)
	hash := issue(t, db, userID, time.Hour)

	uid, ok := consume(t, db, hash)
	if !ok {
		t.Fatal("first consume should succeed")
	}
	if uid != userID {
		t.Errorf("consumed user id = %q, want %q", uid, userID)
	}

	// Second consume of the same hash must fail (single-use).
	if _, ok := consume(t, db, hash); ok {
		t.Fatal("second consume of the same token must fail")
	}
}

func TestConsumeExpiredToken(t *testing.T) {
	db := testDB(t)
	userID := seedUser(t, db)
	hash := issue(t, db, userID, -time.Minute) // already expired

	if _, ok := consume(t, db, hash); ok {
		t.Fatal("expired token must not be consumable")
	}
}

func TestConsumeUnknownToken(t *testing.T) {
	db := testDB(t)
	_ = seedUser(t, db)

	if _, ok := consume(t, db, "no-such-hash"); ok {
		t.Fatal("unknown token must not be consumable")
	}
}

func TestSupersedeInvalidatesPriorTokens(t *testing.T) {
	db := testDB(t)
	userID := seedUser(t, db)

	first := issue(t, db, userID, time.Hour)
	// Issuing a second token supersedes the first (IssueResetToken does this).
	second := issue(t, db, userID, time.Hour)

	if _, ok := consume(t, db, first); ok {
		t.Fatal("first (superseded) token must no longer validate")
	}
	uid, ok := consume(t, db, second)
	if !ok {
		t.Fatal("newest token must still validate")
	}
	if uid != userID {
		t.Errorf("user id = %q, want %q", uid, userID)
	}
}

func TestSupersedePendingResetTokensExplicit(t *testing.T) {
	db := testDB(t)
	userID := seedUser(t, db)
	hash := issue(t, db, userID, time.Hour)

	if err := SupersedePendingResetTokens(db, userID); err != nil {
		t.Fatalf("SupersedePendingResetTokens: %v", err)
	}
	if _, ok := consume(t, db, hash); ok {
		t.Fatal("token must not validate after explicit supersede")
	}
}

func TestDeleteExpiredResetTokens(t *testing.T) {
	db := testDB(t)
	userID := seedUser(t, db)

	// One already-expired token (well past the cutoff) and one still valid.
	expiredHash := "expired-" + uuid.NewString()
	if err := IssueResetToken(db, userID, expiredHash, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("issue expired: %v", err)
	}
	validHash := issue(t, db, userID, time.Hour)

	cutoff := time.Now().Add(-24 * time.Hour)
	n, err := DeleteExpiredResetTokens(db, cutoff)
	if err != nil {
		t.Fatalf("DeleteExpiredResetTokens: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted %d rows, want 1 (only the long-expired token)", n)
	}

	// The valid token survives.
	if _, ok := consume(t, db, validHash); !ok {
		t.Fatal("valid token should survive cleanup")
	}
}
