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
)

func (s CapState) String() string {
	switch s {
	case CapPresent:
		return "present"
	case CapAbsent:
		return "absent"
	case CapPermissionMissing:
		return "permission_missing"
	}
	return "unknown"
}

// Caps are the per-device capabilities the pollers gate their reads on.
// Role and capabilities decide which reads apply to a device, never how
// much: a switch without the wireless package is never asked for a
// registration table, so it never pays for the trap.
type Caps struct {
	Wifi       CapState // /interface/wifi (RouterOS 7 wifi, CAPsMAN v2)
	CAPsMAN    CapState // /caps-man (legacy CAPsMAN)
	Wireless   CapState // /interface/wireless (legacy wireless)
	DHCPServer CapState // /ip/dhcp-server
	Bridge     CapState // /interface/bridge

	ProbedAt time.Time
	Version  string // RouterOS version the probe ran on
	// Partial marks a probe the device did not finish answering, filled in
	// from the previous one; it is retried soon (probeDue).
	Partial bool
}

// RegistrationMenus lists the registration-table menus the device has, in
// read order. Empty for a device without any wireless package.
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

// Forget drops a device's probe (deleted device, or force a re-probe).
func (c *CapabilityCache) Forget(deviceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.devs, deviceID)
}

func probeDue(caps Caps, version string, now time.Time) bool {
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

// probeMenu classifies one =count-only= print.
func probeMenu(run routeros.Runner, cmd string) CapState {
	_, err := routeros.CountOnly(run, cmd)
	switch {
	case err == nil:
		return CapPresent
	case routeros.IsNoSuchCommand(err):
		return CapAbsent
	case routeros.IsPermissionDenied(err):
		return CapPermissionMissing
	}
	return CapUnknown
}
