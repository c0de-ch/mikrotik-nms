package poller

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// commandLog records which device each command was counted against.
type commandLog struct {
	mu       sync.Mutex
	byDevice map[string][]string
}

func (l *commandLog) Command(e routeros.CommandEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byDevice == nil {
		l.byDevice = map[string][]string{}
	}
	l.byDevice[e.DeviceID] = append(l.byDevice[e.DeviceID], e.Command)
}

func (l *commandLog) Session(routeros.SessionEvent) {}

func (l *commandLog) of(deviceID string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.byDevice[deviceID]...)
}

// A firmware job dials every device through the pinned dial. A device that
// presents another certificate than its pin gets neither the typed login nor
// a retry — the job does not keep knocking for its whole connect window —
// and the job goes on with the next device: one certificate says nothing
// about another device's.
func TestUpgradeJobHonoursTheTLSPin(t *testing.T) {
	shortUpgradeTimings(t)
	upgradeConnectWindow = time.Minute // a retry loop would spin here
	routeros.SetDialPolicy(nil)
	db := pollerTestDB(t)
	t.Cleanup(InstallRouterOSHooks(db))
	log := &commandLog{}
	routeros.SetObserver(log)
	t.Cleanup(func() { routeros.SetObserver(nil) })

	wrong := routerostest.NewTLSDevice(t)
	unpinned := routerostest.NewTLSDevice(t)
	good := routerostest.NewTLSDevice(t)
	good.SetReply("/system/package/update/print", map[string]string{
		"channel": "stable", "installed-version": "7.17", "latest-version": "7.17", "status": "System is already up to date",
	})
	for _, d := range []*queries.Device{
		{ID: "a", Identity: "sw-wrong", Address: wrong.Host, APIPort: wrong.Port, UseTLS: true, PinCertSHA256: strings.Repeat("ab", 32)},
		{ID: "b", Identity: "sw-unpinned", Address: "localhost", APIPort: unpinned.Port, UseTLS: true},
		{ID: "c", Identity: "sw-good", Address: "::ffff:127.0.0.1", APIPort: good.Port, UseTLS: true, PinCertSHA256: good.Fingerprint()},
	} {
		mustCreateDevice(t, db, d)
	}
	if err := queries.CreateUpgradeJob(db, &queries.UpgradeJob{ID: "job", Status: "pending", Reboot: false}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if err := queries.CreateUpgradeJobDevice(db, &queries.UpgradeJobDevice{ID: "jd-" + id, JobID: "job", DeviceID: id, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}

	NewUpgradeExecutor(db, routeros.NewPool(), ws.NewHub(), routeros.NewCredentials("fw-admin", "typed-secret")).Execute("job")

	devs, _ := queries.ListUpgradeJobDevices(db, "job")
	if len(devs) != 3 {
		t.Fatalf("job devices = %+v", devs)
	}
	byDevice := map[string]queries.UpgradeJobDevice{}
	for _, d := range devs {
		byDevice[d.DeviceID] = d
	}
	if d := byDevice["a"]; d.Status != "failed" || !strings.Contains(d.Message, "pin mismatch") {
		t.Errorf("device with another certificate = %+v, want it failed over the pin", d)
	}
	if d := byDevice["b"]; d.Status != "failed" || !strings.Contains(d.Message, "TLS pin missing") {
		t.Errorf("device without a pin = %+v, want it failed for the missing pin", d)
	}
	if d := byDevice["c"]; d.Status != "completed" {
		t.Errorf("pinned device = %+v, want it completed (the others' certificates are not its business)", d)
	}
	if wrong.Logins() != 0 || wrong.Conns() != 1 {
		t.Errorf("device with another certificate: %d login(s), %d connection(s); want no login and one handshake, not a retry loop",
			wrong.Logins(), wrong.Conns())
	}
	if unpinned.Conns() != 0 {
		t.Errorf("device without a pin was contacted %d time(s)", unpinned.Conns())
	}
	if findings, _ := queries.ListDeviceFindings(db, "a", 10); len(findings) != 1 || findings[0].Kind != queries.FindingTLSPinMismatch {
		t.Errorf("findings of the mismatching device = %+v", findings)
	}

	// The job's one-shot session is counted against its device.
	if got := log.of("c"); len(got) == 0 {
		t.Errorf("no command of the job was counted against its device: %+v", log.byDevice)
	}
	if got := log.of(""); len(got) != 0 {
		t.Errorf("commands counted against no device: %v", got)
	}
}
