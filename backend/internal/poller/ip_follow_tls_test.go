package poller

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// tlsMoveFixture is a TLS device polled at an address nothing answers at
// (192.0.2.56) and sighted at the loopback address, where fake listens. The
// device is pinned to whatever pin returns for fake.
func tlsMoveFixture(t *testing.T, pin func(fake *routerostest.Device) string) (*sql.DB, *routerostest.Device, AddressMove) {
	t.Helper()
	routeros.SetDialPolicy(nil)
	db := pollerTestDB(t)
	t.Cleanup(InstallRouterOSHooks(db))
	fake := routerostest.NewTLSDevice(t)
	fake.Reply("/system/resource/print", map[string]string{"board-name": "cAP ax", "version": "7.16"})
	mustCreateDevice(t, db, &queries.Device{
		ID: "d1", Identity: "ap02", Address: "192.0.2.56", Board: "cAP ax", ROSVersion: "7.16",
		Username: "admin", PasswordEnc: "secret", APIPort: fake.Port, UseTLS: true, PinCertSHA256: pin(fake),
	})
	return db, fake, AddressMove{DeviceID: "d1", OldAddr: "192.0.2.56", NewAddr: "127.0.0.1"}
}

func pinFindings(t *testing.T, db *sql.DB, deviceID string) []string {
	t.Helper()
	findings, err := queries.ListDeviceFindings(db, deviceID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range findings {
		if f.Kind == queries.FindingTLSPinMismatch {
			out = append(out, f.Detail)
		}
	}
	return out
}

// An approved move of a TLS device is verified over a pinned session: where
// the device's own certificate answers, the NMS logs in, checks the identity
// and commits.
func TestVerifyAndCommitAddressMove_PinnedTLSCommits(t *testing.T) {
	db, fake, mv := tlsMoveFixture(t, func(f *routerostest.Device) string { return f.Fingerprint() })
	if _, err := VerifyAndCommitAddressMove(db, routeros.NewPool(), ws.NewHub(), mv); err != nil {
		t.Fatalf("verify: %v", err)
	}
	assertAddr(t, db, "d1", "127.0.0.1")
	if fake.Logins() != 1 {
		t.Errorf("logins = %d, want the one verification login", fake.Logins())
	}
	if got := pinFindings(t, db, "d1"); len(got) != 0 {
		t.Errorf("findings = %v, want none", got)
	}
}

// Where another certificate answers at the proposed address the stored
// credential stays here: the move is refused, the address unchanged, and the
// mismatch is the device's finding.
func TestVerifyAndCommitAddressMove_PinMismatchSendsNoCredential(t *testing.T) {
	db, fake, mv := tlsMoveFixture(t, func(*routerostest.Device) string { return strings.Repeat("ab", 32) })
	_, err := VerifyAndCommitAddressMove(db, routeros.NewPool(), ws.NewHub(), mv)
	var me *MoveError
	if !errors.As(err, &me) || me.Category != MoveDialFailed || !errors.Is(err, routeros.ErrPinMismatch) {
		t.Fatalf("err = %v, want a refused move over the pin mismatch", err)
	}
	if fake.Logins() != 0 {
		t.Fatalf("logins = %d: the stored credential was sent to a host with another certificate", fake.Logins())
	}
	assertAddr(t, db, "d1", "192.0.2.56")
	if got := pinFindings(t, db, "d1"); len(got) != 1 || got[0] != fake.Fingerprint() {
		t.Fatalf("findings = %v, want the fingerprint presented at the proposed address", got)
	}
}

// A TLS device without a pin is not moved either: nothing is dialled.
func TestVerifyAndCommitAddressMove_NoPinNoDial(t *testing.T) {
	db, fake, mv := tlsMoveFixture(t, func(*routerostest.Device) string { return "" })
	if _, err := VerifyAndCommitAddressMove(db, routeros.NewPool(), ws.NewHub(), mv); !errors.Is(err, routeros.ErrPinMissing) {
		t.Fatalf("err = %v, want ErrPinMissing", err)
	}
	if fake.Conns() != 0 {
		t.Fatalf("connections = %d, want none", fake.Conns())
	}
	assertAddr(t, db, "d1", "192.0.2.56")
}
