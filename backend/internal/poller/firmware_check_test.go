package poller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/firmware"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

func feedServer(t *testing.T, stable string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/NEWESTa7.stable" {
			_, _ = w.Write([]byte(stable + " 1732700000\n"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// The scheduled firmware check and "Check All" read MikroTik's feed and
// recompute from the database: a device, even one reachable through the
// pool, receives no command — in particular no check-for-updates.
func TestFirmwareRefreshSendsDevicesNothing(t *testing.T) {
	db := pollerTestDB(t)
	fake := routerostest.NewDevice(t)
	pool := routeros.NewPool(false)
	mustCreateDevice(t, db, &queries.Device{ID: "r1", Identity: "r1", Address: fake.Host, APIPort: fake.Port, Status: "online"})
	fake.Dial(t, pool, "r1")

	hub := ws.NewHub()
	var published []any
	cancel := hub.Listen("firmware.update", func(d any) { published = append(published, d) })
	defer cancel()

	c := NewFirmwareChecker(db, hub)
	srv, hits := feedServer(t, "7.17")
	c.Feed = firmware.Feed{BaseURL: srv.URL}

	// The inventory read stored the device's channel and installed version
	// before the feed was ever fetched: nothing to compare with yet.
	c.RecordDevice("r1", &routeros.FirmwareInfo{Channel: "stable", InstalledVersion: "7.16.2"}, nil)
	st, err := queries.GetFirmwareStatusByDevice(db, "r1")
	if err != nil || st.UpdateAvailable || st.LatestVersion != nil {
		t.Fatalf("before the feed: %+v %v", st, err)
	}

	fetched, err := c.Refresh(context.Background(), 24*time.Hour)
	if err != nil || !fetched {
		t.Fatalf("Refresh = %v %v", fetched, err)
	}
	st, _ = queries.GetFirmwareStatusByDevice(db, "r1")
	if !st.UpdateAvailable || st.LatestVersion == nil || *st.LatestVersion != "7.17" {
		t.Errorf("after the feed: available=%v latest=%v", st.UpdateAvailable, st.LatestVersion)
	}
	if len(published) != 1 {
		t.Errorf("firmware.update published %d times, want once", len(published))
	}

	// A second check within the hour only recomputes.
	before := hits.Load()
	if fetched, err := c.Refresh(context.Background(), 0); err != nil || fetched {
		t.Errorf("second Refresh = %v %v, want a recompute only", fetched, err)
	}
	if hits.Load() != before {
		t.Error("the feed was fetched again within FirmwareFetchMinInterval")
	}
	if cmds := fake.Commands(); len(cmds) != 0 {
		t.Errorf("the device received %v", cmds)
	}
	if len(published) != 1 {
		t.Errorf("an unchanged comparison was published again")
	}
}

// The inventory read is two read-only proplisted prints; a device without
// RouterBOARD firmware is not an error, and nothing else is sent.
func TestReadFirmwareIsReadOnly(t *testing.T) {
	fake := routerostest.NewDevice(t)
	fake.SetReply("/system/package/update/print", map[string]string{"channel": "long-term", "installed-version": "7.12.1"})
	fake.SetTrap("/system/routerboard/print", "no such command prefix")
	pool := routeros.NewPool(false)
	fake.Dial(t, pool, "chr")

	pkg, rb, err := ReadFirmware(routeros.ClientRunner{Client: pool.Get("chr")})
	if err != nil || pkg.Channel != "long-term" || pkg.InstalledVersion != "7.12.1" || rb != nil {
		t.Fatalf("ReadFirmware = %+v %+v %v", pkg, rb, err)
	}
	want := []string{"/system/package/update/print", "/system/routerboard/print"}
	if got := fake.Commands(); !slices.Equal(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i, args := range fake.Args() {
		if len(args) != 1 || !strings.HasPrefix(args[0], "=.proplist=") {
			t.Errorf("%s sent %v, want a proplist only", want[i], args)
		}
	}
}

func shortUpgradeTimings(t *testing.T) {
	t.Helper()
	oldPoll, oldWin, oldEvery, oldGrace, oldTimeout := routeros.UpdateCheckPoll, upgradeConnectWindow, upgradeRetryEvery, rebootGrace, rebootTimeout
	routeros.UpdateCheckPoll, upgradeConnectWindow, upgradeRetryEvery, rebootGrace, rebootTimeout =
		time.Millisecond, 0, time.Millisecond, time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() {
		routeros.UpdateCheckPoll, upgradeConnectWindow, upgradeRetryEvery, rebootGrace, rebootTimeout = oldPoll, oldWin, oldEvery, oldGrace, oldTimeout
	})
}

// An upgrade job without reboot downloads the update — install would reboot
// the device — on a one-shot session logged in with the job's credentials,
// which are zeroed when the job ends.
func TestUpgradeWithoutRebootDownloadsOnly(t *testing.T) {
	shortUpgradeTimings(t)
	db := pollerTestDB(t)
	fake := routerostest.NewDevice(t)
	fake.SetReply("/system/package/update/print", map[string]string{
		"channel": "stable", "installed-version": "7.16.2", "latest-version": "7.17", "status": "New version is available",
	})
	mustCreateDevice(t, db, &queries.Device{ID: "r1", Identity: "r1", Address: fake.Host, APIPort: fake.Port, Status: "online"})
	if err := queries.CreateUpgradeJob(db, &queries.UpgradeJob{ID: "job", Status: "pending", Reboot: false}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateUpgradeJobDevice(db, &queries.UpgradeJobDevice{ID: "jd1", JobID: "job", DeviceID: "r1", Status: "pending"}); err != nil {
		t.Fatal(err)
	}

	creds := routeros.NewCredentials("admin-typed", "typed-secret")
	pool := routeros.NewPool(false)
	NewUpgradeExecutor(db, pool, ws.NewHub(), creds, false).Execute("job")

	cmds := fake.Commands()
	if !slices.Contains(cmds, "/system/package/update/download") {
		t.Errorf("no download sent: %v", cmds)
	}
	if slices.Contains(cmds, "/system/package/update/install") || slices.Contains(cmds, "/system/reboot") {
		t.Errorf("a job without reboot sent %v", cmds)
	}
	if pool.Get("r1") != nil {
		t.Error("the job used the pool")
	}
	devs, _ := queries.ListUpgradeJobDevices(db, "job")
	if len(devs) != 1 || devs[0].Status != "completed" || !strings.Contains(devs[0].Message, "next reboot") {
		t.Errorf("job device = %+v", devs)
	}
	if strings.Contains(devs[0].Message, "typed-secret") {
		t.Error("the password leaked into the job row")
	}
	if _, err := routeros.DialOnceWith(fake.Host, fake.Port, creds, false, false); !errors.Is(err, routeros.ErrCredentialsCleared) {
		t.Errorf("credentials usable after the job: %v", err)
	}
}

// A refused login fails that device with a clear message, and the job does
// not try the same credentials on the next device.
func TestUpgradeLoginRefusedStopsTheJob(t *testing.T) {
	shortUpgradeTimings(t)
	db := pollerTestDB(t)
	a := routerostest.NewDevice(t)
	b := routerostest.NewDevice(t)
	a.RefuseLogins(true)
	mustCreateDevice(t, db, &queries.Device{ID: "a", Identity: "sw-a", Address: a.Host, APIPort: a.Port, Status: "online"})
	mustCreateDevice(t, db, &queries.Device{ID: "b", Identity: "sw-b", Address: "localhost", APIPort: b.Port, Status: "online"})
	if err := queries.CreateUpgradeJob(db, &queries.UpgradeJob{ID: "job", Status: "pending", Reboot: true}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := queries.CreateUpgradeJobDevice(db, &queries.UpgradeJobDevice{ID: "jd-" + id, JobID: "job", DeviceID: id, Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
	NewUpgradeExecutor(db, routeros.NewPool(false), ws.NewHub(), routeros.NewCredentials("u", "wrong"), false).Execute("job")

	devs, _ := queries.ListUpgradeJobDevices(db, "job")
	if len(devs) != 2 {
		t.Fatalf("devices = %+v", devs)
	}
	if devs[0].Status != "failed" || !strings.Contains(devs[0].Message, "login refused by sw-a") {
		t.Errorf("refused device = %+v", devs[0])
	}
	if devs[1].Status != "failed" || !strings.HasPrefix(devs[1].Message, "skipped") {
		t.Errorf("next device = %+v", devs[1])
	}
	if b.Conns() != 0 {
		t.Error("the job dialled the next device with credentials already refused")
	}
	if len(a.Commands()) != 0 {
		t.Errorf("commands after a refused login: %v", a.Commands())
	}
}

// With reboot the job installs, waits for the device and verifies that it
// came back on the new version — every session one-shot.
func TestUpgradeWithRebootInstallsAndVerifies(t *testing.T) {
	shortUpgradeTimings(t)
	db := pollerTestDB(t)
	fake := routerostest.NewDevice(t)
	fake.SetReply("/system/package/update/print", map[string]string{
		"channel": "stable", "installed-version": "7.16.2", "latest-version": "7.17", "status": "New version is available",
	})
	fake.SetReply("/system/resource/print", map[string]string{"version": "7.17 (stable)", "board-name": "RB5009"})
	mustCreateDevice(t, db, &queries.Device{ID: "r1", Identity: "r1", Address: fake.Host, APIPort: fake.Port, Status: "online"})
	if err := queries.CreateUpgradeJob(db, &queries.UpgradeJob{ID: "job", Status: "pending", Reboot: true}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateUpgradeJobDevice(db, &queries.UpgradeJobDevice{ID: "jd1", JobID: "job", DeviceID: "r1", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	NewUpgradeExecutor(db, routeros.NewPool(false), ws.NewHub(), routeros.NewCredentials("u", "p"), false).Execute("job")

	cmds := fake.Commands()
	if !slices.Contains(cmds, "/system/package/update/install") || slices.Contains(cmds, "/system/package/update/download") {
		t.Errorf("commands = %v", cmds)
	}
	devs, _ := queries.ListUpgradeJobDevices(db, "job")
	if len(devs) != 1 || devs[0].Status != "completed" || !strings.Contains(devs[0].Message, "Upgraded to 7.17") {
		t.Errorf("job device = %+v", devs)
	}
}
