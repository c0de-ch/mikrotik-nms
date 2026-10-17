package poller

import (
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// PortMonitorSettings holds the runtime-tunable knobs for port-state monitoring.
// Values are read from the app_settings table on each poll cycle so the
// operator can adjust thresholds without restarting the backend.
type PortMonitorSettings struct {
	Enabled       bool
	TypeFilter    []string      // include only interfaces whose type starts with one of these (empty = all)
	FlapThreshold int           // ≥N flaps (link-downs) in window → flap event
	FlapWindow    time.Duration // window for flap detection
}

// portMonitorDefaults are used when the corresponding app_settings row is
// missing or unparseable.
var portMonitorDefaults = PortMonitorSettings{
	Enabled:       true,
	TypeFilter:    []string{"ether", "sfp", "wlan", "bridge", "vlan"},
	FlapThreshold: 3,
	FlapWindow:    5 * time.Minute,
}

func loadPortMonitorSettings(db *sql.DB) PortMonitorSettings {
	s := portMonitorDefaults

	if v, err := queries.GetSetting(db, "port_monitor_enabled"); err == nil {
		s.Enabled = v == "true" || v == "1"
	}
	if v, err := queries.GetSetting(db, "port_monitor_filter"); err == nil {
		v = strings.TrimSpace(v)
		if v == "" || v == "all" {
			s.TypeFilter = nil
		} else {
			parts := strings.Split(v, ",")
			out := make([]string, 0, len(parts))
			for _, p := range parts {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			s.TypeFilter = out
		}
	}
	if v, err := queries.GetSetting(db, "port_flap_threshold"); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			s.FlapThreshold = n
		}
	}
	if v, err := queries.GetSetting(db, "port_flap_window_seconds"); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			s.FlapWindow = time.Duration(n) * time.Second
		}
	}
	return s
}

// includeInterface returns true when the interface should be monitored
// according to the type-filter setting. An empty filter means everything.
func (s *PortMonitorSettings) includeInterface(iface routeros.InterfaceSnapshot) bool {
	if len(s.TypeFilter) == 0 {
		return true
	}
	for _, prefix := range s.TypeFilter {
		// Match by exact type or prefix — RouterOS reports types like "ether",
		// "wlan", "wifi" (newer firmware uses "wifi"), "bridge", "vlan",
		// "sfp-sfpplus1" actually doesn't appear; sfp interfaces have type
		// "ether" too. The "sfp" filter therefore matches by name prefix.
		if iface.Type == prefix {
			return true
		}
		if strings.HasPrefix(iface.Name, prefix) {
			return true
		}
	}
	return false
}

// portState is the per-interface in-memory state tracked by the poller.
//
// The exported `interface_state` row mirrors `running` / `disabled` /
// `lastUp` / `lastDown` / the flaps in the window for the UI. The rest is
// needed only across cycles to detect transitions.
type portState struct {
	running           bool
	disabled          bool
	lastUp            string
	lastDown          string
	loopProtectStatus string // "" / "none" / "on-loop" / "in-loop"
	// linkDowns is the link-downs counter at the previous observation, a
	// baseline only while hasLinkDowns (not after a restart: the counter
	// is not persisted, so the first observation re-baselines).
	linkDowns     int64
	hasLinkDowns  bool
	seenAt        time.Time  // when that observation was read (zero: none since startup)
	flaps         []flapMark // flaps within the flap window, oldest first
	flappedFiring bool       // true while the interface is over the flap threshold (suppress duplicate critical events)
	firedAt       time.Time  // when the current port_link_flap fired
}

// flapMark records n flaps (link-downs) observed at one observation.
type flapMark struct {
	at time.Time
	n  int
}

// flapsInWindow drops the marks older than window before now and returns
// how many flaps remain.
func (p *portState) flapsInWindow(now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	kept := p.flaps[:0]
	total := 0
	for _, f := range p.flaps {
		if !f.at.Before(cutoff) {
			kept = append(kept, f)
			total += f.n
		}
	}
	p.flaps = kept
	return total
}

// observedFlaps is how many times iface lost link since the previous
// observation prev: the link-downs delta while both readings carry the
// counter (every RouterOS 7 interface does) — which also catches a flap
// that came and went between two reads — else, without a baseline, a running
// → not running transition. A counter that went backwards (reboot, reset)
// counts as 0, and so does a reading more than a flap window after the
// previous one (a polling pause, an outage, a snapshot that was missing):
// those link-downs are spread over a gap no window can place them in, and
// booking them at once would fire a critical flap for hours of history. It
// moves prev's baseline to the current reading, read at now.
func observedFlaps(prev *portState, hadPrev bool, iface routeros.InterfaceSnapshot, now time.Time, window time.Duration) int {
	n := 0
	gap := !prev.seenAt.IsZero() && now.Sub(prev.seenAt) > window
	switch {
	case gap:
	case iface.HasLinkDowns && prev.hasLinkDowns:
		if iface.LinkDowns > prev.linkDowns {
			n = int(min(iface.LinkDowns-prev.linkDowns, math.MaxInt32))
		}
	case hadPrev && prev.running && !iface.Running:
		n = 1
	}
	prev.linkDowns, prev.hasLinkDowns, prev.seenAt = iface.LinkDowns, iface.HasLinkDowns, now
	return n
}

// portMonitor keeps per-device per-interface state across poll cycles.
// Embedded into NetworkHealthPoller via a single field.
type portMonitor struct {
	prev     map[string]map[string]*portState // device_id → iface_name → state
	firstRun map[string]bool                  // device_id → first-poll-since-startup flag
}

func newPortMonitor() *portMonitor {
	return &portMonitor{
		prev:     make(map[string]map[string]*portState),
		firstRun: make(map[string]bool),
	}
}

// portEvent describes a transition that should be emitted as a loop_events row.
type portEvent struct {
	Kind      string // port_disabled | port_link_down | port_link_flap
	Severity  string
	Interface string
	Message   string
}

// processInterface compares the current snapshot row against the previous
// observation and returns events to emit. It also updates the in-memory
// state and returns the up-to-date interface_state row for persistence. now
// is the observation time — the snapshot's read time — so processing the
// same snapshot twice changes nothing.
//
// On the first poll for a device, no events are returned (firstRun guard) —
// otherwise a backend restart would replay every "port disabled" /
// "link down" condition that already existed at startup.
func (m *portMonitor) processInterface(
	devID string,
	iface routeros.InterfaceSnapshot,
	settings PortMonitorSettings,
	now time.Time,
) (events []portEvent, state *queries.InterfaceState) {
	dev, ok := m.prev[devID]
	if !ok {
		dev = make(map[string]*portState)
		m.prev[devID] = dev
	}
	prev, hadPrev := dev[iface.Name]
	if !hadPrev {
		prev = &portState{}
		dev[iface.Name] = prev
	}

	first := m.firstRun[devID]

	// running=false transitions
	if hadPrev && !first {
		if prev.running && !iface.Running && !iface.Disabled {
			events = append(events, portEvent{
				Kind:      "port_link_down",
				Severity:  "warn",
				Interface: iface.Name,
				Message:   fmt.Sprintf("interface %q lost link", iface.Name),
			})
		}
		// admin disabled=false → true
		if !prev.disabled && iface.Disabled {
			events = append(events, portEvent{
				Kind:      "port_disabled",
				Severity:  "warn",
				Interface: iface.Name,
				Message:   fmt.Sprintf("interface %q was administratively disabled", iface.Name),
			})
		}
	}

	// loop-protect transition: any value → "on-loop" / "in-loop". The status
	// comes from the Tier 4a ethernet read merged into the snapshot; a row
	// without one (non-ethernet, or no fresh ethernet read yet) keeps the
	// last known status instead of reading as "" — so the first read after
	// a restart cannot re-fire a loop that the restored state already knew.
	loopProtect := prev.loopProtectStatus
	if iface.HasEthernet {
		loopProtect = iface.Ethernet.LoopProtectStatus
	}
	if hadPrev && !first {
		wasInLoop := isInLoop(prev.loopProtectStatus)
		isInLoopNow := isInLoop(loopProtect)
		if !wasInLoop && isInLoopNow {
			events = append(events, portEvent{
				Kind:      "port_loop_protect",
				Severity:  "critical",
				Interface: iface.Name,
				Message: fmt.Sprintf("interface %q tripped loop-protect (status=%s)",
					iface.Name, loopProtect),
			})
		}
	}

	// Flap detection: port_flap_threshold flaps (link-downs) within
	// port_flap_window_seconds fire one critical port_link_flap. Re-armed
	// once the window drops below the threshold, or a whole window after
	// the event while the storm goes on.
	count := prev.flapsInWindow(now, settings.FlapWindow)
	if prev.flappedFiring && (count < settings.FlapThreshold || now.Sub(prev.firedAt) > settings.FlapWindow) {
		prev.flappedFiring = false
	}
	if n := observedFlaps(prev, hadPrev, iface, now, settings.FlapWindow); n > 0 {
		prev.flaps = append(prev.flaps, flapMark{at: now, n: n})
		count += n
		if count >= settings.FlapThreshold && !prev.flappedFiring && !first {
			events = append(events, portEvent{
				Kind:      "port_link_flap",
				Severity:  "critical",
				Interface: iface.Name,
				Message: fmt.Sprintf("interface %q link flapped %d× in last %s",
					iface.Name, count, settings.FlapWindow),
			})
			prev.flappedFiring, prev.firedAt = true, now
		}
	}

	prev.running = iface.Running
	prev.disabled = iface.Disabled
	prev.lastUp = iface.LastLinkUp
	prev.lastDown = iface.LastLinkDown
	prev.loopProtectStatus = loopProtect

	state = &queries.InterfaceState{
		ID:                devID + ":" + iface.Name,
		DeviceID:          devID,
		InterfaceName:     iface.Name,
		InterfaceType:     iface.Type,
		Running:           iface.Running,
		Disabled:          iface.Disabled,
		Slave:             iface.Slave,
		LastLinkUp:        iface.LastLinkUp,
		LastLinkDown:      iface.LastLinkDown,
		FlapCountWindow:   count,
		LoopProtectStatus: loopProtect,
		Comment:           iface.Comment,
	}
	return events, state
}

// isInLoop returns true ONLY when RouterOS reports the port is currently
// blocked because loop-protect detected a loop. Real values RouterOS uses:
//
//	""        — field not exposed (non-ethernet, older firmware)
//	"none"    — older firmware: not-in-loop
//	"off"     — loop-protect feature DISABLED on this port (healthy!)
//	"on"      — loop-protect enabled, no loop detected (healthy)
//	"on-loop" — RouterOS 7.x: a loop is currently detected on this port
//	"in-loop" — older phrasing for the same condition
//
// The first version of this code treated anything that wasn't "" or "none"
// as in-loop, which fired false-positive critical events on every port that
// simply had loop-protect *disabled*.
func isInLoop(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "on-loop", "in-loop":
		return true
	}
	return false
}

// RestoreFromDB seeds the in-memory state from interface_state rows. Called
// once on startup so the first poll after a restart doesn't blanket-suppress
// every transition (the first-run guard treats any device with no in-memory
// state as brand-new). Without this, ports that were already disabled at
// boot remain invisible in the events feed until they transition again.
func (m *portMonitor) RestoreFromDB(db *sql.DB) error {
	rows, err := queries.ListInterfaceStates(db)
	if err != nil {
		return err
	}
	for _, r := range rows {
		dev, ok := m.prev[r.DeviceID]
		if !ok {
			dev = make(map[string]*portState)
			m.prev[r.DeviceID] = dev
		}
		dev[r.InterfaceName] = &portState{
			running:           r.Running,
			disabled:          r.Disabled,
			lastUp:            r.LastLinkUp,
			lastDown:          r.LastLinkDown,
			loopProtectStatus: r.LoopProtectStatus,
		}
	}
	return nil
}

// markDeviceProcessed flips the firstRun guard off for this device so that
// the next cycle's transitions actually fire events.
func (m *portMonitor) markDeviceProcessed(devID string) {
	m.firstRun[devID] = false
}

// rememberFirstRun is called before processing if the device has never been
// seen — so we know to suppress events on this cycle.
func (m *portMonitor) rememberFirstRun(devID string) {
	if _, ok := m.prev[devID]; !ok {
		m.firstRun[devID] = true
	}
}

// pruneDevices drops in-memory state for devices that are no longer active,
// so the maps don't grow unboundedly as devices are added and removed over
// the lifetime of the backend. Called once per poll cycle with the current
// set of device IDs.
func (m *portMonitor) pruneDevices(active map[string]struct{}) {
	for id := range m.prev {
		if _, ok := active[id]; !ok {
			delete(m.prev, id)
			delete(m.firstRun, id)
		}
	}
}
