package poller

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

func TestParseTrafficTopic(t *testing.T) {
	tests := []struct {
		topic, dev, iface string
		ok                bool
	}{
		{"traffic.d1.ether1", "d1", "ether1", true},
		{"traffic.d1.vlan.100", "d1", "vlan.100", true}, // names may contain dots
		{"traffic.d1.sfp1 uplink/a", "d1", "sfp1 uplink/a", true},
		{"traffic.ports", "", "", false},
		{"traffic.d1.", "", "", false},
		{"traffic..ether1", "", "", false},
		{"topology.traffic", "", "", false},
	}
	for _, tt := range tests {
		dev, iface, ok := ParseTrafficTopic(tt.topic)
		if dev != tt.dev || iface != tt.iface || ok != tt.ok {
			t.Errorf("ParseTrafficTopic(%q) = %q, %q, %v", tt.topic, dev, iface, ok)
		}
		if tt.ok && TrafficTopic(dev, iface) != tt.topic {
			t.Errorf("TrafficTopic(%q, %q) does not round-trip", dev, iface)
		}
	}
}

// policyOf builds a vetting policy the way loadPolicy does.
func policyOf(class devclass.Class, profile devclass.Profile, paused bool, ifaces ...string) livePolicy {
	d := queries.Device{ID: "d", Status: "online", DeviceClass: string(class), PollProfile: string(profile)}
	if paused {
		until := time.Now().Add(time.Hour)
		d.PollingPausedUntil = &until
	}
	p := livePolicy{exists: true, device: d, caps: devclass.EffectiveLiveCaps(d.EffectiveClass(), d.Profile()),
		limits: LiveLimitsFor(&d, 15*time.Second), ifaces: map[string]bool{}}
	for _, i := range ifaces {
		p.ifaces[i] = true
	}
	return p
}

func ports(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("ether%d", i+1)
	}
	return out
}

// leasesOf is a device already streaming ifaces to viewer 1.
func leasesOf(ifaces ...string) *liveLeases {
	l := newLiveLeases()
	for _, i := range ifaces {
		l.ifaces[i] = true
		l.viewers[1] = true
	}
	return l
}

// The subscribe decision: class × role × interface count × leases, plus the
// pause and inventory checks that come first.
func TestVetLive(t *testing.T) {
	all := ports(20)
	const admin, viewer = true, false
	tests := []struct {
		name    string
		policy  livePolicy
		leases  *liveLeases
		who     uint64
		isAdmin bool
		iface   string
		want    string
	}{
		{"C1 viewer", policyOf(devclass.C1, "", false, all...), newLiveLeases(), 1, viewer, "ether1", LiveRefusedAdminOnly},
		{"C1 admin", policyOf(devclass.C1, "", false, all...), newLiveLeases(), 1, admin, "ether1", ""},
		{"C1 admin, 5th port", policyOf(devclass.C1, "", false, all...), leasesOf(ports(4)...), 1, admin, "ether5", LiveRefusedIfaceLimit},
		{"C1 admin, a port already streamed", policyOf(devclass.C1, "", false, all...), leasesOf(ports(4)...), 1, admin, "ether4", ""},
		{"C1 second admin, same port", policyOf(devclass.C1, "", false, all...), leasesOf("ether1"), 2, admin, "ether1", LiveRefusedLeaseLimit},
		{"unprobed counts as C1", policyOf(devclass.C0Unknown, "", false, all...), newLiveLeases(), 1, viewer, "ether1", LiveRefusedAdminOnly},
		{"C3 on the light profile counts as C1", policyOf(devclass.C3, devclass.ProfileLight, false, all...), newLiveLeases(), 1, viewer, "ether1", LiveRefusedAdminOnly},
		{"C2 viewer", policyOf(devclass.C2, "", false, all...), leasesOf(ports(7)...), 1, viewer, "ether8", ""},
		{"C2 9th port", policyOf(devclass.C2, "", false, all...), leasesOf(ports(8)...), 1, viewer, "ether9", LiveRefusedIfaceLimit},
		{"C2 second viewer", policyOf(devclass.C2, "", false, all...), leasesOf("ether1"), 2, viewer, "ether1", ""},
		{"C3 viewer", policyOf(devclass.C3, "", false, all...), newLiveLeases(), 1, viewer, "ether1", ""},
		{"C3 16th port", policyOf(devclass.C3, "", false, all...), leasesOf(ports(15)...), 1, viewer, "ether16", ""},
		{"C3 17th port", policyOf(devclass.C3, "", false, all...), leasesOf(ports(16)...), 1, viewer, "ether17", LiveRefusedIfaceLimit},
		{"paused", policyOf(devclass.C3, "", true, all...), newLiveLeases(), 1, admin, "ether1", LiveRefusedPaused},
		{"unknown interface", policyOf(devclass.C3, "", false, all...), newLiveLeases(), 1, admin, "wlan9", LiveRefusedUnknownIface},
		{"unknown device", livePolicy{}, newLiveLeases(), 1, admin, "ether1", LiveRefusedUnknownDevice},
	}
	for _, tt := range tests {
		if got := vetLive(tt.policy, tt.leases, tt.who, tt.isAdmin, tt.iface, time.Now()); got != tt.want {
			t.Errorf("%s: vetLive = %q, want %q", tt.name, got, tt.want)
		}
	}

	// Leases fill up across viewers: C2 allows two, C3 four.
	for _, tc := range []struct {
		class  devclass.Class
		leases int
	}{{devclass.C1, 1}, {devclass.C2, 2}, {devclass.C3, 4}} {
		p := policyOf(tc.class, "", false, all...)
		l := newLiveLeases()
		for v := uint64(1); v <= uint64(tc.leases); v++ {
			if code := vetLive(p, l, v, true, "ether1", time.Now()); code != "" {
				t.Fatalf("%s viewer %d refused: %s", tc.class, v, code)
			}
		}
		if code := vetLive(p, l, uint64(tc.leases)+1, true, "ether1", time.Now()); code != LiveRefusedLeaseLimit {
			t.Errorf("%s viewer %d = %q, want lease_limit", tc.class, tc.leases+1, code)
		}
	}
}

func TestLivePeriodPerClass(t *testing.T) {
	for _, tc := range []struct {
		class   devclass.Class
		profile devclass.Profile
		want    time.Duration
	}{
		{devclass.C1, "", 15 * time.Second},
		{devclass.C0Unknown, "", 15 * time.Second},
		{devclass.C2, "", 5 * time.Second},
		{devclass.C3, "", time.Second},
		{devclass.C3, devclass.ProfileLight, 15 * time.Second},
	} {
		if got := policyOf(tc.class, tc.profile, false).caps.Period(liveRequestedPeriod); got != tc.want {
			t.Errorf("%s/%s period = %v, want %v", tc.class, tc.profile, got, tc.want)
		}
	}
}

func TestLiveAccessForAndMessages(t *testing.T) {
	now := time.Now()
	c1 := &queries.Device{ID: "sw", DeviceClass: "C1"}
	c3 := &queries.Device{ID: "rt", DeviceClass: "C3"}
	paused := &queries.Device{ID: "p", DeviceClass: "C3"}
	until := now.Add(time.Hour)
	paused.PollingPausedUntil = &until

	l, code := LiveAccessFor(c1, false, true, now, 15*time.Second)
	if code != LiveRefusedAdminOnly || l.Class != devclass.C1 || l.MinPeriodSeconds != 15 || l.MaxIfaces != 4 || l.Leases != 1 || !l.AdminOnly {
		t.Errorf("C1 viewer: %+v %q", l, code)
	}
	if got, want := LiveRefusalMessage(code, l), "Live view is limited on weak devices — showing 15 s snapshot data"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	if _, code := LiveAccessFor(c1, true, true, now, 15*time.Second); code != "" {
		t.Errorf("C1 admin refused: %q", code)
	}
	if l, code := LiveAccessFor(c3, false, true, now, 15*time.Second); code != "" || l.MinPeriodSeconds != 1 || l.AdminOnly {
		t.Errorf("C3 viewer: %+v %q", l, code)
	}
	if _, code := LiveAccessFor(c3, true, false, now, 15*time.Second); code != LiveRefusedPollersOff {
		t.Errorf("polling off: %q", code)
	}
	if _, code := LiveAccessFor(paused, true, true, now, 15*time.Second); code != LiveRefusedPaused {
		t.Errorf("paused: %q", code)
	}
	// The fallback cadence follows the snapshot's.
	l = LiveLimitsFor(c1, 60*time.Second)
	if got := LiveRefusalMessage(LiveRefusedIfaceLimit, l); got != "Live view is limited to 4 ports at a time on this device — showing 60 s snapshot data" {
		t.Errorf("iface-limit message = %q", got)
	}
}

// liveRecorder captures the manager's control frames and device reads.
type liveRecorder struct {
	mu      sync.Mutex
	notices []recordedNotice
	reads   map[string][][]string // by device id
	full    bool                  // the client's send buffer is full: frames are not queued

	// stampedBeforeRead reports, at each read, whether the manager had
	// already recorded that read's start (nil: not checked).
	stampedBeforeRead func(dev string) bool
	unstamped         int
}

type recordedNotice struct {
	viewer uint64
	typ    string
	topic  string
	status liveStatus
}

func (r *liveRecorder) notify(c *ws.Client, typ, topic string, data any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return false
	}
	r.notices = append(r.notices, recordedNotice{c.ID(), typ, topic, data.(liveStatus)})
	return true
}

func (r *liveRecorder) setFull(full bool) {
	r.mu.Lock()
	r.full = full
	r.mu.Unlock()
}

func (r *liveRecorder) sample(dev queries.Device, ifaces []string) (map[string]routeros.TrafficData, error) {
	stamped := true
	if r.stampedBeforeRead != nil {
		stamped = r.stampedBeforeRead(dev.ID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !stamped {
		r.unstamped++
	}
	if r.reads == nil {
		r.reads = make(map[string][][]string)
	}
	r.reads[dev.ID] = append(r.reads[dev.ID], append([]string(nil), ifaces...))
	out := make(map[string]routeros.TrafficData, len(ifaces))
	for i, name := range ifaces {
		out[name] = routeros.TrafficData{RxBitsPerSec: int64(1000 * (i + 1)), TxBitsPerSec: 500}
	}
	return out, nil
}

// take returns and clears the recorded notices.
func (r *liveRecorder) take() []recordedNotice {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.notices
	r.notices = nil
	return out
}

func (r *liveRecorder) readCount(dev string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reads[dev])
}

func (r *liveRecorder) lastRead(dev string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reads[dev]) == 0 {
		return nil
	}
	return r.reads[dev][len(r.reads[dev])-1]
}

func subscribe(t *testing.T, hub *ws.Hub, c *ws.Client, topic string) {
	t.Helper()
	before := hub.TopicSubscriberCount(topic)
	hub.Subscribe(c, topic)
	waitUntil(t, func() bool { return hub.TopicSubscriberCount(topic) == before+1 })
}

func unsubscribe(t *testing.T, hub *ws.Hub, c *ws.Client, topic string) {
	t.Helper()
	before := hub.TopicSubscriberCount(topic)
	hub.Unsubscribe(c, topic)
	waitUntil(t, func() bool { return hub.TopicSubscriberCount(topic) == before-1 })
}

func wantNotice(t *testing.T, got []recordedNotice, viewer uint64, typ, topic string) recordedNotice {
	t.Helper()
	for _, n := range got {
		if n.viewer == viewer && n.typ == typ && n.topic == topic {
			return n
		}
	}
	t.Fatalf("no %s for viewer %d on %s in %+v", typ, viewer, topic, got)
	return recordedNotice{}
}

// Subscriptions on a weak and a capable device, end to end through the hub:
// refusals and acks, the shared C1 sampling loop, lease and interface
// ceilings, teardown, and a class change picked up after the cache TTL.
func TestTrafficManagerVetsAndStreams(t *testing.T) {
	db := liveDB(t)
	liveDevice(t, db, "sw", devclass.C1, ports(6)...)
	liveDevice(t, db, "rt", devclass.C3, "ether1")

	hub := ws.NewHub()
	go hub.Run()
	rec := &liveRecorder{}
	tm := NewTrafficManager(db, nil, hub)
	tm.notify, tm.sample = rec.notify, rec.sample
	var skew atomic.Int64
	tm.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	// Every read must be stamped before it goes out (see runStream).
	rec.stampedBeforeRead = func(dev string) bool {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		last := tm.lastRead[dev]
		return !last.IsZero() && tm.now().Sub(last) < time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); tm.stopAll() })

	viewer := ws.NewClientFor(hub, nil, ws.ClientInfo{Username: "v", Role: "viewer"})
	admin := ws.NewClientFor(hub, nil, ws.ClientInfo{Username: "a", Role: "admin"})
	admin2 := ws.NewClientFor(hub, nil, ws.ClientInfo{Username: "b", Role: "admin"})

	// A viewer on the weak switch is refused and dropped: no device read.
	swEther1 := TrafficTopic("sw", "ether1")
	subscribe(t, hub, viewer, swEther1)
	tm.reconcile(ctx)
	n := wantNotice(t, rec.take(), viewer.ID(), ws.TypeSubscribeError, swEther1)
	if n.status.Code != LiveRefusedAdminOnly || n.status.Fallback != PortRatesTopic ||
		n.status.Error != "Live view is limited on weak devices — showing 15 s snapshot data" {
		t.Errorf("refusal = %+v", n.status)
	}
	waitUntil(t, func() bool { return hub.TopicSubscriberCount(swEther1) == 0 })
	tm.reconcile(ctx)
	if got := rec.take(); len(got) != 0 {
		t.Errorf("a dropped subscription was answered again: %+v", got)
	}

	// An admin streams five ports: four are granted at 15 s, one refused.
	samples := make(chan map[string]interface{}, 8)
	defer hub.Listen(swEther1, func(v any) { samples <- v.(map[string]interface{}) })()
	for _, p := range ports(5) {
		subscribe(t, hub, admin, TrafficTopic("sw", p))
	}
	tm.reconcile(ctx)
	got := rec.take()
	for _, p := range ports(4) {
		if n := wantNotice(t, got, admin.ID(), ws.TypeSubscribeAck, TrafficTopic("sw", p)); n.status.PeriodSeconds != 15 || n.status.Class != devclass.C1 {
			t.Errorf("ack on %s = %+v", p, n.status)
		}
	}
	if n := wantNotice(t, got, admin.ID(), ws.TypeSubscribeError, TrafficTopic("sw", "ether5")); n.status.Code != LiveRefusedIfaceLimit {
		t.Errorf("5th port = %+v", n.status)
	}
	// One loop for the device: the first read covers all four ports at once.
	waitUntil(t, func() bool { return rec.readCount("sw") == 1 })
	if want := ports(4); !reflect.DeepEqual(rec.lastRead("sw"), want) {
		t.Errorf("read %v, want one call for %v", rec.lastRead("sw"), want)
	}
	select {
	case s := <-samples:
		if s["period_seconds"] != 15 || s["rx_bps"] != int64(1000) || s["interface_name"] != "ether1" {
			t.Errorf("published sample = %v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no sample published on the stream topic")
	}
	tm.mu.Lock()
	if st := tm.streams["sw"]; st == nil || st.period != 15*time.Second {
		t.Errorf("C1 stream = %+v, want a 15 s loop", st)
	}
	tm.mu.Unlock()

	// A second admin on the same port needs a lease of its own: C1 has one.
	subscribe(t, hub, admin2, swEther1)
	tm.reconcile(ctx)
	if n := wantNotice(t, rec.take(), admin2.ID(), ws.TypeSubscribeError, swEther1); n.status.Code != LiveRefusedLeaseLimit {
		t.Errorf("second lease = %+v", n.status)
	}

	// A viewer on the capable router streams at the old 1 s.
	rtEther1 := TrafficTopic("rt", "ether1")
	subscribe(t, hub, viewer, rtEther1)
	tm.reconcile(ctx)
	if n := wantNotice(t, rec.take(), viewer.ID(), ws.TypeSubscribeAck, rtEther1); n.status.PeriodSeconds != 1 || n.status.AdminOnly {
		t.Errorf("C3 ack = %+v", n.status)
	}

	// Everyone leaves the switch: its loop stops.
	for _, p := range ports(4) {
		unsubscribe(t, hub, admin, TrafficTopic("sw", p))
	}
	tm.reconcile(ctx)
	tm.mu.Lock()
	_, swRunning := tm.streams["sw"]
	tm.mu.Unlock()
	if swRunning {
		t.Fatal("the switch is still streamed after its last viewer left")
	}

	// Reopening the view right away must not read before the class period
	// has passed since the last read.
	reads := rec.readCount("sw")
	subscribe(t, hub, admin, swEther1)
	tm.reconcile(ctx)
	wantNotice(t, rec.take(), admin.ID(), ws.TypeSubscribeAck, swEther1)
	time.Sleep(150 * time.Millisecond)
	if rec.readCount("sw") != reads {
		t.Errorf("reopening read the switch again within its 15 s period")
	}

	// The router is reclassified C1. Within the cache TTL nothing changes;
	// after it the viewer's grant is withdrawn.
	if err := queries.SetDeviceClass(db, "rt", devclass.C1, devclass.ResourceFacts{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	tm.reconcile(ctx)
	if got := rec.take(); len(got) != 0 {
		t.Errorf("policy re-read before its TTL: %+v", got)
	}
	skew.Store(int64(livePolicyTTL + time.Second))
	tm.reconcile(ctx)
	if n := wantNotice(t, rec.take(), viewer.ID(), ws.TypeSubscribeError, rtEther1); n.status.Code != LiveRefusedAdminOnly {
		t.Errorf("after reclassification = %+v", n.status)
	}

	rec.mu.Lock()
	unstamped := rec.unstamped
	rec.mu.Unlock()
	if unstamped != 0 {
		t.Errorf("%d reads went out before their start was recorded", unstamped)
	}
}

// liveManager wires a manager to a fresh hub with the recorder's seams.
func liveManager(t *testing.T) (*TrafficManager, *ws.Hub, *liveRecorder, context.Context) {
	t.Helper()
	db := liveDB(t)
	liveDevice(t, db, "sw", devclass.C1, ports(6)...)
	hub := ws.NewHub()
	go hub.Run()
	rec := &liveRecorder{}
	tm := NewTrafficManager(db, nil, hub)
	tm.notify, tm.sample = rec.notify, rec.sample
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); tm.stopAll() })
	return tm, hub, rec, ctx
}

// Every subscribe gets its own answer: a refused viewer that asks again is
// told again rather than ignored, and a viewer that left and came back
// between two rounds gets a fresh ack for its new subscription.
func TestTrafficManagerAnswersEverySubscribe(t *testing.T) {
	tm, hub, rec, ctx := liveManager(t)
	viewer := ws.NewClientFor(hub, nil, ws.ClientInfo{Role: "viewer"})
	admin := ws.NewClientFor(hub, nil, ws.ClientInfo{Role: "admin"})
	topic := TrafficTopic("sw", "ether1")

	subscribe(t, hub, viewer, topic)
	tm.reconcile(ctx)
	wantNotice(t, rec.take(), viewer.ID(), ws.TypeSubscribeError, topic)
	waitUntil(t, func() bool { return hub.TopicSubscriberCount(topic) == 0 })
	subscribe(t, hub, viewer, topic) // asks again at once
	tm.reconcile(ctx)
	wantNotice(t, rec.take(), viewer.ID(), ws.TypeSubscribeError, topic)
	waitUntil(t, func() bool { return hub.TopicSubscriberCount(topic) == 0 })

	subscribe(t, hub, admin, topic)
	tm.reconcile(ctx)
	wantNotice(t, rec.take(), admin.ID(), ws.TypeSubscribeAck, topic)
	// Live → 15m → live within one round: the new subscribe is answered.
	unsubscribe(t, hub, admin, topic)
	subscribe(t, hub, admin, topic)
	tm.reconcile(ctx)
	wantNotice(t, rec.take(), admin.ID(), ws.TypeSubscribeAck, topic)
}

// A refusal the client never received (its send buffer was full) does not
// count: the subscription stays until the error gets through, so the page
// is never left waiting for an answer that was dropped.
func TestTrafficManagerRetriesUnheardRefusal(t *testing.T) {
	tm, hub, rec, ctx := liveManager(t)
	viewer := ws.NewClientFor(hub, nil, ws.ClientInfo{Role: "viewer"})
	topic := TrafficTopic("sw", "ether1")
	subscribe(t, hub, viewer, topic)

	rec.setFull(true)
	tm.reconcile(ctx)
	time.Sleep(20 * time.Millisecond)
	if n := hub.TopicSubscriberCount(topic); n != 1 {
		t.Fatalf("an unheard refusal dropped the subscription (%d subscribers)", n)
	}

	rec.setFull(false)
	tm.reconcile(ctx)
	wantNotice(t, rec.take(), viewer.ID(), ws.TypeSubscribeError, topic)
	waitUntil(t, func() bool { return hub.TopicSubscriberCount(topic) == 0 })
}

// The policy cache follows the subscriptions — topics name client-chosen
// device ids — and a failed read is never cached.
func TestTrafficManagerPolicyCache(t *testing.T) {
	tm, hub, rec, ctx := liveManager(t)
	c := ws.NewClientFor(hub, nil, ws.ClientInfo{Role: "admin"})
	bogus := []string{TrafficTopic("nope1", "x"), TrafficTopic("nope2", "x"), TrafficTopic("sw", "ether1")}
	for _, topic := range bogus {
		subscribe(t, hub, c, topic)
	}
	tm.reconcile(ctx)
	got := rec.take()
	wantNotice(t, got, c.ID(), ws.TypeSubscribeError, bogus[0])
	wantNotice(t, got, c.ID(), ws.TypeSubscribeAck, bogus[2])
	if n := policyCount(tm); n != 3 {
		t.Fatalf("policies cached = %d, want 3", n)
	}

	// The refused ones are dropped, the granted one is left: so is its policy.
	waitUntil(t, func() bool { return hub.TopicSubscriberCount(bogus[0]) == 0 && hub.TopicSubscriberCount(bogus[1]) == 0 })
	tm.reconcile(ctx)
	if n := policyCount(tm); n != 1 {
		t.Errorf("policies after the refused topics left = %d, want 1", n)
	}
	unsubscribe(t, hub, c, bogus[2])
	tm.reconcile(ctx)
	if n := policyCount(tm); n != 0 {
		t.Errorf("policies after every viewer left = %d, want 0", n)
	}

	// A device that cannot be read is retried next round, not cached.
	tm.db.Close()
	if p := tm.policy("sw", time.Now()); !p.unavailable {
		t.Fatalf("policy on a closed database = %+v, want unavailable", p)
	}
	if n := policyCount(tm); n != 0 {
		t.Errorf("a failed read was cached (%d policies)", n)
	}
}

func policyCount(tm *TrafficManager) int {
	tm.policyMu.Lock()
	defer tm.policyMu.Unlock()
	return len(tm.policies)
}

// A database error must not read as "device gone": the grants stand.
func TestTrafficManagerKeepsGrantsWhenTheDeviceCannotBeRead(t *testing.T) {
	db := liveDB(t)
	liveDevice(t, db, "rt", devclass.C3, "ether1")
	hub := ws.NewHub()
	go hub.Run()
	rec := &liveRecorder{}
	tm := NewTrafficManager(db, nil, hub)
	tm.notify, tm.sample = rec.notify, rec.sample
	var skew atomic.Int64
	tm.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); tm.stopAll() })

	c := ws.NewClientFor(hub, nil, ws.ClientInfo{Role: "viewer"})
	topic := TrafficTopic("rt", "ether1")
	subscribe(t, hub, c, topic)
	tm.reconcile(ctx)
	wantNotice(t, rec.take(), c.ID(), ws.TypeSubscribeAck, topic)

	db.Close() // every read now fails
	skew.Store(int64(livePolicyTTL + time.Second))
	tm.reconcile(ctx)
	if got := rec.take(); len(got) != 0 {
		t.Errorf("a failed device read changed the grants: %+v", got)
	}
	tm.mu.Lock()
	_, running := tm.streams["rt"]
	tm.mu.Unlock()
	if !running {
		t.Error("the stream stopped on a failed device read")
	}
}
