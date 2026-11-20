package queries

import (
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/mikrotik-nms/backend/internal/crypto"
)

// deviceCipher protects device passwords at rest. It starts disabled; main
// installs the real one with SetCipher. A disabled cipher does not mean
// "store plaintext": that needs AllowPlaintextSecrets.
var deviceCipher, _ = crypto.New("")

// allowPlaintext lets a device password be stored unencrypted when no key is
// configured: a development setting, refused on an instance that polls.
var allowPlaintext atomic.Bool

// ErrSecretStorage matches (errors.Is) every refusal to store a device
// password: a missing key, a failing encryption, an unusable value.
var ErrSecretStorage = errors.New("cannot store device credentials")

// ErrNoEncryptionKey is returned when a device password would have to be
// stored but no encryption key is configured.
var ErrNoEncryptionKey = fmt.Errorf("%w: MIKROTIK_NMS_ENCRYPTION_KEY is not set", ErrSecretStorage)

// SetCipher installs the process-wide cipher used to encrypt/decrypt device
// credentials. Call once at startup.
func SetCipher(c *crypto.Cipher) {
	if c != nil {
		deviceCipher = c
	}
}

// AllowPlaintextSecrets permits storing device passwords unencrypted while
// no key is configured (MIKROTIK_NMS_DEV_PLAINTEXT_SECRETS=1). It reports
// the previous setting.
func AllowPlaintextSecrets(allow bool) bool {
	return allowPlaintext.Swap(allow)
}

// EncryptionEnabled reports whether secret encryption is active.
func EncryptionEnabled() bool { return deviceCipher.Enabled() }

// encryptSecret returns what to store for a device password. It never falls
// back to plaintext: without a key it fails (unless plaintext storage was
// allowed explicitly), and so does a failing encryption.
func encryptSecret(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if plaintext == crypto.Unreadable {
		// The stand-in for a secret we could not read must never be written
		// back over the real one.
		return "", fmt.Errorf("%w: the current ones could not be decrypted", ErrSecretStorage)
	}
	if !deviceCipher.Enabled() {
		if allowPlaintext.Load() {
			return plaintext, nil
		}
		return "", ErrNoEncryptionKey
	}
	out, err := deviceCipher.Encrypt(plaintext)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrSecretStorage, err)
	}
	return out, nil
}

// decryptSecret returns the device password, or crypto.Unreadable when the
// stored value cannot be decrypted (wrong or missing key, damaged value) —
// never an empty string, which would be tried as an empty password on the
// device at every poll.
func decryptSecret(stored string) string {
	out, err := deviceCipher.Decrypt(stored)
	if err != nil {
		return crypto.Unreadable
	}
	return out
}

// CountUnreadableDeviceSecrets returns how many devices have a stored
// password that the configured key cannot decrypt.
func CountUnreadableDeviceSecrets(db *sql.DB) (int, error) {
	rows, err := db.Query(`SELECT password_enc FROM devices WHERE password_enc != ''`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var stored string
		if err := rows.Scan(&stored); err != nil {
			return 0, err
		}
		if _, err := deviceCipher.Decrypt(stored); err != nil {
			n++
		}
	}
	return n, rows.Err()
}

// EncryptPlaintextDeviceSecrets re-stores any legacy plaintext device password
// as ciphertext. It is a no-op when encryption is disabled and is safe to run
// on every startup (idempotent).
func EncryptPlaintextDeviceSecrets(db *sql.DB) (int, error) {
	if !deviceCipher.Enabled() {
		return 0, nil
	}
	rows, err := db.Query(`SELECT id, password_enc FROM devices`)
	if err != nil {
		return 0, err
	}
	type rec struct{ id, pw string }
	var pending []rec
	for rows.Next() {
		var id, pw string
		if err := rows.Scan(&id, &pw); err != nil {
			rows.Close()
			return 0, err
		}
		if pw != "" && !crypto.IsEncrypted(pw) {
			pending = append(pending, rec{id, pw})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, r := range pending {
		enc, err := deviceCipher.Encrypt(r.pw)
		if err != nil {
			continue
		}
		if _, err := db.Exec(`UPDATE devices SET password_enc=? WHERE id=?`, enc, r.id); err == nil {
			n++
		}
	}
	return n, nil
}
