package queries

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/crypto"
	"github.com/mikrotik-nms/backend/internal/database"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// withCipher installs a cipher for the duration of a test and restores the
// disabled default afterwards (the cipher is process-global).
func withCipher(t *testing.T, key string) {
	t.Helper()
	c, err := crypto.New(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	SetCipher(c)
	t.Cleanup(func() {
		disabled, _ := crypto.New("")
		SetCipher(disabled)
	})
}

func TestCreateFirstUserIsAtomic(t *testing.T) {
	db := testDB(t)

	ok, err := CreateFirstUser(db, &User{ID: uuid.NewString(), Username: "admin", PasswordHash: "h", Role: "admin"})
	if err != nil || !ok {
		t.Fatalf("first setup should create: ok=%v err=%v", ok, err)
	}
	// A second setup with a *different* username must not create a second admin.
	ok2, err := CreateFirstUser(db, &User{ID: uuid.NewString(), Username: "attacker", PasswordHash: "h", Role: "admin"})
	if err != nil {
		t.Fatalf("second setup error: %v", err)
	}
	if ok2 {
		t.Fatal("second CreateFirstUser must not create when users already exist")
	}
	if n, _ := CountUsers(db); n != 1 {
		t.Fatalf("want exactly 1 user, got %d", n)
	}
}

func TestDevicePasswordEncryptedAtRest(t *testing.T) {
	db := testDB(t)
	withCipher(t, "unit-test-key")

	dev := &Device{
		ID:          uuid.NewString(),
		Address:     "10.0.0.1",
		Username:    "admin",
		PasswordEnc: "router-pass",
		Status:      "online",
		Tags:        "[]",
	}
	if err := CreateDevice(db, dev); err != nil {
		t.Fatalf("create device: %v", err)
	}

	// Stored column must be ciphertext, not the plaintext password.
	var raw string
	if err := db.QueryRow(`SELECT password_enc FROM devices WHERE id=?`, dev.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if !crypto.IsEncrypted(raw) {
		t.Fatalf("stored password is not encrypted: %q", raw)
	}
	if raw == "router-pass" {
		t.Fatal("stored password is plaintext")
	}

	// Reads decrypt transparently.
	got, err := GetDevice(db, dev.ID)
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if got.PasswordEnc != "router-pass" {
		t.Fatalf("decrypted password mismatch: %q", got.PasswordEnc)
	}
}

func TestEncryptPlaintextDeviceSecretsMigratesLegacyRows(t *testing.T) {
	db := testDB(t)

	// Insert a legacy plaintext row directly (cipher disabled).
	id := uuid.NewString()
	if _, err := db.Exec(
		`INSERT INTO devices (id, address, username, password_enc, status, tags) VALUES (?, ?, ?, ?, ?, ?)`,
		id, "10.0.0.2", "admin", "legacy-plain", "online", "[]",
	); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}

	// Now enable encryption and run the migration.
	withCipher(t, "unit-test-key")
	n, err := EncryptPlaintextDeviceSecrets(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 row migrated, got %d", n)
	}

	var raw string
	_ = db.QueryRow(`SELECT password_enc FROM devices WHERE id=?`, id).Scan(&raw)
	if !crypto.IsEncrypted(raw) {
		t.Fatalf("legacy row not encrypted after migration: %q", raw)
	}
	got, _ := GetDevice(db, id)
	if got.PasswordEnc != "legacy-plain" {
		t.Fatalf("migrated password should still decrypt to original: %q", got.PasswordEnc)
	}
}
