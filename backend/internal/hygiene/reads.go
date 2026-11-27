// Package hygiene is the pure half of the hygiene audit and the diagnose
// pass (P0-10): which read-only configuration prints a pass sends a device,
// what of the answers is kept as the device's configuration snapshot, and
// the rules that turn a snapshot into findings — settings known to cost CPU
// or to be unsafe, each with a plain explanation and a suggested fix.
//
// It reaches no device and no database: the poller runs the reads
// (routeros.ReadConfig), hands the answers and the facts it already has to
// Evaluate, and stores what comes back. Nothing here ever changes a device;
// a fix is text for an operator.
package hygiene

import (
	"strings"

	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Mode is which pass is running.
type Mode int

const (
	// ModeAudit is the scheduled audit (every 6 h) and an admin's "Audit
	// now": every read of the catalogue that applies to the device.
	ModeAudit Mode = iota
	// ModeDiagnose is the diagnose pass: the shorter list of reads that
	// cover the usual device-side causes of a high CPU.
	ModeDiagnose
)

func (m Mode) String() string {
	if m == ModeDiagnose {
		return "diagnose"
	}
	return "audit"
}

// Section keys: where a read's answer lives in the snapshot.
const (
	SecGraphingInterface  = "graphing_interface"
	SecGraphingResource   = "graphing_resource"
	SecGraphingQueue      = "graphing_queue"
	SecTrafficFlow        = "traffic_flow"
	SecTrafficFlowTargets = "traffic_flow_targets"
	SecSniffer            = "sniffer"
	SecSwitches           = "switches"
	SecSwitchRules        = "switch_rules"
	SecBridgeSettings     = "bridge_settings"
	SecBonding            = "bonding"
	SecSNMP               = "snmp"
	SecSNMPCommunities    = "snmp_communities"
	SecSessions           = "sessions"
	SecFirewallForward    = "firewall_forward"
	SecBridgeFilter       = "bridge_filter"
	SecLogging            = "logging"
	SecLoggingActions     = "logging_actions"
	SecScheduler          = "scheduler"
	SecUserGroups         = "user_groups"
	SecDNS                = "dns"
	SecDHCPServers        = "dhcp_servers"
	SecCAPsMAN            = "capsman"
	SecCAPsMANLegacy      = "capsman_legacy"
	SecDeviceMode         = "device_mode"
)

// Read is one read of a pass.
type Read struct {
	// Key is the section the answer is stored under.
	Key string
	routeros.ConfigRead
	// Volatile fields change without anyone configuring anything
	// (counters). They are kept out of the stored snapshot and its hash; the
	// rules get them through Runtime.
	Volatile []string
	// Runtime marks a whole section as state, not configuration (open
	// sessions, a running sniffer): the rules see it, the snapshot does not.
	Runtime bool
	// Diagnose: the read is also part of the diagnose pass.
	Diagnose bool
	// When decides, with the facts and what the pass has read so far,
	// whether the read is sent at all (nil = always). A read that is not
	// sent costs the device nothing and is recorded as skipped.
	When func(f Facts, got Sections) bool
}

// catalogue lists every read in the order a pass sends them: the diagnose
// reads first — most likely cause first — then the ones only the audit
// makes. No field of any read is a secret: SNMP community names, passwords,
// keys and scheduler scripts are never asked for (see reads_test.go).
var catalogue = []Read{
	// Tools → Graphing: the device samples counters and draws graphs on its
	// own CPU.
	{Key: SecGraphingInterface, Diagnose: true, ConfigRead: routeros.ConfigRead{
		Command: "/tool/graphing/interface/print",
		Fields:  []string{"interface", "allow-address", "store-on-disk", "disabled"}}},
	{Key: SecGraphingResource, Diagnose: true, ConfigRead: routeros.ConfigRead{
		Command: "/tool/graphing/resource/print",
		Fields:  []string{"allow-address", "store-on-disk", "disabled"}}},
	{Key: SecGraphingQueue, Diagnose: true, ConfigRead: routeros.ConfigRead{
		Command: "/tool/graphing/queue/print",
		Fields:  []string{"simple-queue", "allow-address", "store-on-disk", "disabled"}}},

	// Flow export; the targets only matter while it is enabled.
	{Key: SecTrafficFlow, Diagnose: true, ConfigRead: routeros.ConfigRead{
		Command: "/ip/traffic-flow/print",
		Fields:  []string{"enabled", "interfaces", "cache-entries", "active-flow-timeout"}}},
	{Key: SecTrafficFlowTargets, Diagnose: true, When: trafficFlowEnabled, ConfigRead: routeros.ConfigRead{
		Command: "/ip/traffic-flow/target/print",
		Fields:  []string{"dst-address", "port", "version", "disabled"}}},

	{Key: SecSniffer, Diagnose: true, Runtime: true, ConfigRead: routeros.ConfigRead{
		Command: "/tool/sniffer/print", Fields: []string{"running"}}},

	// Switch chip: a mirror to the CPU, and rules that copy or redirect
	// packets to it. The rules are only read where there is a switch.
	{Key: SecSwitches, Diagnose: true, When: mayHaveSwitchChip, ConfigRead: routeros.ConfigRead{
		Command: "/interface/ethernet/switch/print",
		Fields:  []string{"name", "type", "l3-hw-offloading", "mirror-source", "mirror-target", "mirror-egress-target"}}},
	{Key: SecSwitchRules, Diagnose: true, When: hasSwitches, ConfigRead: routeros.ConfigRead{
		Command: "/interface/ethernet/switch/rule/print",
		Fields:  []string{"switch", "ports", "copy-to-cpu", "redirect-to-cpu", "mirror", "disabled", "dynamic"}}},

	// Bridge settings; the fast-path counters say how much the CPU forwards.
	{Key: SecBridgeSettings, Diagnose: true, When: mayHaveBridge,
		Volatile: []string{"bridge-fast-path-active", "bridge-fast-path-packets", "bridge-fast-forward-packets"},
		ConfigRead: routeros.ConfigRead{
			Command: "/interface/bridge/settings/print",
			Fields: []string{"use-ip-firewall", "use-ip-firewall-for-vlan", "allow-fast-path",
				"bridge-fast-path-active", "bridge-fast-path-packets", "bridge-fast-forward-packets"}}},

	{Key: SecBonding, Diagnose: true, When: mayHaveBond, ConfigRead: routeros.ConfigRead{
		Command: "/interface/bonding/print", Fields: []string{"name", "mode", "slaves", "disabled"}}},

	// SNMP: never the community names — only who may ask and how.
	{Key: SecSNMP, Diagnose: true, ConfigRead: routeros.ConfigRead{
		Command: "/snmp/print", Fields: []string{"enabled"}}},
	{Key: SecSNMPCommunities, Diagnose: true, When: snmpEnabled, ConfigRead: routeros.ConfigRead{
		Command: "/snmp/community/print",
		Fields:  []string{"addresses", "security", "read-access", "write-access", "disabled"}}},

	// Who is logged in right now (another API poller; the NMS's own group).
	{Key: SecSessions, Diagnose: true, Runtime: true, ConfigRead: routeros.ConfigRead{
		Command: "/user/active/print", Fields: []string{"name", "address", "via", "group"}}},

	// ---- the audit only ----

	// On a switch: rules that only make sense for traffic the CPU forwards.
	{Key: SecFirewallForward, When: isSwitch, ConfigRead: routeros.ConfigRead{
		Command: "/ip/firewall/filter/print", Query: []string{"?chain=forward"}, CountOnly: true}},
	{Key: SecBridgeFilter, When: isSwitch, ConfigRead: routeros.ConfigRead{
		Command: "/interface/bridge/filter/print", CountOnly: true}},

	// Logging: the actions are only read when a rule logs debug topics.
	{Key: SecLogging, ConfigRead: routeros.ConfigRead{
		Command: "/system/logging/print", Fields: []string{"topics", "action", "disabled"}}},
	{Key: SecLoggingActions, When: logsDebug, ConfigRead: routeros.ConfigRead{
		Command: "/system/logging/action/print", Fields: []string{"name", "target", "remote"}}},

	// Schedulers: name and interval, never the script.
	{Key: SecScheduler, ConfigRead: routeros.ConfigRead{
		Command: "/system/scheduler/print", Fields: []string{"name", "interval", "disabled"}}},

	// What the NMS's own account is allowed to do.
	{Key: SecUserGroups, ConfigRead: routeros.ConfigRead{
		Command: "/user/group/print", Fields: []string{"name", "policy"}}},

	// Services that belong on a router, on a weak switch.
	{Key: SecDNS, When: isWeakSwitch, ConfigRead: routeros.ConfigRead{
		Command: "/ip/dns/print", Fields: []string{"allow-remote-requests"}}},
	{Key: SecDHCPServers, When: isWeakSwitch, ConfigRead: routeros.ConfigRead{
		Command: "/ip/dhcp-server/print", Fields: []string{"interface", "disabled"}}},
	{Key: SecCAPsMAN, When: func(f Facts, _ Sections) bool { return isWeakSwitch(f, nil) && f.WifiPackage },
		ConfigRead: routeros.ConfigRead{Command: "/interface/wifi/capsman/print", Fields: []string{"enabled"}}},
	{Key: SecCAPsMANLegacy, When: func(f Facts, _ Sections) bool { return isWeakSwitch(f, nil) && f.LegacyCAPsMAN },
		ConfigRead: routeros.ConfigRead{Command: "/caps-man/manager/print", Fields: []string{"enabled"}}},

	{Key: SecDeviceMode, When: func(f Facts, _ Sections) bool { return f.Role.NeverExports() },
		ConfigRead: routeros.ConfigRead{
			Command: "/system/device-mode/print",
			Fields:  []string{"mode", "fetch", "bandwidth-test", "email", "traffic-gen", "sniffer"}}},
}

// Plan lists the reads of a pass, in order. Whether each one is sent is
// decided as the pass goes (Read.When).
func Plan(mode Mode) []Read {
	out := make([]Read, 0, len(catalogue))
	for _, r := range catalogue {
		if mode == ModeAudit || r.Diagnose {
			out = append(out, r)
		}
	}
	return out
}

// readOf returns the catalogue entry of a section.
func readOf(key string) (Read, bool) {
	for _, r := range catalogue {
		if r.Key == key {
			return r, true
		}
	}
	return Read{}, false
}

// ---- gates ----

func trafficFlowEnabled(_ Facts, got Sections) bool {
	return routeros.Bool(got.first(SecTrafficFlow)["enabled"])
}

func snmpEnabled(_ Facts, got Sections) bool {
	return routeros.Bool(got.first(SecSNMP)["enabled"])
}

func hasSwitches(_ Facts, got Sections) bool {
	return len(got.rows(SecSwitches)) > 0
}

func logsDebug(_ Facts, got Sections) bool {
	for _, row := range got.rows(SecLogging) {
		if !routeros.Bool(row["disabled"]) && hasDebugTopic(row["topics"]) {
			return true
		}
	}
	return false
}

// The interface facts come from reads the pollers make anyway. While they
// are not known the read is sent: the menu exists on every device and an
// empty table is a cheap answer.
func mayHaveBridge(f Facts, _ Sections) bool     { return !f.InterfacesKnown || f.HasBridge }
func mayHaveBond(f Facts, _ Sections) bool       { return !f.InterfacesKnown || f.HasBond }
func mayHaveSwitchChip(f Facts, _ Sections) bool { return !f.EthernetKnown || f.HasSwitchChip() }

func isSwitch(f Facts, _ Sections) bool { return f.Role == devclass.RoleSwitch }

// isWeakSwitch: switch hardware on a CPU that has nothing to spare — class
// C1, or any MIPS.
func isWeakSwitch(f Facts, _ Sections) bool {
	return f.Role == devclass.RoleSwitch &&
		(f.Class == devclass.C1 || strings.Contains(strings.ToLower(f.Architecture), "mips"))
}

// hasDebugTopic reports a logging rule's topics that include debug (and do
// not exclude it).
func hasDebugTopic(topics string) bool {
	for _, t := range strings.Split(topics, ",") {
		if strings.TrimSpace(t) == "debug" {
			return true
		}
	}
	return false
}
