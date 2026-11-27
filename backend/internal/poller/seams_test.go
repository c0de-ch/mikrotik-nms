package poller

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
	"github.com/mikrotik-nms/backend/internal/leasesource"
)

// emitRecorder keeps what a collect half emits.
type emitRecorder struct {
	emits [][]edgeproto.Observation
	txs   []edgeproto.StateTx
	err   error
}

func (s *emitRecorder) Emit(_ context.Context, tx edgeproto.StateTx, obs ...edgeproto.Observation) error {
	s.txs = append(s.txs, tx)
	s.emits = append(s.emits, obs)
	return s.err
}

func (s *emitRecorder) Live(edgeproto.Observation) {}

// failingConfig is a ConfigSource whose device list can be made to fail.
type failingConfig struct {
	edgeproto.ConfigSource
	fail bool
}

func (c *failingConfig) Devices() ([]edgeproto.DeviceSpec, error) {
	if c.fail {
		return nil, errors.New("database is locked")
	}
	return c.ConfigSource.Devices()
}

// The monolith's ConfigSource describes each device as the loops judged it
// before the seams: its name, liveness, pause and, for every tier, the
// interval the shared CadenceSource gives it.
func TestDBConfigDescribesTheDevicesAsTheCadenceDoes(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"r3": devclass.C3, "s1": devclass.C1, "held": devclass.C3}, "held")
	mustCreateDevice(t, db, &queries.Device{ID: "down", Address: "198.51.100.40", Status: "offline"})
	if err := queries.SetSetting(db, "client_discovery_interval", "1200"); err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: at(12, 0, 0)}
	cadence := NewCadence(db, nil)
	cfg := monolithSeams(db, nil, cadence, clock.now).Config

	specs, err := cfg.Devices()
	if err != nil {
		t.Fatal(err)
	}
	devices, err := queries.ListDevices(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != len(devices) {
		t.Fatalf("%d specs for %d devices", len(specs), len(devices))
	}
	byID := map[string]edgeproto.DeviceSpec{}
	for i, spec := range specs {
		dev := devices[i]
		if spec.ID != dev.ID {
			t.Fatalf("spec %d is %q, the device list has %q there: the order must be the list's", i, spec.ID, dev.ID)
		}
		byID[spec.ID] = spec
		for _, tier := range devclass.Tiers() {
			if got, want := spec.Cadence[tier], cadence.Interval(dev, tier, clock.now()); got != want {
				t.Errorf("%s: cadence of %s = %v, the CadenceSource says %v", dev.ID, tier, got, want)
			}
			if got, want := spec.RunInterval(tier), runInterval(cadence, dev, tier, clock.now()); got != want {
				t.Errorf("%s: run interval of %s = %v, runInterval says %v", dev.ID, tier, got, want)
			}
		}
	}

	for id, want := range map[string]struct {
		name           string
		online, paused bool
		arp, dhcp      time.Duration
	}{
		"r3":   {name: "r3", online: true, arp: 20 * time.Minute, dhcp: 20 * time.Minute},
		"s1":   {name: "s1", online: true, arp: 30 * time.Minute},
		"held": {name: "held", online: true, paused: true},
		"down": {name: "198.51.100.40", arp: 30 * time.Minute},
	} {
		spec := byID[id]
		if spec.Name != want.name || spec.Online != want.online || spec.Paused != want.paused {
			t.Errorf("%s = name %q online %v paused %v, want %q %v %v", id, spec.Name, spec.Online, spec.Paused, want.name, want.online, want.paused)
		}
		if got := spec.RunInterval(devclass.TierARP); got != want.arp {
			t.Errorf("%s: ARP runs every %v, want %v", id, got, want.arp)
		}
		if got := spec.RunInterval(devclass.TierDHCPLeases); got != want.dhcp {
			t.Errorf("%s: DHCP leases run every %v, want %v", id, got, want.dhcp)
		}
	}

	if got := cfg.Setting("client_discovery_interval"); got != "1200" {
		t.Errorf("Setting(client_discovery_interval) = %q, want 1200", got)
	}
	if got := cfg.Setting("no_such_setting"); got != "" {
		t.Errorf("Setting of a missing key = %q, want empty", got)
	}

	db.Close()
	if specs, err := cfg.Devices(); err == nil {
		t.Errorf("Devices on a closed database = %d specs and no error: a failed read must not look like an empty fleet", len(specs))
	}
}

// One pass reports every client it found, merged by MAC as the poller
// always merged them, in one Emit without a transaction of its own; a tick
// with nothing due reports nothing.
func TestClientDiscoveryReportsThePassToTheSink(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"r3": devclass.C3})
	router := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/arp/print": rep(
			map[string]string{"address": "192.0.2.10", "mac-address": "02:00:00:00:00:0a"},
			map[string]string{"address": "2001:db8::b", "mac-address": "02:00:00:00:00:0B"},
		),
		"/ip/dhcp-server/lease/print": rep(
			map[string]string{"address": "192.0.2.11", "mac-address": "02:00:00:00:00:0B", "host-name": "phone"},
			map[string]string{"active-address": "192.0.2.13", "active-mac-address": "02:00:00:00:00:0C", "host-name": "tv"},
		),
		"/ip/dhcp-server/print": rep(),
	}}
	clock := &testClock{t: at(12, 0, 0)}
	seams := monolithSeams(db, nil, NewCadence(db, nil), clock.now)
	seams.Exec = scriptedExec(map[string]*fakeRunner{"r3": router})
	sink := &emitRecorder{}
	seams.Sink = sink
	leases := func() []leasesource.Lease {
		return []leasesource.Lease{
			{MAC: "02:00:00:00:00:0A", IP: "192.0.2.10", Hostname: "laptop", Origin: "kea"},
			{MAC: "02:00:00:00:00:0D", IP: "192.0.2.14", Hostname: "printer", Origin: "opnsense"},
		}
	}
	cdp := NewClientDiscoveryPoller(seams, leases, nil, 15*time.Minute)
	cdp.now = clock.now
	cdp.clock = newTierClock(NewScheduleRegistry())
	cdp.clock.phase = noJitter

	cdp.poll(context.Background())
	if len(sink.emits) != 1 || sink.txs[0] != nil {
		t.Fatalf("a pass made %d Emits (tx %v), want one without a transaction", len(sink.emits), sink.txs)
	}
	got := map[string]edgeproto.ClientSeen{}
	var subjects []string
	for _, o := range sink.emits[0] {
		if o.Kind != edgeproto.KindClientSeen || !o.ObservedAt.Equal(at(12, 0, 0)) || o.OwnerGen != 0 || o.UID != "" {
			t.Errorf("observation %+v: want a client.seen at the pass's time, no owner generation, no UID", o)
		}
		got[o.Subject] = o.Body.(edgeproto.ClientSeen)
		subjects = append(subjects, o.Subject)
	}
	sort.Strings(subjects)
	want := map[string]edgeproto.ClientSeen{
		// Found by ARP; the external lease adds its name.
		"02:00:00:00:00:0A": {IP: "192.0.2.10", HostName: "laptop", Source: "arp", DeviceName: "r3"},
		// ARP's IPv6 address gives way to the lease's IPv4 one.
		"02:00:00:00:00:0B": {IP: "192.0.2.11", HostName: "phone", Source: "arp", DeviceName: "r3"},
		// A lease known by its active MAC and address.
		"02:00:00:00:00:0C": {IP: "192.0.2.13", HostName: "tv", Source: "dhcp", DeviceName: "r3"},
		// Only the external lease server knows it.
		"02:00:00:00:00:0D": {IP: "192.0.2.14", HostName: "printer", Source: "dhcp", DeviceName: "opnsense"},
	}
	if len(got) != len(want) || len(subjects) != len(want) {
		t.Fatalf("reported %v, want %d clients once each", subjects, len(want))
	}
	for mac, w := range want {
		if got[mac] != w {
			t.Errorf("%s reported as %+v, want %+v", mac, got[mac], w)
		}
	}

	clock.set(at(12, 0, 5))
	cdp.poll(context.Background())
	if len(sink.emits) != 1 {
		t.Errorf("a tick with nothing due emitted %d times more", len(sink.emits)-1)
	}
	if n := router.countCalls("/ip/arp/print"); n != 1 {
		t.Errorf("ARP read %d times, want once", n)
	}
}

// A device list that cannot be read stops the tick before anything else:
// the schedule of every device is kept — no device is forgotten and read
// again at once — and the external lease servers are not asked either.
func TestClientDiscoveryKeepsItsScheduleWhenTheDeviceListFails(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"r3": devclass.C3})
	router := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/arp/print":               rep(map[string]string{"address": "192.0.2.10", "mac-address": "02:00:00:00:00:0A"}),
		"/ip/dhcp-server/lease/print": rep(),
	}}
	clock := &testClock{t: at(12, 0, 0)}
	seams := monolithSeams(db, nil, NewCadence(db, nil), clock.now)
	cfg := &failingConfig{ConfigSource: seams.Config}
	seams.Config, seams.Exec, seams.Sink = cfg, scriptedExec(map[string]*fakeRunner{"r3": router}), &emitRecorder{}
	fetches := 0
	cdp := NewClientDiscoveryPoller(seams, func() []leasesource.Lease { fetches++; return nil }, nil, 15*time.Minute)
	cdp.now = clock.now
	cdp.clock = newTierClock(NewScheduleRegistry())
	cdp.clock.phase = noJitter

	tick := func(hh, mm int, fail bool) {
		cfg.fail = fail
		clock.set(at(hh, mm, 0))
		cdp.poll(context.Background())
	}
	tick(12, 0, false) // ARP read at once, external leases fetched
	tick(12, 5, true)
	tick(12, 6, false)
	if n := router.countCalls("/ip/arp/print"); n != 1 {
		t.Errorf("ARP read %d times by 12:06, want once: its next slot is 12:15, the failed tick must not reset it", n)
	}
	tick(12, 20, true) // the external sources would be due
	if fetches != 1 {
		t.Errorf("external leases fetched %d times, want once: a failed tick does nothing", fetches)
	}
}
