package queries

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A device's TLS pin is stored with it, listed with it, changed by
// UpdateDevice and cleared by an empty value; a fingerprint is not a secret
// and is part of the device's JSON.
func TestDevicePinRoundTrip(t *testing.T) {
	db := testDB(t)
	pin := strings.Repeat("0a", 32)

	withPin := &Device{ID: uuid.NewString(), Address: "192.0.2.10", Username: "nms", PasswordEnc: "p",
		UseTLS: true, APIPort: 8729, PinCertSHA256: pin, Status: "online", Tags: "[]"}
	plain := &Device{ID: uuid.NewString(), Address: "192.0.2.11", Username: "nms", PasswordEnc: "p",
		APIPort: 8728, Status: "online", Tags: "[]"}
	for _, d := range []*Device{withPin, plain} {
		if err := CreateDevice(db, d); err != nil {
			t.Fatal(err)
		}
	}

	got, err := GetDevice(db, withPin.ID)
	if err != nil || got.PinCertSHA256 != pin {
		t.Fatalf("GetDevice pin = %q, %v; want %q", got.PinCertSHA256, err, pin)
	}
	if got, _ := GetDevice(db, plain.ID); got.PinCertSHA256 != "" {
		t.Fatalf("a device added without a pin has %q", got.PinCertSHA256)
	}
	devices, err := ListDevices(db)
	if err != nil || len(devices) != 2 {
		t.Fatalf("ListDevices = %d, %v", len(devices), err)
	}
	for _, d := range devices {
		if want := map[string]string{withPin.ID: pin, plain.ID: ""}[d.ID]; d.PinCertSHA256 != want {
			t.Errorf("listed pin of %s = %q, want %q", d.Address, d.PinCertSHA256, want)
		}
	}

	raw, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(raw), `"pin_cert_sha256":"`+pin+`"`) {
		t.Fatalf("device JSON = %s, %v; want the pin in it", raw, err)
	}

	// Changed, then cleared, by an edit.
	other := strings.Repeat("0b", 32)
	got.PinCertSHA256 = other
	if err := UpdateDevice(db, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := GetDevice(db, withPin.ID); again.PinCertSHA256 != other {
		t.Fatalf("pin after the edit = %q, want %q", again.PinCertSHA256, other)
	}
	got.PinCertSHA256 = ""
	if err := UpdateDevice(db, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := GetDevice(db, withPin.ID); again.PinCertSHA256 != "" {
		t.Fatalf("pin after clearing = %q, want none", again.PinCertSHA256)
	}
	// An empty pin is stored as NULL, not as an empty string.
	var nulls int
	if err := db.QueryRow(`SELECT COUNT(*) FROM devices WHERE pin_cert_sha256 IS NULL`).Scan(&nulls); err != nil || nulls != 2 {
		t.Fatalf("devices with a NULL pin = %d, %v; want 2", nulls, err)
	}

	n, err := CountTLSDevicesWithoutPin(db)
	if err != nil || n != 1 {
		t.Fatalf("CountTLSDevicesWithoutPin = %d, %v; want 1 (the TLS device whose pin was cleared)", n, err)
	}
}
