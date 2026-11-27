package poller

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// defaultConnectivityInterval is the probe-cycle period when the admin hasn't
// configured connectivity_interval.
const defaultConnectivityInterval = 30 * time.Second

// defaultConnectivityPingCount is the pings-per-probe burst size when the admin
// hasn't configured connectivity_ping_count. Clamped to 1..10 so a burst
// (count × 1s interval) stays well under the /ping command's deadline.
const defaultConnectivityPingCount = 5

// ConnectivityPoller runs periodic ICMP probes FROM RouterOS devices (/ping
// over the API) against the configured ping_targets, persisting loss/RTT/jitter
// time series and broadcasting each sample on the "connectivity.sample" topic.
//
// Per cycle:
//  1. Resolve each enabled target to (probing device, address). Internet
//     targets carry both; client targets resolve their current IP from
//     mac_lookup (cached back into ping_targets.address).
//  2. Group probes by probing device and run the groups concurrently — the
//     per-client mutex in routeros.RunCommand serializes same-device commands
//     anyway, so targets within a group run sequentially.
//  3. If any client target is watched, snapshot wifi signal readings for those
//     MACs from the CAPsMAN/wifi registration tables (client_signal_samples).
//
// Unresolvable/offline targets still persist a sample (sent=0, error set) so
// the time series shows "no data" gaps honestly. A target whose probing
// device's polling is paused is skipped without a sample: paused minutes are
// gaps with no rows, and the device gets nothing but liveness pings.
type ConnectivityPoller struct {
	db       *sql.DB
	pool     *routeros.Pool
	hub      *ws.Hub
	interval time.Duration

	// Auto-traceroute state: per-target cooldown so a sustained dropoff doesn't
	// re-trace every cycle, plus a small global semaphore so a flood of lossy
	// targets can't spawn unbounded dedicated-connection goroutines.
	traceMu      sync.Mutex
	traceLastRun map[string]time.Time
	traceSem     chan struct{}

	// wifi is where a watched client's signal comes from (UseWifiReadings);
	// signalAt is the registration read each watched MAC's last sample was
	// taken from. Both belong to the poll goroutine.
	wifi     WifiReadings
	signalAt map[string]time.Time
}

// WifiReadings is the wifi tracker as the connectivity poller sees it: the
// latest registration read that lists a client (WifiTracker.Reading).
type WifiReadings interface {
	Reading(mac string) (WifiReading, bool)
}

// UseWifiReadings names the source of the watched clients' signal samples.
// Without one no signal is recorded. Call before Run.
func (cp *ConnectivityPoller) UseWifiReadings(src WifiReadings) {
	cp.wifi = src
}

// autoTracerouteCooldown is the minimum spacing between auto-captured
// traceroutes for one target.
const autoTracerouteCooldown = 10 * time.Minute

// tracerouteTimeout bounds one /tool/traceroute run (20 hops × count=1 against
// an unresponsive path stays under this).
const tracerouteTimeout = 45 * time.Second

func NewConnectivityPoller(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, interval time.Duration) *ConnectivityPoller {
	return &ConnectivityPoller{
		db:           db,
		pool:         pool,
		hub:          hub,
		interval:     interval,
		traceLastRun: make(map[string]time.Time),
		traceSem:     make(chan struct{}, 2),
		signalAt:     make(map[string]time.Time),
	}
}

// currentInterval reads the runtime-tunable connectivity_interval (seconds)
// from app_settings, falling back to the constructed interval. Re-read each
// cycle so a Settings change applies without a backend restart. Values are
// bounded to a day: a huge pasted number would overflow the Duration multiply
// into a negative timer, turning the loop into a device-flooding hot loop.
func (cp *ConnectivityPoller) currentInterval() time.Duration {
	if v, err := queries.GetSetting(cp.db, "connectivity_interval"); err == nil {
		if n, e := strconv.Atoi(strings.TrimSpace(v)); e == nil && n > 0 && n <= 86400 {
			return time.Duration(n) * time.Second
		}
	}
	return cp.interval
}

// pingCount reads the runtime-tunable connectivity_ping_count from
// app_settings, clamped to 1..10, falling back to the package default.
func (cp *ConnectivityPoller) pingCount() int {
	count := defaultConnectivityPingCount
	if v, err := queries.GetSetting(cp.db, "connectivity_ping_count"); err == nil {
		if n, e := strconv.Atoi(strings.TrimSpace(v)); e == nil {
			count = n
		}
	}
	if count < 1 {
		count = 1
	}
	if count > 10 {
		count = 10
	}
	return count
}

// tracerouteLossThreshold reads the runtime-tunable traceroute_loss_threshold
// (percent) from app_settings. A returned 0 means auto-capture is DISABLED —
// unset, unparseable or out-of-range (outside 0..100) values all disable it
// rather than guessing.
func (cp *ConnectivityPoller) tracerouteLossThreshold() float64 {
	v, err := queries.GetSetting(cp.db, "traceroute_loss_threshold")
	if err != nil {
		return 0
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || n <= 0 || n > 100 {
		return 0
	}
	return n
}

func (cp *ConnectivityPoller) Run(ctx context.Context) {
	// Run shortly after startup (short delay for device connections to establish)
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}
	cp.safePoll(ctx)

	for {
		timer := time.NewTimer(cp.currentInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			cp.safePoll(ctx)
		}
	}
}

func (cp *ConnectivityPoller) safePoll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("connectivity: panic: %v", r)
		}
	}()
	start := time.Now()
	cp.poll(ctx)
	// The next timer only starts after poll() returns, so an overlong cycle
	// silently stretches the effective sampling interval. Make that visible.
	if elapsed, want := time.Since(start), cp.currentInterval(); elapsed > want {
		log.Printf("connectivity: poll cycle took %s, longer than the %s interval — samples will be spaced further apart (reduce targets per device or the ping count)", elapsed.Round(time.Second), want)
	}
}

// probeTask is one resolved probe: a target plus the address it resolved to.
type probeTask struct {
	target  queries.PingTarget
	address string
}

func (cp *ConnectivityPoller) poll(ctx context.Context) {
	targets, err := queries.ListEnabledPingTargets(cp.db)
	if err != nil {
		log.Printf("connectivity: list targets: %v", err)
		return
	}
	if len(targets) == 0 {
		return // nothing watched — keep the idle cycle free of any device traffic
	}

	devices, err := queries.ListDevices(cp.db)
	if err != nil {
		log.Printf("connectivity: list devices: %v", err)
		return
	}
	deviceByID := make(map[string]queries.Device, len(devices))
	for _, d := range devices {
		deviceByID[d.ID] = d
	}
	now := time.Now()
	paused := func(id string) bool {
		d, ok := deviceByID[id]
		if ok && d.PollingPaused(now) {
			pauseLog.skip("connectivity probes", d)
			return true
		}
		return false
	}

	// Resolve every target to (probing device, address), grouping by device.
	groups := make(map[string][]probeTask)
	watchedMACs := make(map[string]bool)
	for _, t := range targets {
		select {
		case <-ctx.Done():
			return
		default:
		}

		switch t.Kind {
		case "client":
			if t.MACAddress != "" {
				watchedMACs[strings.ToUpper(t.MACAddress)] = true
			}
			devID, addr, _, reason := queries.ResolveClientProbe(cp.db, &t)
			if addr != "" && addr != t.Address {
				// Cache the freshly-resolved IP for the UI.
				_ = queries.UpdatePingTargetAddress(cp.db, t.ID, addr)
			}
			if paused(devID) {
				continue
			}
			if reason != "" {
				cp.recordError(t, devID, addr, reason)
				continue
			}
			groups[devID] = append(groups[devID], probeTask{target: t, address: addr})

		default: // "internet"
			if t.DeviceID == "" {
				cp.recordError(t, "", t.Address, "no probing device configured")
				continue
			}
			dev, ok := deviceByID[t.DeviceID]
			if !ok {
				cp.recordError(t, t.DeviceID, t.Address, "probing device no longer exists")
				continue
			}
			if paused(t.DeviceID) {
				continue
			}
			if dev.Status != "online" {
				cp.recordError(t, t.DeviceID, t.Address, "probing device is "+dev.Status)
				continue
			}
			groups[t.DeviceID] = append(groups[t.DeviceID], probeTask{target: t, address: t.Address})
		}
	}

	count := cp.pingCount()
	lossThreshold := cp.tracerouteLossThreshold()

	// Probe each device's group concurrently; within a group sequentially (the
	// per-client mutex serializes same-device API commands anyway).
	var wg sync.WaitGroup
	for devID, tasks := range groups {
		wg.Add(1)
		go func(devID string, tasks []probeTask) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("connectivity: panic probing via device %s: %v", devID, r)
					cp.pool.Close(devID)
				}
			}()

			client := cp.pool.GetLive(devID)
			if client == nil {
				for _, task := range tasks {
					cp.recordError(task.target, devID, task.address, "no API connection to probing device")
				}
				return
			}

			for _, task := range tasks {
				select {
				case <-ctx.Done():
					return
				default:
				}
				res, err := routeros.RunPing(client, routeros.PingOptions{
					Address:    task.address,
					Count:      count,
					SrcAddress: task.target.SrcAddress,
					Interface:  task.target.SrcInterface,
				})
				if err != nil {
					cp.recordError(task.target, devID, task.address, err.Error())
					continue
				}
				cp.persistAndPublish(&queries.PingSample{
					TargetID: task.target.ID,
					DeviceID: devID,
					Address:  task.address,
					Sent:     res.Sent,
					Received: res.Received,
					LossPct:  res.LossPct,
					RTTMinMs: res.RTTMinMs,
					RTTAvgMs: res.RTTAvgMs,
					RTTMaxMs: res.RTTMaxMs,
					JitterMs: res.JitterMs,
				})
				// Auto-capture the path during a dropoff: an internet target whose
				// probe RAN (error=="") but lost >= threshold gets an async
				// traceroute, so the lossy route is recorded while it's happening.
				if task.target.Kind != "client" && lossThreshold > 0 && res.LossPct >= lossThreshold {
					if dev, ok := deviceByID[devID]; ok {
						cp.maybeAutoTraceroute(task.target, dev)
					}
				}
			}
		}(devID, tasks)
	}
	wg.Wait()

	// Signal phase: the watched clients' wifi signal, from the wifi tracker.
	cp.collectSignals(watchedMACs)
}

// recordError persists (and broadcasts) a probe-couldn't-run sample: sent=0,
// error set, no RTTs. Keeps the series honest about gaps.
func (cp *ConnectivityPoller) recordError(t queries.PingTarget, deviceID, address, reason string) {
	cp.persistAndPublish(&queries.PingSample{
		TargetID: t.ID,
		DeviceID: deviceID,
		Address:  address,
		Sent:     0,
		Received: 0,
		Error:    reason,
	})
}

// persistAndPublish writes the sample to SQLite FIRST (the WS publish drops for
// slow clients, so the DB is the source of truth), then broadcasts it.
func (cp *ConnectivityPoller) persistAndPublish(s *queries.PingSample) {
	if err := queries.InsertPingSample(cp.db, s); err != nil {
		log.Printf("connectivity: insert sample for target %s: %v", s.TargetID, err)
		return
	}
	cp.hub.Publish("connectivity.sample", s)
}

// maybeAutoTraceroute fires an async traceroute for a lossy internet target.
// Per-target cooldown (autoTracerouteCooldown) plus the global traceSem bound
// (cap 2) keep a flood of lossy targets from spawning unbounded goroutines —
// when the bound is hit the capture is simply skipped (the cooldown is only
// recorded for runs that actually start, so the next cycle retries). The
// shared TryBeginRun guard additionally keeps it from overlapping an API
// run-now traceroute for the same target.
//
// The trace runs on a dedicated DialOnce connection: a 20-hop run against an
// unresponsive path takes up to ~45s, and on the pooled session every other
// poller's command would wait behind it — past its own deadline. dev
// comes from the cycle's ListDevices snapshot, credentials already decrypted.
func (cp *ConnectivityPoller) maybeAutoTraceroute(t queries.PingTarget, dev queries.Device) {
	cp.traceMu.Lock()
	if last, ok := cp.traceLastRun[t.ID]; ok && time.Since(last) < autoTracerouteCooldown {
		cp.traceMu.Unlock()
		return
	}
	select {
	case cp.traceSem <- struct{}{}:
	default:
		cp.traceMu.Unlock()
		return // too many traceroutes already in flight
	}
	// Shared run guard with the API's run-now endpoint: if a manual traceroute
	// for this target is mid-flight, skip WITHOUT recording the cooldown so the
	// next cycle retries.
	if !TryBeginRun("traceroute:" + t.ID) {
		<-cp.traceSem
		cp.traceMu.Unlock()
		return
	}
	cp.traceLastRun[t.ID] = time.Now()
	cp.traceMu.Unlock()

	go func() {
		defer EndRun("traceroute:" + t.ID)
		defer func() { <-cp.traceSem }()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("connectivity: panic in auto-traceroute for target %s: %v", t.ID, r)
			}
		}()

		run := &queries.TracerouteRun{TargetID: t.ID, Address: t.Address}
		client, err := routeros.DialOnce(DialTarget(dev), dev.Username, dev.PasswordEnc)
		if err != nil {
			run.Error = err.Error()
		} else {
			hops, err := routeros.Traceroute(client, t.Address, t.SrcAddress, t.SrcInterface, tracerouteTimeout)
			client.Close()
			if err != nil {
				run.Error = err.Error()
			} else {
				run.Hops = hops
			}
		}

		// Persist FIRST (the WS publish drops for slow clients), then broadcast.
		if err := queries.InsertTracerouteRun(cp.db, run); err != nil {
			log.Printf("connectivity: insert traceroute run for target %s: %v", t.ID, err)
			return
		}
		cp.hub.Publish("connectivity.traceroute", run)
	}()
}

// collectSignals records a ClientSignalSample for each watched MAC from the
// wifi tracker's registration reads. It sends no command: the tables are
// read once, by the tracker, on each controller's own cadence and only on
// devices with a wireless package — this loop used to print them on every
// online device every cycle, three trapping prints on a switch.
//
// One sample per registration read, stamped with the time of that read: a
// client on a C1 access point gets a point every 5 minutes, one whose
// controller is paused or unreachable gets none until it is read again.
func (cp *ConnectivityPoller) collectSignals(watched map[string]bool) {
	for mac := range cp.signalAt {
		if !watched[mac] {
			delete(cp.signalAt, mac)
		}
	}
	if cp.wifi == nil {
		return
	}
	for mac := range watched {
		r, ok := cp.wifi.Reading(mac)
		if !ok || !r.At.After(cp.signalAt[mac]) {
			continue
		}
		cp.signalAt[mac] = r.At
		err := queries.InsertClientSignalSample(cp.db, &queries.ClientSignalSample{
			MACAddress: mac,
			APName:     r.AP,
			SSID:       r.SSID,
			Band:       r.Band,
			SignalDBm:  parseSignalDBm(r.Signal),
			TxRate:     r.TxRate,
			RxRate:     r.RxRate,
			RecordedAt: r.At.UTC(),
		})
		if err != nil {
			log.Printf("connectivity: insert signal sample for %s: %v", mac, err)
		}
	}
}

// parseSignalDBm extracts the leading integer dBm from a registration-table
// signal string, which may look like "-62 dBm", "-65@HT20-...", or just "-58".
// Returns nil when no leading integer is present.
func parseSignalDBm(s string) *int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	i := 0
	if s[0] == '-' || s[0] == '+' {
		i = 1
	}
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == i {
		return nil
	}
	v, err := strconv.Atoi(s[:j])
	if err != nil {
		return nil
	}
	return &v
}
