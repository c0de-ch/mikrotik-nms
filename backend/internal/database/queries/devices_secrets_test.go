package queries

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/crypto"
	"github.com/mikrotik-nms/backend/internal/database"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.OpenFresh(t.TempDir() + "/test.db")
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

// rawSecret reads the stored password column as it is on disk.
func rawSecret(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT password_enc FROM devices WHERE id=?`, id).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	return raw
}

// Without a key a device password is refused, not stored in plaintext —
// unless plaintext storage was allowed explicitly (development).
func TestDeviceSecretsFailClosedWithoutAKey(t *testing.T) {
	db := testDB(t)
	prev := AllowPlaintextSecrets(false)
	t.Cleanup(func() { AllowPlaintextSecrets(prev) })

	dev := &Device{ID: uuid.NewString(), Address: "10.0.0.3", Username: "admin", PasswordEnc: "router-pass", Status: "online", Tags: "[]"}
	err := CreateDevice(db, dev)
	if !errors.Is(err, ErrNoEncryptionKey) || !errors.Is(err, ErrSecretStorage) {
		t.Fatalf("CreateDevice without a key = %v, want ErrNoEncryptionKey", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM devices`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("devices stored = %d (%v), want none", n, err)
	}

	// A device without a password needs no key.
	bare := &Device{ID: uuid.NewString(), Address: "10.0.0.4", Username: "admin", Status: "online", Tags: "[]"}
	if err := CreateDevice(db, bare); err != nil {
		t.Fatalf("CreateDevice without a password: %v", err)
	}
	if err := SetDevicePassword(db, bare.ID, "router-pass"); !errors.Is(err, ErrNoEncryptionKey) {
		t.Fatalf("SetDevicePassword without a key = %v, want ErrNoEncryptionKey", err)
	}
	if raw := rawSecret(t, db, bare.ID); raw != "" {
		t.Fatalf("a refused password was stored: %q", raw)
	}

	// The development opt-in stores it as it is.
	AllowPlaintextSecrets(true)
	if err := SetDevicePassword(db, bare.ID, "router-pass"); err != nil {
		t.Fatalf("SetDevicePassword with plaintext allowed: %v", err)
	}
	if raw := rawSecret(t, db, bare.ID); raw != "router-pass" {
		t.Fatalf("stored = %q, want the plaintext", raw)
	}
}

// An edit never rewrites the stored password; only SetDevicePassword does,
// and it refuses an empty one.
func TestUpdateDeviceLeavesThePasswordAlone(t *testing.T) {
	db := testDB(t)
	withCipher(t, "unit-test-key")
	dev := &Device{ID: uuid.NewString(), Address: "10.0.0.5", Username: "admin", PasswordEnc: "router-pass", Status: "online", Tags: "[]"}
	if err := CreateDevice(db, dev); err != nil {
		t.Fatal(err)
	}
	before := rawSecret(t, db, dev.ID)

	got, err := GetDevice(db, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Address, got.Notes = "10.0.0.50", "moved"
	got.PasswordEnc = "" // whatever the caller holds, it is not written
	if err := UpdateDevice(db, got); err != nil {
		t.Fatal(err)
	}
	if after := rawSecret(t, db, dev.ID); after != before {
		t.Fatalf("UpdateDevice rewrote the stored password: %q → %q", before, after)
	}
	if again, _ := GetDevice(db, dev.ID); again.Address != "10.0.0.50" || again.PasswordEnc != "router-pass" {
		t.Fatalf("after the edit: address %q password %q", again.Address, again.PasswordEnc)
	}

	if err := SetDevicePassword(db, dev.ID, ""); !errors.Is(err, ErrSecretStorage) {
		t.Fatalf("SetDevicePassword(\"\") = %v, want a refusal", err)
	}
	if err := SetDevicePassword(db, dev.ID, "new-pass"); err != nil {
		t.Fatal(err)
	}
	raw := rawSecret(t, db, dev.ID)
	if !crypto.IsEncrypted(raw) || raw == before {
		t.Fatalf("new password not stored encrypted: %q", raw)
	}
	if again, _ := GetDevice(db, dev.ID); again.PasswordEnc != "new-pass" {
		t.Fatalf("new password reads back as %q", again.PasswordEnc)
	}
}

// With the wrong key a stored password reads as the Unreadable stand-in —
// never as an empty password to try on the device — the ciphertext survives
// an edit, the stand-in can never be stored, and the startup check counts it.
func TestWrongKeyNeverLosesOrFakesAPassword(t *testing.T) {
	db := testDB(t)
	withCipher(t, "the-right-key")
	dev := &Device{ID: uuid.NewString(), Address: "10.0.0.6", Username: "admin", PasswordEnc: "router-pass", Status: "online", Tags: "[]"}
	if err := CreateDevice(db, dev); err != nil {
		t.Fatal(err)
	}
	stored := rawSecret(t, db, dev.ID)
	if n, err := CountUnreadableDeviceSecrets(db); err != nil || n != 0 {
		t.Fatalf("unreadable with the right key = %d (%v), want 0", n, err)
	}

	withCipher(t, "a-different-key")
	got, err := GetDevice(db, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordEnc != crypto.Unreadable {
		t.Fatalf("password read with the wrong key = %q, want the Unreadable stand-in", got.PasswordEnc)
	}
	if n, err := CountUnreadableDeviceSecrets(db); err != nil || n != 1 {
		t.Fatalf("unreadable with the wrong key = %d (%v), want 1", n, err)
	}

	got.Notes = "edited while the key was wrong"
	if err := UpdateDevice(db, got); err != nil {
		t.Fatal(err)
	}
	if after := rawSecret(t, db, dev.ID); after != stored {
		t.Fatalf("an edit under the wrong key changed the stored password: %q → %q", stored, after)
	}
	if err := SetDevicePassword(db, dev.ID, got.PasswordEnc); !errors.Is(err, ErrSecretStorage) {
		t.Fatalf("storing the Unreadable stand-in = %v, want a refusal", err)
	}

	// Back on the right key the password is intact.
	withCipher(t, "the-right-key")
	if again, _ := GetDevice(db, dev.ID); again.PasswordEnc != "router-pass" {
		t.Fatalf("with the right key again: %q", again.PasswordEnc)
	}

	// No key at all: also unreadable, not a pass-through of the ciphertext.
	disabled, _ := crypto.New("")
	SetCipher(disabled)
	if again, _ := GetDevice(db, dev.ID); again.PasswordEnc != crypto.Unreadable {
		t.Fatalf("an encrypted password read without a key = %q, want the Unreadable stand-in", again.PasswordEnc)
	}
}
