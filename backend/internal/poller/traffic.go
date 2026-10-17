package poller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

const (
	// trafficTopicPrefix starts every per-interface stream topic,
	// traffic.<deviceID>.<iface> (and also traffic.ports, which is not one).
	trafficTopicPrefix = "traffic."

	// liveReconcileEvery is how often subscriptions are vetted and streams
	// started or stopped — the longest a viewer waits for its
	// subscribe.ack / subscribe.error.
	liveReconcileEvery = time.Second
	// livePolicyTTL is how long a device's class, profile, pause, status and
	// interface list are cached for vetting.
	livePolicyTTL = 30 * time.Second
	// liveRequestedPeriod is what every viewer asks for: the 1 s the port
	// detail has always streamed. The class caps stretch it (LiveCaps.Period).
	liveRequestedPeriod = time.Second
	// liveLogEvery rate-limits a failing stream's log line.
	liveLogEvery = time.Minute
)

// Refusal codes: the code of a subscribe.error on a stream topic, and the
// reason GET /devices/{id}/live-limits gives when a viewer may not stream.
const (
	LiveRefusedUnknownDevice = "unknown_device"
	LiveRefusedUnknownIface  = "unknown_interface"
	LiveRefusedAdminOnly     = "admin_only"
	LiveRefusedPaused        = "polling_paused"
	LiveRefusedIfaceLimit    = "iface_limit"
	LiveRefusedLeaseLimit    = "lease_limit"
	LiveRefusedPollersOff    = "pollers_off"
)

// TrafficTopic is the stream topic of one interface.
func TrafficTopic(deviceID, iface string) string {
	return trafficTopicPrefix + deviceID + "." + iface
}

// ParseTrafficTopic splits traffic.<deviceID>.<iface>. Interface names may
// contain dots, device ids never do, so the device id ends at the first dot.
// ok is false for any other topic, traffic.ports included.
func ParseTrafficTopic(topic string) (deviceID, iface string, ok bool) {
	rest, found := strings.CutPrefix(topic, trafficTopicPrefix)
	if !found {
		return "", "", false
	}
	deviceID, iface, found = strings.Cut(rest, ".")
	if !found || deviceID == "" || iface == "" {
		return "", "", false
	}
	return deviceID, iface, true
}

// LiveLimits are a device's live-stream ceilings as the UI shows them: what
// GET /devices/{id}/live-limits serves and every subscribe.ack /
// subscribe.error on a stream topic carries.
type LiveLimits struct {
	// Class is the class the caps follow: the effective class, or C1 on
	// the light profile.
	Class            devclass.Class   `json:"class"`
	Profile          devclass.Profile `json:"profile"`
	MinPeriodSeconds int              `json:"min_period_seconds"`
	MaxIfaces        int              `json:"max_ifaces"`
	Leases           int              `json:"leases"`
	AdminOnly        bool             `json:"admin_only"`
	// SnapshotIntervalSeconds is how often the port-stats snapshot — what
	// a view shows instead of a refused stream — refreshes.
	SnapshotIntervalSeconds int `json:"snapshot_interval_seconds"`
}

// liveClassOf is the class whose caps apply to the device (see
// devclass.EffectiveLiveCaps).
func liveClassOf(d *queries.Device) devclass.Class {
	if d.Profile() == devclass.ProfileLight {
		return devclass.C1
	}
	return d.EffectiveClass()
}

// LiveLimitsFor describes the device's caps; snapshotInterval is the
// port-stats cadence (PortStatsInterval).
func LiveLimitsFor(d *queries.Device, snapshotInterval time.Duration) LiveLimits {
	caps := devclass.EffectiveLiveCaps(d.EffectiveClass(), d.Profile())
	return LiveLimits{
		Class:                   liveClassOf(d),
		Profile:                 d.Profile(),
		MinPeriodSeconds:        int(caps.MinPeriod / time.Second),
		MaxIfaces:               caps.MaxIfaces,
		Leases:                  caps.Leases,
		AdminOnly:               caps.AdminOnly,
		SnapshotIntervalSeconds: int(snapshotInterval / time.Second),
	}
}

// LiveAccessFor reports whether a viewer may open a stream on the device at
// all — as far as role, an admin pause and the instance's polling mode
// decide — returning the refusal code, or "". The interface and lease
// ceilings depend on what other viewers hold, so only a subscription is
// vetted against them.
func LiveAccessFor(d *queries.Device, admin, pollingOn bool, now time.Time, snapshotInterval time.Duration) (LiveLimits, string) {
	l := LiveLimitsFor(d, snapshotInterval)
	switch {
	case !pollingOn:
		return l, LiveRefusedPollersOff
	case l.AdminOnly && !admin:
		return l, LiveRefusedAdminOnly
	case d.PollingPaused(now):
		return l, LiveRefusedPaused
	}
	return l, ""
}

// LiveRefusalMessage is what the UI says instead of a refused stream.
func LiveRefusalMessage(code string, l LiveLimits) string {
	snapshot := fmt.Sprintf("showing %d s snapshot data", l.SnapshotIntervalSeconds)
	switch code {
	case LiveRefusedAdminOnly:
		return "Live view is limited on weak devices — " + snapshot
	case LiveRefusedIfaceLimit:
		return fmt.Sprintf("Live view is limited to %d ports at a time on this device — %s", l.MaxIfaces, snapshot)
	case LiveRefusedLeaseLimit:
		return "Another session is using this device's live view — " + snapshot
	case LiveRefusedPaused:
		return "NMS polling is paused for this device — live view is off"
	case LiveRefusedPollersOff:
		return "Device polling is off on this instance"
	case LiveRefusedUnknownIface:
		return "This interface is not in the device's inventory yet, so it cannot stream"
	case LiveRefusedUnknownDevice:
		return "This device is not managed"
	}
	return ""
}

// liveStatus is the data of a subscribe.ack or subscribe.error frame on a
// stream topic.
type liveStatus struct {
	LiveLimits
	// PeriodSeconds (ack) is the period the stream samples at.
	PeriodSeconds int `json:"period_seconds,omitempty"`
	// Code and Error (error) say why the stream was refused.
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
	// Fallback (error) is the topic that still carries the port's rate.
	Fallback string `json:"fallback,omitempty"`
}

// livePolicy is what vetting and the streams need to know about a device.
type livePolicy struct {
	fetched time.Time
	// unavailable: the device could not be read (database error); its
	// subscriptions are left as they are until a read succeeds.
	unavailable bool
	exists      bool
	device      queries.Device
	caps        devclass.LiveCaps
	limits      LiveLimits
	ifaces      map[string]bool
}

// liveLeases is what one device streams: the viewers holding a lease and the
// union of interfaces they stream (which one sample reads).
type liveLeases struct {
	viewers map[uint64]bool
	ifaces  map[string]bool
}

func newLiveLeases() *liveLeases {
	return &liveLeases{viewers: map[uint64]bool{}, ifaces: map[string]bool{}}
}

// vetLive decides one viewer's request to stream iface against the device's
// policy and the leases granted so far this round: "" grants it (and adds it
// to leases), anything else is the refusal code. Joining an interface
// another viewer already streams costs the device nothing, but the viewer
// still needs a lease of its own.
func vetLive(p livePolicy, leases *liveLeases, viewer uint64, admin bool, iface string, now time.Time) string {
	switch {
	case !p.exists:
		return LiveRefusedUnknownDevice
	case !p.ifaces[iface]:
		return LiveRefusedUnknownIface
	case p.caps.AdminOnly && !admin:
		return LiveRefusedAdminOnly
	case p.device.PollingPaused(now):
		return LiveRefusedPaused
	}
	if !leases.ifaces[iface] && len(leases.ifaces) >= p.caps.MaxIfaces {
		return LiveRefusedIfaceLimit
	}
	if !leases.viewers[viewer] && len(leases.viewers) >= p.caps.Leases {
		return LiveRefusedLeaseLimit
	}
	leases.ifaces[iface] = true
	leases.viewers[viewer] = true
	return ""
}

// grantKey is one subscribe of one viewer to one stream topic: a viewer that
// subscribes again (new Seq) is a new request and gets its own answer.
type grantKey struct {
	viewer uint64
	topic  string
	seq    uint64
}

// deviceStream is the one sampling loop of a device: every granted interface
// is read in a single monitor-traffic call per period.
type deviceStream struct {
	cancel context.CancelFunc
	period time.Duration
	ifaces []string // sorted union; replaced by reconcile under tm.mu
}

// TrafficManager runs the per-interface live streams (WS topics
// traffic.<deviceID>.<iface>) the traffic page's port detail opens, within
// the class caps of devclass.LiveCaps: on a weak device (C1, and every
// unprobed or light-profile device) only admins may stream, one viewer at a
// time and at most four interfaces, sampled every 15 s instead of every
// second. Each subscription is answered with a subscribe.ack (with the
// effective period) or a subscribe.error (and dropped); a refused view shows
// the port-stats snapshot (traffic.ports) instead. A device's granted
// interfaces share one sampling loop and one monitor-traffic call per
// period, and nothing is read while nobody is subscribed.
//
// The caps bound device load, not who may see a port's rate (every user
// sees it on the snapshot), so a subscriber may receive the samples of an
// interface someone else streams in the second before it is vetted.
type TrafficManager struct {
	db   *sql.DB
	pool *routeros.Pool
	hub  *ws.Hub

	// Test seams: the device read, control frames (true = queued) and the
	// clock.
	sample func(dev queries.Device, ifaces []string) (map[string]routeros.TrafficData, error)
	notify func(c *ws.Client, typ, topic string, data any) bool
	now    func() time.Time

	policyMu sync.Mutex
	policies map[string]livePolicy // only devices someone subscribes to

	mu       sync.Mutex
	grants   map[grantKey]time.Duration // granted → the period acked (0: ack not queued yet)
	refused  map[grantKey]bool          // refused and told; the hub is dropping it
	streams  map[string]*deviceStream   // by device id
	lastRead map[string]time.Time       // by device id: when the last read began
	lastLog  map[string]time.Time
}

func NewTrafficManager(db *sql.DB, pool *routeros.Pool, hub *ws.Hub) *TrafficManager {
	tm := &TrafficManager{
		db:       db,
		pool:     pool,
		hub:      hub,
		now:      time.Now,
		policies: make(map[string]livePolicy),
		grants:   make(map[grantKey]time.Duration),
		refused:  make(map[grantKey]bool),
		streams:  make(map[string]*deviceStream),
		lastRead: make(map[string]time.Time),
		lastLog:  make(map[string]time.Time),
	}
	tm.sample = tm.poolSample
	tm.notify = hub.Notify
	return tm
}

// Run vets subscriptions and keeps the streams in step with them until ctx
// is done.
func (tm *TrafficManager) Run(ctx context.Context) {
	ticker := time.NewTicker(liveReconcileEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			tm.stopAll()
			return
		case <-ticker.C:
			tm.reconcile(ctx)
		}
	}
}

// liveRequest is one viewer's subscribe to one stream topic.
type liveRequest struct {
	sub   ws.Subscription
	topic string
	iface string
}

func (rq liveRequest) key() grantKey {
	return grantKey{rq.sub.Client.ID(), rq.topic, rq.sub.Seq}
}

// reconcile vets every stream subscription the hub holds and starts, retunes
// or stops the device streams to match. Granted subscriptions are vetted
// first, so a viewer never loses its stream to a newcomer, then the rest in
// connection order (oldest first). A policy change (pause, class override,
// light profile) re-vets everyone within livePolicyTTL: a viewer refused now
// gets a subscribe.error, one whose period changed a fresh subscribe.ack.
//
// A refusal counts only once the client was told: the hub then drops that
// subscribe (UnsubscribeSeq, so a newer subscribe of the same topic is left
// to be answered on its own). An error the client's full send buffer
// swallowed is retried next round instead, and an ack that was not queued
// keeps the grant but is sent again.
func (tm *TrafficManager) reconcile(ctx context.Context) {
	now := tm.now()
	byDevice := make(map[string][]liveRequest)
	current := make(map[grantKey]bool)
	for topic, subs := range tm.hub.SubscribersWithPrefix(trafficTopicPrefix) {
		dev, iface, ok := ParseTrafficTopic(topic)
		if !ok {
			continue
		}
		for _, sub := range subs {
			rq := liveRequest{sub: sub, topic: topic, iface: iface}
			byDevice[dev] = append(byDevice[dev], rq)
			current[rq.key()] = true
		}
	}
	policies := make(map[string]livePolicy, len(byDevice))
	for dev := range byDevice {
		policies[dev] = tm.policy(dev, now)
	}
	tm.prunePolicies(byDevice)

	var drops []liveRequest // refused and told: unsubscribed after tm.mu
	tm.mu.Lock()
	for k := range tm.grants {
		if !current[k] {
			delete(tm.grants, k) // unsubscribed, re-subscribed or disconnected
		}
	}
	for k := range tm.refused {
		if !current[k] {
			delete(tm.refused, k)
		}
	}

	type want struct {
		period time.Duration
		ifaces map[string]bool
	}
	desired := make(map[string]want)
	for dev, reqs := range byDevice {
		p := policies[dev]
		if p.unavailable {
			// Keep serving what was granted; vet again once the device reads.
			if st := tm.streams[dev]; st != nil {
				d := want{period: st.period, ifaces: map[string]bool{}}
				for _, rq := range reqs {
					if _, ok := tm.grants[rq.key()]; ok {
						d.ifaces[rq.iface] = true
					}
				}
				desired[dev] = d
			}
			continue
		}
		sort.Slice(reqs, func(i, j int) bool {
			_, gi := tm.grants[reqs[i].key()]
			_, gj := tm.grants[reqs[j].key()]
			if gi != gj {
				return gi
			}
			if a, b := reqs[i].sub.Client.ID(), reqs[j].sub.Client.ID(); a != b {
				return a < b
			}
			return reqs[i].topic < reqs[j].topic
		})
		period := p.caps.Period(liveRequestedPeriod)
		leases := newLiveLeases()
		for _, rq := range reqs {
			k, c := rq.key(), rq.sub.Client
			if tm.refused[k] {
				continue // told; the hub is dropping it
			}
			code := vetLive(p, leases, c.ID(), c.Info().Role == "admin", rq.iface, now)
			if code != "" {
				delete(tm.grants, k)
				// notify only queues (never blocks), so it may run under tm.mu.
				if tm.notify(c, ws.TypeSubscribeError, rq.topic, liveStatus{LiveLimits: p.limits, Code: code,
					Error: LiveRefusalMessage(code, p.limits), Fallback: PortRatesTopic}) {
					tm.refused[k] = true
					drops = append(drops, rq)
				}
				continue
			}
			if acked, ok := tm.grants[k]; !ok || acked != period {
				tm.grants[k] = 0
				if tm.notify(c, ws.TypeSubscribeAck, rq.topic, liveStatus{LiveLimits: p.limits,
					PeriodSeconds: int(period / time.Second)}) {
					tm.grants[k] = period
				}
			}
			d, ok := desired[dev]
			if !ok {
				d = want{period: period, ifaces: map[string]bool{}}
			}
			d.ifaces[rq.iface] = true
			desired[dev] = d
		}
	}

	for dev, st := range tm.streams {
		if d, ok := desired[dev]; !ok || len(d.ifaces) == 0 || d.period != st.period {
			st.cancel()
			delete(tm.streams, dev)
		}
	}
	for dev, d := range desired {
		if len(d.ifaces) == 0 {
			continue
		}
		st := tm.streams[dev]
		if st == nil {
			sctx, cancel := context.WithCancel(ctx)
			st = &deviceStream{cancel: cancel, period: d.period}
			tm.streams[dev] = st
			go tm.runStream(sctx, dev, st)
		}
		st.ifaces = slices.Sorted(maps.Keys(d.ifaces))
	}
	tm.mu.Unlock()

	// Hub ops only after letting go of tm.mu: they block while the hub's
	// queue is full, and nothing may wait on the hub holding it.
	for _, rq := range drops {
		tm.hub.UnsubscribeSeq(rq.sub.Client, rq.topic, rq.sub.Seq)
	}
}

// policy returns the device's cached policy, reading the database when it
// is older than livePolicyTTL. A failed read is never cached: the last good
// answer stands (or "unavailable" when there is none) and the next round
// reads again.
func (tm *TrafficManager) policy(deviceID string, now time.Time) livePolicy {
	tm.policyMu.Lock()
	p, ok := tm.policies[deviceID]
	tm.policyMu.Unlock()
	if ok && now.Sub(p.fetched) < livePolicyTTL {
		return p
	}
	fresh := tm.loadPolicy(deviceID, now)
	if fresh.unavailable {
		if ok {
			return p
		}
		return fresh
	}
	tm.policyMu.Lock()
	tm.policies[deviceID] = fresh
	tm.policyMu.Unlock()
	return fresh
}

// prunePolicies forgets the devices nobody subscribes to any more. The
// cache is keyed by device ids taken from client-chosen topic names, so it
// must not outlive the subscriptions (a stream only runs for a subscribed
// device, and one that finds its policy gone simply skips the read).
func (tm *TrafficManager) prunePolicies(subscribed map[string][]liveRequest) {
	tm.policyMu.Lock()
	defer tm.policyMu.Unlock()
	for dev := range tm.policies {
		if _, ok := subscribed[dev]; !ok {
			delete(tm.policies, dev)
		}
	}
}

// cachedPolicy is the last policy read for the device, however old; the
// streams use it so a sample never waits on the database.
func (tm *TrafficManager) cachedPolicy(deviceID string) livePolicy {
	tm.policyMu.Lock()
	defer tm.policyMu.Unlock()
	return tm.policies[deviceID]
}

func (tm *TrafficManager) loadPolicy(deviceID string, now time.Time) livePolicy {
	p := livePolicy{fetched: now}
	d, err := queries.GetDevice(tm.db, deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return p // not managed (any more)
	}
	if err != nil {
		log.Printf("poller traffic: load device %s: %v", deviceID, err)
		p.unavailable = true
		return p
	}
	ifaces, err := queries.ListInterfacesByDevice(tm.db, deviceID)
	if err != nil {
		log.Printf("poller traffic: load interfaces of %s: %v", d.Identity, err)
		p.unavailable = true
		return p
	}
	p.exists, p.device = true, *d
	p.caps = devclass.EffectiveLiveCaps(d.EffectiveClass(), d.Profile())
	p.limits = LiveLimitsFor(d, PortStatsInterval(tm.db))
	p.ifaces = make(map[string]bool, len(ifaces))
	for _, i := range ifaces {
		p.ifaces[i.Name] = true
	}
	return p
}

// runStream samples the device's granted interfaces once per period until
// ctx is done. The first read waits for one period since the device's
// previous one began — that may still be in flight on the loop this one
// replaced — so reopening a view or a period change cannot outpace the
// class's period.
func (tm *TrafficManager) runStream(ctx context.Context, deviceID string, st *deviceStream) {
	tm.mu.Lock()
	first := tm.lastRead[deviceID].Add(st.period).Sub(tm.now())
	tm.mu.Unlock()
	timer := time.NewTimer(max(first, 0))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		tm.sampleOnce(ctx, deviceID, st)
		timer.Reset(st.period)
	}
}

// sampleOnce reads every interface the stream carries in one call, records
// the samples (the live range's history seed) and publishes them.
func (tm *TrafficManager) sampleOnce(ctx context.Context, deviceID string, st *deviceStream) {
	tm.mu.Lock()
	ifaces := slices.Clone(st.ifaces)
	tm.mu.Unlock()
	p := tm.cachedPolicy(deviceID)
	if len(ifaces) == 0 || !p.exists || p.device.Status != "online" || p.device.PollingPaused(tm.now()) {
		return // nothing granted, or nothing to read from: no dial
	}

	// Stamped before the call, so a loop started while it is in flight
	// waits a full period from it (runStream).
	tm.mu.Lock()
	tm.lastRead[deviceID] = tm.now()
	tm.mu.Unlock()
	data, err := tm.sample(p.device, ifaces)
	at := tm.now()
	if err != nil {
		if !errors.Is(err, errNoClient) {
			tm.logThrottled(deviceID, "poller traffic: monitor-traffic on %s: %v", p.device.Identity, err)
		}
		return
	}
	if ctx.Err() != nil {
		return // the stream ended while the device answered
	}
	ts := at.UTC().Format(time.RFC3339)
	for _, iface := range ifaces {
		d, ok := data[iface]
		if !ok {
			continue
		}
		if err := queries.InsertTrafficSample(tm.db, &queries.TrafficSample{
			DeviceID:        deviceID,
			InterfaceName:   iface,
			RxBitsPerSec:    d.RxBitsPerSec,
			TxBitsPerSec:    d.TxBitsPerSec,
			RxPacketsPerSec: d.RxPacketsPerSec,
			TxPacketsPerSec: d.TxPacketsPerSec,
		}); err != nil {
			tm.logThrottled("db:"+deviceID, "poller traffic: store sample of %s/%s: %v", p.device.Identity, iface, err)
		}
		tm.hub.Publish(TrafficTopic(deviceID, iface), map[string]interface{}{
			"device_id":      deviceID,
			"interface_name": iface,
			"rx_bps":         d.RxBitsPerSec,
			"tx_bps":         d.TxBitsPerSec,
			"rx_pps":         d.RxPacketsPerSec,
			"tx_pps":         d.TxPacketsPerSec,
			"period_seconds": int(st.period / time.Second),
			"timestamp":      ts,
		})
	}
}

// poolSample reads the interfaces' current rates over the device's pooled
// connection: one /interface/monitor-traffic =once= for all of them.
func (tm *TrafficManager) poolSample(dev queries.Device, ifaces []string) (map[string]routeros.TrafficData, error) {
	client := tm.pool.GetLive(dev.ID)
	if client == nil {
		return nil, errNoClient
	}
	if len(ifaces) == 1 {
		// The single-interface reply is read positionally, so a row that
		// does not echo the interface name still counts.
		d, err := routeros.GetTrafficSnapshot(client, ifaces[0])
		if err != nil {
			return nil, err
		}
		return map[string]routeros.TrafficData{ifaces[0]: *d}, nil
	}
	return routeros.GetPortTraffic(client, ifaces)
}

func (tm *TrafficManager) logThrottled(key, format string, args ...any) {
	tm.mu.Lock()
	now := tm.now()
	if last, ok := tm.lastLog[key]; ok && now.Sub(last) < liveLogEvery {
		tm.mu.Unlock()
		return
	}
	tm.lastLog[key] = now
	tm.mu.Unlock()
	log.Printf(format, args...)
}

func (tm *TrafficManager) stopAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for key, st := range tm.streams {
		st.cancel()
		delete(tm.streams, key)
	}
}
