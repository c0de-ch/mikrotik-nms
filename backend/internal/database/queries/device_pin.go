package queries

import "database/sql"

// pinColumn is how a TLS pin is stored: NULL for none.
func pinColumn(pin string) any {
	if pin == "" {
		return nil
	}
	return pin
}

// CountTLSDevicesWithoutPin is how many devices are set to API-TLS and have
// no pinned certificate fingerprint: they are not logged in to until an
// admin pastes one (startup logs the count).
func CountTLSDevicesWithoutPin(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM devices WHERE use_tls AND COALESCE(pin_cert_sha256, '') = ''`,
	).Scan(&n)
	return n, err
}

// FindingTLSPinMismatch is the device_findings kind of a TLS session refused
// because the peer presented another certificate than the pinned one; the
// detail is the fingerprint it presented.
const FindingTLSPinMismatch = "tls_pin_mismatch"
