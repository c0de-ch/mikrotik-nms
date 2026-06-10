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
// (count × 1s interval) stays well under the RouterOS CommandTimeout.
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
// the time series shows "no data" gaps honestly.
type ConnectivityPoller struct {
	db       *sql.DB
	pool     *routeros.Pool
	hub      *ws.Hub
	interval time.Duration
}

func NewConnectivityPoller(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, interval time.Duration) *ConnectivityPoller {
	return &ConnectivityPoller{db: db, pool: pool, hub: hub, interval: interval}
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
			if dev.Status != "online" {
				cp.recordError(t, t.DeviceID, t.Address, "probing device is "+dev.Status)
				continue
			}
			groups[t.DeviceID] = append(groups[t.DeviceID], probeTask{target: t, address: t.Address})
		}
	}

	count := cp.pingCount()

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

			client := cp.pool.Get(devID)
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
				res, err := routeros.RunPing(client, task.address, count)
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
			}
		}(devID, tasks)
	}
	wg.Wait()

	// Signal phase: snapshot wifi signal for watched client MACs.
	if len(watchedMACs) > 0 {
		cp.collectSignals(ctx, devices, watchedMACs)
	}
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

// collectSignals fetches the wifi registration tables once per online device
// and records a ClientSignalSample for each watched MAC found. Best-effort:
// errors are ignored, panics contained per device.
func (cp *ConnectivityPoller) collectSignals(ctx context.Context, devices []queries.Device, watched map[string]bool) {
	regByMAC := make(map[string]routeros.WifiRegistration)

	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if dev.Status != "online" {
			continue
		}
		client := cp.pool.Get(dev.ID)
		if client == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("connectivity: panic reading registrations from %s: %v", dev.Identity, r)
					cp.pool.Close(dev.ID)
				}
			}()
			regs, err := routeros.GetCAPsMANRegistrations(client)
			if err != nil {
				return
			}
			for _, reg := range regs {
				mac := strings.ToUpper(reg.MAC)
				if mac == "" || !watched[mac] {
					continue
				}
				if _, seen := regByMAC[mac]; !seen {
					regByMAC[mac] = reg
				}
			}
		}()
	}

	for mac, reg := range regByMAC {
		ap := reg.AP
		if ap == "" {
			// Interface names like "cap-wifi1/2" carry the radio before the slash.
			ap = reg.Interface
			if i := strings.Index(ap, "/"); i >= 0 {
				ap = ap[:i]
			}
		}
		err := queries.InsertClientSignalSample(cp.db, &queries.ClientSignalSample{
			MACAddress: mac,
			APName:     ap,
			SSID:       reg.SSID,
			Band:       reg.Band,
			SignalDBm:  parseSignalDBm(reg.Signal),
			TxRate:     reg.TxRate,
			RxRate:     reg.RxRate,
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
