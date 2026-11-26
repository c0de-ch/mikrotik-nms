package queries

import (
	"os"
	"testing"
)

// Tests create devices with passwords but run without an encryption key.
// Production refuses that (secrets fail closed); the suite opts into
// plaintext storage the way a development instance does, and the tests of
// the refusal switch it off for themselves.
func TestMain(m *testing.M) {
	AllowPlaintextSecrets(true)
	os.Exit(m.Run())
}
