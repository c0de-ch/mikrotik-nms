package flow

import (
	"database/sql"
	"log"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// Plan refresh cadence (§7.1).
const (
	planFirstDelay = 60 * time.Second
	planInterval   = 15 * time.Minute
	// planRetry re-runs the plan soon when an answering device's uplink rows
	// were older than uplinkFreshness. After a restart that followed a longer
	// downtime the topology poller refreshes them within about a minute; a
	// plan built before that classifies WAN prefixes (default-route
	// interfaces) as internal for a whole planInterval. Bounded by
	// planMaxRetries so a device whose routes cannot be read does not keep
	// the plan on a 60 s cadence.
	planRetry      = 60 * time.Second
	planMaxRetries = 5
)

// planAddr is one address a managed device reported.
type planAddr struct {
	host     netip.Addr   // the configured address
	prefix   netip.Prefix // its network (masked)
	deviceID string
	iface    string
	ifType   string // from the interfaces table
}

var planClassRank = map[string]int{"transit": 1, "vpn": 2, "internal": 3}

// buildPlan classifies the reported addresses (contract §7.1):
//  1. vpn when the interface is of a VPN type or a fresh vpn uplink;
//  2. transit when it is a fresh non-VPN default-route uplink whose gateway
//     does not lie in a shared prefix of that interface (a LAN client's
//     default route via the LAN router), no other device reported an
//     address inside the prefix and no other device's management address
//     lies inside it;
//  3. internal otherwise.
//
// Duplicates of a prefix merge internal > vpn > transit, except that a
// prefix inside a flow_external_prefixes entry is never upgraded to
// internal. Every address also gets a host-length row (/32, /128) with the
// class of the most specific prefix holding it: classification does not
// change, and the views recognise the device's own interface addresses as
// "self". deviceAddrs maps device id -> devices.address.
func buildPlan(addrs []planAddr, deviceAddrs map[string]netip.Addr, uplinks []queries.DeviceUplink,
	external []netip.Prefix, now time.Time) []queries.FlowPrefix {
	type upKey struct{ device, iface string }
	vpnUp, wanUp := map[upKey]bool{}, map[upKey]bool{}
	for _, u := range uplinks {
		k := upKey{u.DeviceID, u.Interface}
		switch u.Kind {
		case "vpn":
			vpnUp[k] = true
		case "default-route":
			if !topology.IsVPNIfaceType(u.IfaceType) {
				wanUp[k] = true
			}
		}
	}
	insideExternal := func(p netip.Prefix) bool {
		for _, e := range external {
			if e.Bits() <= p.Bits() && e.Contains(p.Addr()) {
				return true
			}
		}
		return false
	}
	shared := func(a planAddr) bool {
		for _, o := range addrs {
			if o.deviceID != a.deviceID && a.prefix.Contains(o.host) {
				return true
			}
		}
		for id, da := range deviceAddrs {
			if id != a.deviceID && da.IsValid() && a.prefix.Contains(da) {
				return true
			}
		}
		return false
	}
	// A default route whose gateway lies in a shared prefix of the same
	// interface is a LAN client's route via the LAN router (a switch on the
	// LAN, a firewall's LAN leg), not an Internet uplink.
	for _, u := range uplinks {
		k := upKey{u.DeviceID, u.Interface}
		if !wanUp[k] {
			continue
		}
		gwStr, _, _ := strings.Cut(strings.TrimSpace(u.GatewayIP), "%")
		gw, err := netip.ParseAddr(gwStr)
		if err != nil {
			continue
		}
		gw = gw.Unmap()
		for _, a := range addrs {
			if a.deviceID == u.DeviceID && a.iface == u.Interface && a.prefix.Contains(gw) && shared(a) {
				delete(wanUp, k)
				break
			}
		}
	}
	rank := func(r queries.FlowPrefix) int {
		if r.Class == "internal" && insideExternal(r.Prefix) {
			return 0
		}
		return planClassRank[r.Class]
	}
	best := map[netip.Prefix]queries.FlowPrefix{}
	merge := func(row queries.FlowPrefix) {
		if cur, ok := best[row.Prefix]; !ok || rank(row) > rank(cur) {
			best[row.Prefix] = row
		}
	}
	for _, a := range addrs {
		k := upKey{a.deviceID, a.iface}
		class := "internal"
		switch {
		case topology.IsVPNIfaceType(a.ifType) || vpnUp[k]:
			class = "vpn"
		case wanUp[k] && !shared(a):
			class = "transit"
		}
		merge(queries.FlowPrefix{Prefix: a.prefix, Class: class, DeviceID: a.deviceID, Iface: a.iface, UpdatedAt: now})
	}
	// Host rows: the class of the most specific (merged) prefix holding the
	// address; a configured /32 or /128 already is one.
	nets := make([]queries.FlowPrefix, 0, len(best))
	for _, r := range best {
		nets = append(nets, r)
	}
	slices.SortFunc(nets, func(a, b queries.FlowPrefix) int { return b.Prefix.Bits() - a.Prefix.Bits() })
	for _, a := range addrs {
		host := netip.PrefixFrom(a.host, a.host.BitLen())
		if _, ok := best[host]; ok {
			continue
		}
		for _, r := range nets {
			if r.Prefix.Contains(a.host) {
				merge(queries.FlowPrefix{Prefix: host, Class: r.Class, DeviceID: a.deviceID, Iface: a.iface, UpdatedAt: now})
				break
			}
		}
	}
	out := make([]queries.FlowPrefix, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b queries.FlowPrefix) int {
		if c := a.Prefix.Addr().Compare(b.Prefix.Addr()); c != 0 {
			return c
		}
		return a.Prefix.Bits() - b.Prefix.Bits()
	})
	return out
}

// planAddrsOf converts a device's reported addresses (skipping parse
// errors, link-local and loopback). A host-length global IPv6 address (a
// DHCPv6 IA_NA /128 on a LAN client) is widened to its /64: nothing else can
// lie inside a /128, so the LAN /64 would never be learned and the LAN's
// IPv6 hosts would classify as external. Other lengths (e.g. a pool /80)
// are kept as configured.
func planAddrsOf(deviceID string, list []routeros.DeviceAddress, types map[string]string) []planAddr {
	var out []planAddr
	for _, a := range list {
		p, err := netip.ParsePrefix(a.Address)
		if err != nil {
			continue
		}
		host := p.Addr().Unmap()
		if host.IsLinkLocalUnicast() || host.IsLoopback() || host.IsUnspecified() {
			continue
		}
		p = netip.PrefixFrom(host, p.Bits()-(p.Addr().BitLen()-host.BitLen())).Masked()
		if host.Is6() && p.Bits() == 128 && host.IsGlobalUnicast() {
			p = netip.PrefixFrom(host, 64).Masked()
		}
		out = append(out, planAddr{host: host, prefix: p, deviceID: deviceID, iface: a.Interface, ifType: types[a.Interface]})
	}
	return out
}

// staleUplinks reports whether a device that answered the address query
// has default-route / vpn uplink rows, none of them fresh: its egress
// classification (transit / vpn) is then not known yet.
func staleUplinks(answered map[string]bool, fresh, all []queries.DeviceUplink) bool {
	hasFresh, hasAny := map[string]bool{}, map[string]bool{}
	for _, u := range fresh {
		hasFresh[u.DeviceID] = true
	}
	for _, u := range all {
		if u.Kind == "default-route" || u.Kind == "vpn" {
			hasAny[u.DeviceID] = true
		}
	}
	for id := range answered {
		if hasAny[id] && !hasFresh[id] {
			return true
		}
	}
	return false
}

// refreshPlan reads every online device's addresses and replaces
// flow_prefixes, keeping the previous plan when no device answered. It
// returns true when the plan was built while an answering device's uplink
// rows were stale (the caller retries after planRetry).
func refreshPlan(db *sql.DB, pool *routeros.Pool, now time.Time) (retry bool) {
	if db == nil || pool == nil {
		return false
	}
	devices, err := queries.ListDevices(db)
	if err != nil {
		log.Printf("flow plan: list devices: %v", err)
		return false
	}
	deviceAddrs := map[string]netip.Addr{}
	var addrs []planAddr
	answered := map[string]bool{}
	for _, d := range devices {
		if a, err := netip.ParseAddr(d.Address); err == nil {
			deviceAddrs[d.ID] = a.Unmap()
		}
		if d.Status != "online" {
			continue
		}
		client := pool.GetLive(d.ID)
		if client == nil {
			continue
		}
		list, err := routeros.ListAddresses(client)
		if err != nil {
			log.Printf("flow plan: addresses of %s: %v", d.Identity, err)
			continue
		}
		answered[d.ID] = true
		types := map[string]string{}
		if ifs, err := queries.ListInterfacesByDevice(db, d.ID); err == nil {
			for _, i := range ifs {
				types[i.Name] = i.Type
			}
		}
		addrs = append(addrs, planAddrsOf(d.ID, list, types)...)
	}
	if len(answered) == 0 {
		return false
	}
	uplinks, err := queries.ListDeviceUplinks(db, uplinkFreshness)
	if err != nil {
		log.Printf("flow plan: uplinks: %v", err)
		return false
	}
	all, err := queries.ListDeviceUplinks(db, 365*24*time.Hour)
	if err != nil {
		log.Printf("flow plan: uplinks: %v", err)
		return false
	}
	retry = staleUplinks(answered, uplinks, all)
	plan := buildPlan(addrs, deviceAddrs, uplinks, prefixListSetting(db, "flow_external_prefixes"), now)
	if err := queries.ReplaceFlowPrefixes(db, plan); err != nil {
		log.Printf("flow plan: store: %v", err)
		return retry
	}
	if retry {
		log.Printf("flow plan: %d prefixes from %d devices (uplinks not refreshed yet; retrying in %s)", len(plan), len(answered), planRetry)
	} else {
		log.Printf("flow plan: %d prefixes from %d devices", len(plan), len(answered))
	}
	return retry
}
