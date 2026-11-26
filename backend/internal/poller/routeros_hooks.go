package poller

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// DialTarget is how to reach a stored device's API: its address, port and
// TLS flag with the pinned certificate fingerprint, tied to the device so
// that the session is counted against it, gets its class's deadlines, and a
// pin mismatch is recorded as its finding. Every dial of a stored device —
// pooled or one-shot — is built from it.
func DialTarget(dev queries.Device) routeros.Target {
	return routeros.Target{
		DeviceID: dev.ID,
		Address:  dev.Address,
		Port:     dev.APIPort,
		UseTLS:   dev.UseTLS,
		Pin:      dev.PinCertSHA256,
	}
}

// InstallRouterOSHooks connects the RouterOS client's two process-wide
// questions to the database:
//
//   - which devices are weak (class C1, or on the light profile): they get
//     the longer TLS handshake and command deadlines;
//   - what to do about a TLS session refused over its pin: it becomes the
//     device's tls_pin_mismatch finding, which the alert of that name fires
//     on.
//
// main calls it once, before anything dials. The returned function removes
// the hooks again (tests).
func InstallRouterOSHooks(db *sql.DB) (remove func()) {
	weak := &weakDevices{db: db, ttl: weakDevicesTTL, now: time.Now}
	routeros.SetWeakDeviceFunc(weak.is)
	routeros.OnPinMismatch(func(m routeros.PinMismatch) { recordPinMismatch(db, m) })
	return func() {
		routeros.SetWeakDeviceFunc(nil)
		routeros.OnPinMismatch(nil)
	}
}

// weakDevicesTTL bounds how long the set of weak devices is cached. The
// client asks on every command; a class, override or profile change applies
// within it.
const weakDevicesTTL = 30 * time.Second

// weakDevices answers "is this device C1?" from a cached read of the device
// list.
type weakDevices struct {
	db  *sql.DB
	ttl time.Duration
	now func() time.Time

	mu     sync.Mutex
	readAt time.Time
	set    map[string]bool
}

func (w *weakDevices) is(deviceID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if w.set == nil || now.Sub(w.readAt) >= w.ttl || now.Before(w.readAt) {
		devices, err := queries.ListDevices(w.db)
		if err != nil {
			// Keep what was known; with nothing known no device is weak.
			log.Printf("poller: device classes for the RouterOS client: %v", err)
			if w.set == nil {
				return false
			}
		} else {
			set := make(map[string]bool, len(devices))
			for i := range devices {
				// The class whose limits apply: C1 when unprobed or on the
				// light profile, like the live-view caps.
				if liveClassOf(&devices[i]) == devclass.C1 {
					set[devices[i].ID] = true
				}
			}
			w.set = set
		}
		w.readAt = now
	}
	return w.set[deviceID]
}

// recordPinMismatch stores a refused TLS session as a critical finding of
// the device, with the fingerprint that was presented as its detail (a
// repeat bumps the row). A dial that named no stored device — the connection
// test of a device being added — is only logged: the admin sees the error.
func recordPinMismatch(db *sql.DB, m routeros.PinMismatch) {
	seen := m.Seen
	if seen == "" {
		seen = "no certificate"
	}
	log.Printf("routeros: TLS pin mismatch at %s (device %q): presented %s, pinned %s — no login was attempted",
		m.Address, m.DeviceID, seen, m.Pinned)
	if m.DeviceID == "" {
		return
	}
	if err := queries.UpsertDeviceFinding(db, &queries.DeviceFinding{
		DeviceID: m.DeviceID,
		Kind:     queries.FindingTLSPinMismatch,
		Severity: "critical",
		Detail:   seen,
	}); err != nil {
		log.Printf("routeros: record the TLS pin mismatch of device %s: %v", m.DeviceID, err)
	}
}
