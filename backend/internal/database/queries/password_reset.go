package queries

import (
	"database/sql"
	"strings"
	"time"
)

// PasswordResetToken is a stored reset token. Only the sha256 hash of the raw
// token is ever persisted (token_hash); the raw token lives only in the email.
type PasswordResetToken struct {
	ID         int64      `json:"id"`
	UserID     string     `json:"user_id"`
	TokenHash  string     `json:"token_hash"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	ConsumedAt *time.Time `json:"consumed_at"`
}

// IssueResetToken supersedes any pending (unconsumed) tokens for the user and
// inserts a new one, all inside one transaction so requesting a new reset
// immediately voids every older link. On the astronomically unlikely token_hash
// UNIQUE collision the caller is expected to regenerate; IssueResetToken itself
// returns the raw error so the handler can retry with a fresh hash.
func IssueResetToken(db *sql.DB, userID, tokenHash string, expiresAt time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE password_reset_tokens SET consumed_at=CURRENT_TIMESTAMP
		 WHERE user_id=? AND consumed_at IS NULL`,
		userID,
	); err != nil {
		return err
	}

	if _, err := tx.Exec(
		`INSERT INTO password_reset_tokens (user_id, token_hash, expires_at)
		 VALUES (?, ?, ?)`,
		userID, tokenHash, expiresAt,
	); err != nil {
		return err
	}

	return tx.Commit()
}

// IsUniqueViolation reports whether err is a SQLite UNIQUE-constraint failure,
// so IssueResetToken callers can regenerate a token on hash collision.
func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

// ConsumeResetToken atomically marks a token consumed and returns its user id.
// The guarded UPDATE (consumed_at IS NULL AND expires_at > now) makes the token
// single-use and race-safe: only one concurrent caller sees RowsAffected==1.
// ok is false for unknown, expired, or already-consumed tokens — all collapsed
// into the same generic outcome so no state oracle leaks. Runs inside the
// caller's transaction so the consume and the password update commit together.
func ConsumeResetToken(tx *sql.Tx, tokenHash string) (userID string, ok bool, err error) {
	err = tx.QueryRow(
		`UPDATE password_reset_tokens SET consumed_at=CURRENT_TIMESTAMP
		 WHERE token_hash=? AND consumed_at IS NULL AND expires_at > CURRENT_TIMESTAMP
		 RETURNING user_id`,
		tokenHash,
	).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return userID, true, nil
}

// SupersedePendingResetTokens marks all of a user's unconsumed tokens consumed.
// IssueResetToken does this internally; this is exposed for completeness/tests.
func SupersedePendingResetTokens(db *sql.DB, userID string) error {
	_, err := db.Exec(
		`UPDATE password_reset_tokens SET consumed_at=CURRENT_TIMESTAMP
		 WHERE user_id=? AND consumed_at IS NULL`,
		userID,
	)
	return err
}

// DeleteExpiredResetTokens removes tokens whose expires_at is before the cutoff.
// Called lazily on each request-reset and periodically by the retention poller
// to bound the at-rest footprint of consumed/expired rows.
func DeleteExpiredResetTokens(db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM password_reset_tokens WHERE expires_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
