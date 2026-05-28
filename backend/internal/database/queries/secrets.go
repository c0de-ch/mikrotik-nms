package queries

import (
	"database/sql"

	"github.com/mikrotik-nms/backend/internal/crypto"
)

// deviceCipher protects device passwords at rest. It defaults to a disabled
// (pass-through) cipher so the package works before SetCipher is called and in
// tests; SetCipher installs the real one at startup.
var deviceCipher, _ = crypto.New("")

// SetCipher installs the process-wide cipher used to encrypt/decrypt device
// credentials. Call once at startup.
func SetCipher(c *crypto.Cipher) {
	if c != nil {
		deviceCipher = c
	}
}

// EncryptionEnabled reports whether secret encryption is active.
func EncryptionEnabled() bool { return deviceCipher.Enabled() }

func encryptSecret(plaintext string) string {
	out, err := deviceCipher.Encrypt(plaintext)
	if err != nil {
		// Fall back to storing the plaintext rather than losing the value;
		// the startup warning already flags a missing/invalid key.
		return plaintext
	}
	return out
}

func decryptSecret(stored string) string {
	out, err := deviceCipher.Decrypt(stored)
	if err != nil {
		// Cannot decrypt (wrong/missing key) — return empty rather than leak
		// ciphertext as if it were a usable password.
		return ""
	}
	return out
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
