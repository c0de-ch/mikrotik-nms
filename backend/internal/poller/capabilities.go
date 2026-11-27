package poller

import (
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

// CapState is what a capability probe found for one menu.
type CapState uint8

const (
	CapUnknown           CapState = iota // not probed yet, or the probe did not reach the device
	CapPresent                           // the menu exists (the package is installed)
	CapAbsent                            // "no such command": the package is not installed
	CapPermissionMissing                 // the menu exists but the API user may not read it
	CapEmpty                             // the menu exists and has nothing in it
)

// Installed reports whether the menu exists on the device, with or without
// anything in it. From RouterOS 7.13 every device has /interface/wifi, radios
// or not, so "installed" says little about wireless; "present" — something is
// configured there — is what the reads go by.
func (s CapState) Installed() bool {
	return s == CapPresent || s == CapEmpty
}

func (s CapState) String() string {
	switch s {
	case CapPresent:
		return "present"
	case CapAbsent:
		return "absent"
	case CapPermissionMissing:
		return "permission_missing"
	case CapEmpty:
		return "empty"
	}
	return "unknown"
}

// Caps are the per-device capabilities the pollers gate their reads on.
// Role and capabilities decide which reads apply to a device, never how
// much: a switch is never asked for a registration table — not when the
// wireless package is missing (it would pay for the trap), and not when the
// menu is there with no interface in it (nothing can register on it).
type Caps struct {
	Wifi       CapState // /interface/wifi (RouterOS 7 wifi, CAPsMAN v2)
	CAPsMAN    CapState // /caps-man (legacy CAPsMAN)
	Wireless   CapState // /interface/wireless (legacy wireless)
	DHCPServer CapState // /ip/dhcp-server
	Bridge     CapState // /interface/bridge

	ProbedAt time.Time
	// EmptyAt is when the empty registration menus were last asked again
	// (recheckEmptyMenus); zero until then.
	EmptyAt time.Time
	Version string // RouterOS version the probe ran on
	// Partial marks a probe the device did not finish answering, filled in
	// from the previous one; it is retried soon (probeDue).
	Partial bool
}

// RegistrationMenus lists the registration-table menus worth reading, in
// read order: the wireless menus that have at least one interface. Empty for
// a device without any wireless package, and for one whose wireless menus
// are empty — a switch on RouterOS 7.13 or later, a controller no access
// point has joined yet.
func (c Caps) RegistrationMenus() []string {
	var menus []string
	if c.Wifi == CapPresent {
		menus = append(menus, routeros.RegMenuWifi)
	}
	if c.CAPsMAN == CapPresent {
		menus = append(menus, routeros.RegMenuCAPsMAN)
	}
	if c.Wireless == CapPresent {
		menus = append(menus, routeros.RegMenuWireless)
	}
	return menus
}

// HasWifiRole reports whether the device can have wireless clients at all
// (locally or as a CAPsMAN controller), i.e. whether its log can carry
// wireless events and its registration tables are worth reading.
func (c Caps) HasWifiRole() bool {
	return len(c.RegistrationMenus()) > 0
}

// complete reports whether every menu got a definite answer.
func (c Caps) complete() bool {
	for _, s := range []CapState{c.Wifi, c.CAPsMAN, c.Wireless, c.DHCPServer, c.Bridge} {
		if s == CapUnknown {
			return false
		}
	}
	return true
}

// needsPermissionRetry reports whether any menu was unreadable for lack of
// permission, which an operator may fix at any time.
func (c Caps) needsPermissionRetry() bool {
	for _, s := range []CapState{c.Wifi, c.CAPsMAN, c.Wireless, c.DHCPServer, c.Bridge} {
		if s == CapPermissionMissing {
			return true
		}
	}
	return false
}

const (
	capProbeEvery      = 24 * time.Hour // packages change with upgrades, which also change the version
	capPermissionRetry = time.Hour      // a fixed user policy is picked up within the hour
	capIncompleteRetry = time.Minute    // the probe did not reach the device: try again soon
	capEmptyRetry      = time.Hour      // an empty wireless menu: a first interface (an access point joining its controller) is found within the hour
)

// CapabilityCache remembers each device's probe. Probes run on the first
// sight of a device, every 24 h, when its RouterOS version changes, and
// sooner after an incomplete probe or a permission trap. Safe for
// concurrent use.
type CapabilityCache struct {
	mu   sync.Mutex
	devs map[string]Caps
}

// NewCapabilityCache returns an empty cache.
func NewCapabilityCache() *CapabilityCache {
	return &CapabilityCache{devs: make(map[string]Caps)}
}

// Get returns the cached probe, ok=false when the device was never probed.
func (c *CapabilityCache) Get(deviceID string) (Caps, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	caps, ok := c.devs[deviceID]
	return caps, ok
}

// Ensure returns the device's capabilities, probing first when the cached
// probe is missing or due. The probe is five =count-only= prints, each a
// few milliseconds of device time, so it is cheap even on the weakest
// hardware; what it saves is the trapping prints every cycle.
func (c *CapabilityCache) Ensure(deviceID, version string, run routeros.Runner, now time.Time) Caps {
	c.mu.Lock()
	caps, ok := c.devs[deviceID]
	c.mu.Unlock()
	if ok && !probeDue(caps, version, now) {
		return caps
	}
	if ok && !fullProbeDue(caps, version, now) {
		// Only the empty wireless menus are due: ask for those alone.
		caps = recheckEmptyMenus(run, caps, now)
		c.mu.Lock()
		c.devs[deviceID] = caps
		c.mu.Unlock()
		return caps
	}
	fresh := probeCapabilities(run, version, now)
	if ok && !fresh.complete() {
		// The device did not answer every probe: keep what we knew, but
		// remember the attempt so the next cycle retries without a storm.
		fresh = mergeCaps(caps, fresh)
	}
	c.mu.Lock()
	c.devs[deviceID] = fresh
	c.mu.Unlock()
	return fresh
}

// NoteReadTrap tells the cache that a registration read of menu (RegMenu*),
// sent because the probe found the menu present, trapped as if it were not:
// "no such command" (a package was removed or swapped — legacy wireless for
// wifi-qcom keeps the RouterOS version, so the version check never fires) or
// "not enough permissions" (a policy was taken from the API user). The menu
// takes the state the trap says, and the device is re-probed after
// capIncompleteRetry instead of a day later: until then the gone menu is not
// printed into the trap again, and a menu the device gained (the new wifi
// package) is found within a minute. Any other error (transport) changes
// nothing. It reports whether the cache changed.
func (c *CapabilityCache) NoteReadTrap(deviceID, menu string, err error, now time.Time) bool {
	var state CapState
	switch {
	case routeros.IsNoSuchCommand(err):
		state = CapAbsent
	case routeros.IsPermissionDenied(err):
		state = CapPermissionMissing
	default:
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	caps, ok := c.devs[deviceID]
	if !ok {
		return false
	}
	var field *CapState
	switch menu {
	case routeros.RegMenuWifi:
		field = &caps.Wifi
	case routeros.RegMenuCAPsMAN:
		field = &caps.CAPsMAN
	case routeros.RegMenuWireless:
		field = &caps.Wireless
	default:
		return false
	}
	*field = state
	caps.Partial, caps.ProbedAt = true, now
	c.devs[deviceID] = caps
	return true
}

// Forget drops a device's probe (deleted device, or force a re-probe).
func (c *CapabilityCache) Forget(deviceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.devs, deviceID)
}

// probeDue reports whether the device has to be asked again: the whole probe
// (fullProbeDue), or only its empty wireless menus (emptyRecheckDue).
func probeDue(caps Caps, version string, now time.Time) bool {
	return fullProbeDue(caps, version, now) || emptyRecheckDue(caps, now)
}

// emptyRegistrationMenu reports a wireless menu that is installed and empty.
func (c Caps) emptyRegistrationMenu() bool {
	return c.Wifi == CapEmpty || c.CAPsMAN == CapEmpty || c.Wireless == CapEmpty
}

// emptyRecheckDue: an empty wireless menu is asked again every capEmptyRetry,
// so the first interface in it starts the registration reads within the hour
// rather than at the next daily probe.
func emptyRecheckDue(caps Caps, now time.Time) bool {
	if !caps.emptyRegistrationMenu() {
		return false
	}
	last := caps.ProbedAt
	if caps.EmptyAt.After(last) {
		last = caps.EmptyAt
	}
	return now.Sub(last) >= capEmptyRetry
}

// recheckEmptyMenus asks again for the wireless menus that were installed
// and empty — one =count-only= print each, nothing else of the probe. A menu
// the device does not answer for keeps its state and is asked again after
// another capEmptyRetry.
func recheckEmptyMenus(run routeros.Runner, caps Caps, now time.Time) Caps {
	caps.EmptyAt = now
	for _, p := range []struct {
		state *CapState
		cmd   string
	}{
		{&caps.Wifi, routeros.WifiProbeCommand},
		{&caps.CAPsMAN, routeros.CAPsMANProbeCommand},
		{&caps.Wireless, routeros.WirelessProbeCommand},
	} {
		if *p.state != CapEmpty {
			continue
		}
		state := probeMenu(run, p.cmd)
		if state == CapUnknown {
			break
		}
		*p.state = state
	}
	return caps
}

func fullProbeDue(caps Caps, version string, now time.Time) bool {
	switch {
	case caps.ProbedAt.IsZero():
		return true
	case version != "" && caps.Version != "" && version != caps.Version:
		return true
	case !caps.complete() || caps.Partial:
		return now.Sub(caps.ProbedAt) >= capIncompleteRetry
	case caps.needsPermissionRetry():
		return now.Sub(caps.ProbedAt) >= capPermissionRetry
	}
	return now.Sub(caps.ProbedAt) >= capProbeEvery
}

// mergeCaps keeps the old answer for every menu the new probe left unknown.
func mergeCaps(old, fresh Caps) Caps {
	keep := func(o, f CapState) CapState {
		if f == CapUnknown {
			return o
		}
		return f
	}
	fresh.Wifi = keep(old.Wifi, fresh.Wifi)
	fresh.CAPsMAN = keep(old.CAPsMAN, fresh.CAPsMAN)
	fresh.Wireless = keep(old.Wireless, fresh.Wireless)
	fresh.DHCPServer = keep(old.DHCPServer, fresh.DHCPServer)
	fresh.Bridge = keep(old.Bridge, fresh.Bridge)
	fresh.Partial = true
	return fresh
}

// probeCapabilities runs the five menu probes. A transport failure leaves the
// remaining menus unknown rather than guessing, and stops early: a dead
// session answers nothing.
func probeCapabilities(run routeros.Runner, version string, now time.Time) Caps {
	caps := Caps{ProbedAt: now, Version: version}
	probes := []struct {
		state *CapState
		cmd   string
	}{
		{&caps.Wifi, routeros.WifiProbeCommand},
		{&caps.CAPsMAN, routeros.CAPsMANProbeCommand},
		{&caps.Wireless, routeros.WirelessProbeCommand},
		{&caps.DHCPServer, routeros.DHCPServerProbeCommand},
		{&caps.Bridge, routeros.BridgeProbeCommand},
	}
	for _, p := range probes {
		*p.state = probeMenu(run, p.cmd)
		if *p.state == CapUnknown {
			break
		}
	}
	return caps
}

// probeMenu classifies one =count-only= print: a menu with rows is present,
// one without is empty.
func probeMenu(run routeros.Runner, cmd string) CapState {
	n, err := routeros.CountOnly(run, cmd)
	switch {
	case err == nil && n == 0:
		return CapEmpty
	case err == nil:
		return CapPresent
	case routeros.IsNoSuchCommand(err):
		return CapAbsent
	case routeros.IsPermissionDenied(err):
		return CapPermissionMissing
	}
	return CapUnknown
}
