package routeros

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
)

// TLS pins.
//
// A RouterOS api-ssl certificate is self-signed: no CA chain says whose it
// is. What identifies the device is the certificate itself, so an admin
// pastes its fingerprint — the SHA-256 of the certificate's DER encoding,
// which RouterOS prints as "fingerprint" in /certificate print detail — into
// the device (devices.pin_cert_sha256) and every TLS session compares the
// leaf the peer presents with it during the handshake, before anything is
// sent over the session:
//
//   - no pin: the dial is refused (ErrPinMissing) before any connection;
//   - another certificate: the handshake is aborted (PinMismatchError), no
//     login is attempted, and the mismatch is reported (OnPinMismatch).
//
// There is no trust on first use: whoever answers first at the device's
// address would pin itself and receive every later login.

// ErrPinMissing is returned by every dial of a TLS device that has no pinned
// certificate fingerprint. Nothing is sent: not even a connection is opened.
var ErrPinMissing = errors.New("TLS pin missing: paste the fingerprint of the device's api-ssl certificate (SHA-256) into the device before the NMS logs in to it over TLS")

// ErrPinMismatch matches (errors.Is) every *PinMismatchError.
var ErrPinMismatch = errors.New("the device's TLS certificate does not match the pinned fingerprint")

// PinMismatchError is a TLS handshake aborted because the peer presented a
// certificate other than the pinned one. No credentials were sent.
type PinMismatchError struct {
	Address string
	// Seen is the fingerprint of the certificate the peer presented
	// (lower-case hex SHA-256), "" when it presented none.
	Seen string
	// Pinned is the fingerprint the device is pinned to.
	Pinned string
}

func (e *PinMismatchError) Error() string {
	seen := e.Seen
	if seen == "" {
		seen = "no certificate"
	}
	return fmt.Sprintf("TLS pin mismatch at %s: the peer presented %s, the device is pinned to %s — no login was attempted", e.Address, seen, e.Pinned)
}

// Is lets errors.Is(err, ErrPinMismatch) match without the concrete type.
func (e *PinMismatchError) Is(target error) bool { return target == ErrPinMismatch }

// PinMismatch is what OnPinMismatch receives.
type PinMismatch struct {
	// DeviceID is the stored device the dial was for ("" when the caller
	// did not name one, e.g. the connection test of a device being added).
	DeviceID string
	Address  string
	Seen     string
	Pinned   string
}

var pinMismatchHook atomic.Pointer[func(PinMismatch)]

// OnPinMismatch installs the process-wide handler called for every TLS dial
// refused over its pin (nil removes it). main installs the one that records
// the device finding behind the tls_pin_mismatch alert. It runs on the
// dialling goroutine and must not block.
func OnPinMismatch(fn func(PinMismatch)) {
	if fn == nil {
		pinMismatchHook.Store(nil)
		return
	}
	pinMismatchHook.Store(&fn)
}

func reportPinMismatch(m PinMismatch) {
	if fn := pinMismatchHook.Load(); fn != nil {
		(*fn)(m)
	}
}

// CertFingerprint is the fingerprint of a certificate in its DER encoding:
// lower-case hex SHA-256, the form RouterOS prints and pins are stored in.
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// NormalizePin turns a pasted fingerprint into the stored form: lower-case
// hex without separators. It accepts upper case, surrounding white space and
// colon-separated bytes ("AB:CD:…"); "" stays "" (no pin). Anything that is
// not 64 hex digits is an error.
func NormalizePin(s string) (string, error) {
	p := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
	if p == "" {
		return "", nil
	}
	if len(p) != sha256.Size*2 {
		return "", fmt.Errorf("a certificate fingerprint is 64 hex digits (SHA-256), got %d characters", len(p))
	}
	if _, err := hex.DecodeString(p); err != nil {
		return "", errors.New("a certificate fingerprint is 64 hex digits (SHA-256)")
	}
	return p, nil
}

// pinnedTLSConfig is the TLS configuration of a session to t: the peer's
// leaf certificate must be the pinned one.
//
// InsecureSkipVerify switches off only what cannot work here — building a
// chain to a public CA and matching a host name, for a self-signed
// certificate on a device addressed by IP. It does not make the session
// unverified: with it set, crypto/tls calls VerifyConnection on every
// handshake (there is no session cache, so no resumption either), and that
// is where the certificate is checked. An empty pin never reaches this
// function (checkPin), and VerifyConnection refuses it regardless.
func pinnedTLSConfig(t Target) *tls.Config {
	pin := t.Pin
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // the leaf is pinned in VerifyConnection; see above
		VerifyConnection: func(cs tls.ConnectionState) error {
			seen := ""
			if len(cs.PeerCertificates) > 0 {
				seen = CertFingerprint(cs.PeerCertificates[0].Raw)
			}
			if pin == "" || seen == "" || subtle.ConstantTimeCompare([]byte(seen), []byte(pin)) != 1 {
				return &PinMismatchError{Address: t.Address, Seen: seen, Pinned: pin}
			}
			return nil
		},
	}
	// A device addressed by name is sent that name (SNI), as before.
	host := strings.Trim(strings.TrimSpace(t.Address), "[]")
	if _, err := netip.ParseAddr(host); err != nil {
		cfg.ServerName = host
	}
	return cfg
}

// checkPin refuses a TLS target without a (well-formed) pin before anything
// is dialled.
func checkPin(t Target) error {
	if !t.UseTLS {
		return nil
	}
	if t.Pin == "" {
		return ErrPinMissing
	}
	if p, err := NormalizePin(t.Pin); err != nil || p != t.Pin {
		return fmt.Errorf("%w (the stored fingerprint is malformed)", ErrPinMissing)
	}
	return nil
}
