package hygiene

import (
	"net/netip"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// ---- builders ----

func row(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func sec(rows ...map[string]string) Section { return Section{State: routeros.ConfigOK, Rows: rows} }

func cnt(n int) Section { return Section{State: routeros.ConfigOK, Count: &n} }

var (
	skippedSec = Section{State: StateSkipped}
	deniedSec  = Section{State: routeros.ConfigDenied}
	failedSec  = Section{State: routeros.ConfigFailed, Error: "i/o timeout"}
	absentSec  = Section{State: routeros.ConfigAbsent}
)

var t0 = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

// cleanAudit is a whole audit of a device on which nothing is configured
// that the rules care about.
func cleanAudit() Sections {
	return Sections{
		SecGraphingInterface: sec(), SecGraphingResource: sec(), SecGraphingQueue: sec(),
		SecTrafficFlow: sec(row("enabled", "false", "interfaces", "all")), SecTrafficFlowTargets: skippedSec,
		SecSniffer:  sec(row("running", "false")),
		SecSwitches: sec(row("name", "switch1", "mirror-source", "none", "mirror-target", "none")), SecSwitchRules: sec(),
		SecBridgeSettings: sec(row("use-ip-firewall", "false", "allow-fast-path", "true",
			"bridge-fast-path-packets", "1000", "bridge-fast-forward-packets", "0")),
		SecBonding: sec(),
		SecSNMP:    sec(row("enabled", "false")), SecSNMPCommunities: skippedSec,
		SecSessions:        sec(row("name", "nms", "address", "192.0.2.10", "via", "api", "group", "nms-read")),
		SecFirewallForward: cnt(0), SecBridgeFilter: cnt(0),
		SecLogging: sec(row("topics", "info", "action", "memory"), row("topics", "critical", "action", "echo")), SecLoggingActions: skippedSec,
		SecScheduler:  sec(),
		SecUserGroups: sec(row("name", "nms-read", "policy", "read,test,api,!write,!policy"), row("name", "full", "policy", "read,write,policy,test,api,ftp,reboot,password,sensitive")),
		SecDNS:        sec(row("allow-remote-requests", "false")), SecDHCPServers: sec(), SecCAPsMAN: skippedSec, SecCAPsMANLegacy: skippedSec,
		SecDeviceMode: sec(row("mode", "advanced", "fetch", "false", "bandwidth-test", "false", "email", "false", "traffic-gen", "false", "sniffer", "false")),
	}
}

func with(s Sections, key string, section Section) Sections {
	out := Sections{}
	for k, v := range s {
		out[k] = v
	}
	out[key] = section
	return out
}

func baseFacts(class devclass.Class, role devclass.Role, board string) Facts {
	return Facts{
		Class: class, Role: role, Board: board, Version: "7.20.4", NMSUser: "nms",
		NMSAddresses: []netip.Addr{netip.MustParseAddr("192.0.2.10")}, Collector: "192.0.2.10", CollectorPort: 2055,
		InterfacesKnown: true, EthernetKnown: true, BridgePortsKnown: true, HasBridge: true, Now: t0,
	}
}

var (
	weakSwitch = func() Facts {
		f := baseFacts(devclass.C1, devclass.RoleSwitch, "CRS354-48G-4S+2Q+")
		f.Architecture = "mipsbe"
		return f
	}
	router = func() Facts { return baseFacts(devclass.C3, devclass.RoleRouter, "RB5009UG+S+") }
	ap     = func() Facts { return baseFacts(devclass.C3, devclass.RoleAPCPE, "cAP ax") }
)

// find returns the finding of a kind, or fails.
func find(t *testing.T, res Result, kind string) Finding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Kind == kind {
			return f
		}
	}
	t.Fatalf("no %s finding among %v", kind, kinds(res))
	return Finding{}
}

func kinds(res Result) []string {
	out := []string{}
	for _, f := range res.Findings {
		out = append(out, f.Kind+":"+f.Severity)
	}
	return out
}

func wantKinds(t *testing.T, res Result, want ...string) {
	t.Helper()
	if got := kinds(res); !reflect.DeepEqual(got, append([]string{}, want...)) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
}

func commandsOf(f Finding) []string {
	var out []string
	for _, step := range f.Fix {
		out = append(out, step.Commands...)
	}
	return out
}

// ---- the clean device ----

func TestCleanDeviceHasNoFindings(t *testing.T) {
	for name, f := range map[string]Facts{"weak switch": weakSwitch(), "router": router(), "access point": ap()} {
		res := Evaluate(ModeAudit, cleanAudit(), f)
		if len(res.Findings) != 0 {
			t.Errorf("%s: findings on a clean device: %v", name, kinds(res))
		}
	}
}

// ---- graphing ----

func TestGraphing(t *testing.T) {
	defaults := row("interface", "all", "allow-address", "0.0.0.0/0", "store-on-disk", "true", "disabled", "false")
	tuned := row("interface", "ether1", "allow-address", "192.0.2.0/24", "store-on-disk", "false", "disabled", "false")

	// Any enabled rule on a weak device is critical — also a tuned one.
	res := Evaluate(ModeAudit, with(cleanAudit(), SecGraphingInterface, sec(tuned)), weakSwitch())
	f := find(t, res, KindGraphing)
	if f.Severity != Critical || !strings.Contains(f.Detail, "interface=ether1") {
		t.Fatalf("weak device: %+v", f)
	}
	if got := commandsOf(f); !reflect.DeepEqual(got, []string{`/tool graphing interface remove [find interface="ether1"]`}) {
		t.Fatalf("fix commands = %q", got)
	}

	// Elsewhere: the default rule (everything, to flash, open) is a warning …
	res = Evaluate(ModeAudit, with(cleanAudit(), SecGraphingInterface, sec(defaults)), router())
	f = find(t, res, KindGraphing)
	if f.Severity != Warn || !strings.Contains(f.Detail, "interface=all") || !strings.Contains(f.Detail, "store-on-disk=yes") ||
		!strings.Contains(f.Detail, "allow-address=0.0.0.0/0") {
		t.Fatalf("router, default rule: %+v", f)
	}
	// … a tuned one is nobody's business, and neither is a disabled one.
	wantKinds(t, Evaluate(ModeAudit, with(cleanAudit(), SecGraphingInterface, sec(tuned)), router()))
	off := row("interface", "all", "store-on-disk", "true", "disabled", "true")
	wantKinds(t, Evaluate(ModeAudit, with(cleanAudit(), SecGraphingInterface, sec(off)), weakSwitch()))

	// Resource and queue rules count too; each is removed by what names it.
	s := with(with(cleanAudit(), SecGraphingResource, sec(row("allow-address", "0.0.0.0/0", "store-on-disk", "true"))),
		SecGraphingQueue, sec(row("simple-queue", "all", "allow-address", "192.0.2.0/24", "store-on-disk", "false")))
	f = find(t, Evaluate(ModeAudit, s, weakSwitch()), KindGraphing)
	want := []string{`/tool graphing resource remove [find allow-address="0.0.0.0/0"]`, `/tool graphing queue remove [find simple-queue="all"]`}
	if got := commandsOf(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("fix commands = %q\nwant %q", got, want)
	}

	// A rule whose name cannot be quoted safely gets words, not a command.
	odd := row("interface", `eth"1; /system reset`, "store-on-disk", "true")
	f = find(t, Evaluate(ModeAudit, with(cleanAudit(), SecGraphingInterface, sec(odd)), weakSwitch()), KindGraphing)
	if got := commandsOf(f); len(got) != 0 {
		t.Fatalf("a command was built from an unsafe name: %q", got)
	}
}

// ---- traffic-flow ----

func TestTrafficFlow(t *testing.T) {
	enabled := with(with(cleanAudit(), SecTrafficFlow, sec(row("enabled", "true", "interfaces", "all"))),
		SecTrafficFlowTargets, sec(row("dst-address", "192.0.2.10", "port", "2055", "version", "ipfix", "disabled", "false")))

	// On a switch, an access point and a weak router alike: critical, with
	// the rollback — whose risky step is flagged before RouterOS 7.25.
	for name, f := range map[string]Facts{"switch": weakSwitch(), "access point": ap(),
		"weak router": baseFacts(devclass.C1, devclass.RoleWeakRouter, "hAP lite")} {
		fnd := find(t, Evaluate(ModeAudit, enabled, f), KindTrafficFlow)
		if fnd.Severity != Critical || !strings.Contains(fnd.Detail, "192.0.2.10:2055") {
			t.Fatalf("%s: %+v", name, fnd)
		}
		var windows int
		for _, step := range fnd.Fix {
			if step.Window {
				windows++
				if !strings.Contains(step.Text, "crash the kernel") {
					t.Errorf("%s: the flagged step does not say why: %q", name, step.Text)
				}
			}
		}
		if windows != 1 {
			t.Errorf("%s: %d steps carry the maintenance-window flag, want 1 (version %s)", name, windows, f.Version)
		}
		cmds := strings.Join(commandsOf(fnd), "\n")
		if !strings.Contains(cmds, "/ip traffic-flow target disable [find dst-address=192.0.2.10 port=2055]") {
			t.Errorf("%s: the rollback does not name the target:\n%s", name, cmds)
		}
	}
	// From 7.25 on switching it off is safe: no window, no download step.
	fixed := weakSwitch()
	fixed.Version = "7.25.1"
	fnd := find(t, Evaluate(ModeAudit, enabled, fixed), KindTrafficFlow)
	for _, step := range fnd.Fix {
		if step.Window || strings.Contains(strings.Join(step.Commands, " "), "package update download") {
			t.Fatalf("7.25: %+v", step)
		}
	}

	// Enabled without a target: still critical on a switch, with only the
	// switch-off step.
	noTarget := with(enabled, SecTrafficFlowTargets, sec())
	fnd = find(t, Evaluate(ModeAudit, noTarget, weakSwitch()), KindTrafficFlow)
	if len(fnd.Fix) != 1 || !fnd.Fix[0].Window || commandsOf(fnd)[len(commandsOf(fnd))-1] != "/ip traffic-flow set enabled=no" {
		t.Fatalf("no target: %+v", fnd.Fix)
	}

	// A router may export. To this NMS: nothing. To somewhere else: a warning
	// that names the target and removes only it.
	wantKinds(t, Evaluate(ModeAudit, enabled, router()))
	foreign := with(enabled, SecTrafficFlowTargets, sec(
		row("dst-address", "192.0.2.10", "port", "2055"), row("dst-address", "198.51.100.7", "port", "9995"),
		row("dst-address", "198.51.100.8", "port", "9995", "disabled", "true")))
	res := Evaluate(ModeAudit, foreign, router())
	wantKinds(t, res, KindTrafficFlowTarget+":"+Warn)
	fnd = find(t, res, KindTrafficFlowTarget)
	if fnd.Detail != "targets: 198.51.100.7:9995" ||
		!reflect.DeepEqual(commandsOf(fnd), []string{"/ip traffic-flow target remove [find dst-address=198.51.100.7 port=9995]"}) {
		t.Fatalf("foreign target: %+v", fnd)
	}
	// Without an advertised collector address the NMS cannot tell which
	// target is its own.
	unset := router()
	unset.Collector = ""
	wantKinds(t, Evaluate(ModeAudit, foreign, unset))
}

// ---- SNMP ----

func TestSNMP(t *testing.T) {
	enabled := with(cleanAudit(), SecSNMP, sec(row("enabled", "true")))
	public := row("addresses", "::/0", "security", "none", "read-access", "true", "write-access", "false")
	restricted := row("addresses", "192.0.2.20/32", "security", "none", "read-access", "true")
	v3 := row("addresses", "::/0", "security", "private", "read-access", "true")
	writable := row("addresses", "192.0.2.20/32", "security", "authorized", "read-access", "true", "write-access", "true")

	// Any SNMP on a weak device: info, whatever the communities.
	res := Evaluate(ModeAudit, with(enabled, SecSNMPCommunities, sec(restricted, v3)), weakSwitch())
	wantKinds(t, res, KindSNMPWeak+":"+Info)

	// Open to the world without authentication: a warning (never critical).
	res = Evaluate(ModeAudit, with(enabled, SecSNMPCommunities, sec(public, restricted, v3)), router())
	wantKinds(t, res, KindSNMPOpen+":"+Warn)
	if f := find(t, res, KindSNMPOpen); !strings.HasPrefix(f.Detail, "1 community") {
		t.Fatalf("open: %+v", f)
	}
	// 0.0.0.0/0 in a list, and no address at all, are just as open.
	for _, addrs := range []string{"192.0.2.20/32,0.0.0.0/0", ""} {
		c := row("addresses", addrs, "security", "none", "read-access", "true")
		wantKinds(t, Evaluate(ModeAudit, with(enabled, SecSNMPCommunities, sec(c)), router()), KindSNMPOpen+":"+Warn)
	}
	// Write access is its own warning; a disabled community is none.
	res = Evaluate(ModeAudit, with(enabled, SecSNMPCommunities, sec(writable)), router())
	wantKinds(t, res, KindSNMPWrite+":"+Warn)
	off := row("addresses", "::/0", "security", "none", "write-access", "true", "disabled", "true")
	wantKinds(t, Evaluate(ModeAudit, with(enabled, SecSNMPCommunities, sec(off)), router()))

	// Disabled SNMP: nothing, whatever is configured underneath.
	wantKinds(t, Evaluate(ModeAudit, with(cleanAudit(), SecSNMPCommunities, sec(public, writable)), weakSwitch()))

	// No finding ever carries a community name: the read does not ask for it.
	for _, f := range Evaluate(ModeAudit, with(enabled, SecSNMPCommunities, sec(public, writable)), weakSwitch()).Findings {
		if strings.Contains(f.Detail+f.Explanation, "public") {
			t.Fatalf("%s mentions a community name: %+v", f.Kind, f)
		}
	}
}

// ---- bridge ----

func TestBridgeIPFirewall(t *testing.T) {
	s := with(cleanAudit(), SecBridgeSettings, sec(row("use-ip-firewall", "true", "use-ip-firewall-for-vlan", "true")))
	f := find(t, Evaluate(ModeAudit, s, weakSwitch()), KindBridgeIPFirewall)
	if f.Severity != Critical || f.Detail != "use-ip-firewall=yes, use-ip-firewall-for-vlan=yes" {
		t.Fatalf("switch: %+v", f)
	}
	if len(f.Fix) != 1 || !f.Fix[0].Window {
		t.Fatalf("the fix changes how traffic is forwarded and must carry the window flag: %+v", f.Fix)
	}
	// Elsewhere it can be wanted: a warning.
	if f := find(t, Evaluate(ModeAudit, s, router()), KindBridgeIPFirewall); f.Severity != Warn {
		t.Fatalf("router: %+v", f)
	}
}

// chipPorts are the bridge members of a switch that all sit on the chip.
func chipPorts() []Port {
	return []Port{
		{Name: "ether1", Type: "ether", Running: true, Switch: "switch1", Bridge: "bridge", HW: true, HWKnown: true, HWOffload: true, HWOffloadKnown: true, Bps: 5000, RateKnown: true},
		{Name: "ether2", Type: "ether", Running: true, Switch: "switch1", Bridge: "bridge", HW: true, HWKnown: true, HWOffload: true, HWOffloadKnown: true, Bps: 9000, RateKnown: true},
		{Name: "ether49", Type: "ether"}, // the CPU-attached management port, not bridged
	}
}

func TestCPUForwarding(t *testing.T) {
	prev := int64(1_000_000)
	counters := func(packets string) Sections {
		return with(cleanAudit(), SecBridgeSettings, sec(row("use-ip-firewall", "false",
			"bridge-fast-path-packets", packets, "bridge-fast-forward-packets", "0")))
	}
	f := weakSwitch()
	f.Ports = chipPorts()
	f.Prev = &Runtime{At: t0.Add(-6 * time.Hour), CPUForwarded: &prev}

	// 6 h at 2,000 packets/s on a bridge whose members are all on the chip:
	// switched traffic is on the CPU — a warning, never critical.
	busy := counters("44200000") // +43.2 M packets in 21,600 s = 2,000/s
	res := Evaluate(ModeAudit, busy, f)
	wantKinds(t, res, KindCPUForwarding+":"+Warn)
	fnd := find(t, res, KindCPUForwarding)
	if !strings.Contains(fnd.Explanation, "about 2000 packets a second") {
		t.Fatalf("explanation = %q", fnd.Explanation)
	}
	// The detail identifies the finding, so it must not carry the measurement.
	if strings.ContainsAny(fnd.Detail, "0123456789") {
		t.Fatalf("detail carries a number and would change every pass: %q", fnd.Detail)
	}

	// The same growth with the management port bridged and up: expected —
	// info only.
	mgmt := f
	mgmt.Ports = append(chipPorts()[:2:2], Port{Name: "ether49", Type: "ether", Running: true, Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true})
	res = Evaluate(ModeAudit, busy, mgmt)
	wantKinds(t, res, KindCPUForwarding+":"+Info)
	if fnd := find(t, res, KindCPUForwarding); !strings.Contains(fnd.Detail, "ether49") {
		t.Fatalf("info: %+v", fnd)
	}
	// A member that is down or disabled in the bridge does not explain it.
	down := f
	down.Ports = append(chipPorts()[:2:2], Port{Name: "ether49", Type: "ether", Running: false, Bridge: "bridge"})
	wantKinds(t, Evaluate(ModeAudit, busy, down), KindCPUForwarding+":"+Warn)

	// Below 1,000 packets/s: nothing.
	wantKinds(t, Evaluate(ModeAudit, counters("11800000"), f)) // 500/s

	// Nothing to compare with, a reboot in between (the counter went back),
	// or readings too close together: the rule cannot tell — it reports
	// nothing and does not claim the finding is gone.
	for name, alt := range map[string]func(*Facts){
		"first pass": func(x *Facts) { x.Prev = nil },
		"reboot": func(x *Facts) {
			big := int64(900_000_000)
			x.Prev = &Runtime{At: t0.Add(-time.Hour), CPUForwarded: &big}
		},
		"too close":   func(x *Facts) { x.Prev = &Runtime{At: t0.Add(-10 * time.Second), CPUForwarded: &prev} },
		"ports unset": func(x *Facts) { x.BridgePortsKnown = false },
	} {
		x := f
		alt(&x)
		res := Evaluate(ModeAudit, busy, x)
		if len(res.Findings) != 0 || contains(res.Evaluated, KindCPUForwarding) {
			t.Errorf("%s: findings %v, evaluated=%v", name, kinds(res), contains(res.Evaluated, KindCPUForwarding))
		}
	}

	// Not a switch that offloads: the counters mean nothing special there.
	r := router()
	r.Prev = f.Prev
	res = Evaluate(ModeAudit, busy, r)
	if len(res.Findings) != 0 || !contains(res.Evaluated, KindCPUForwarding) {
		t.Fatalf("router: findings %v", kinds(res))
	}
}

func TestBondModes(t *testing.T) {
	s := with(cleanAudit(), SecBonding, sec(
		row("name", "bond1", "mode", "802.3ad", "slaves", "ether1,ether2"),
		row("name", "bond2", "mode", "balance-rr", "slaves", "ether3,ether4"),
		row("name", "bond3", "mode", "balance-tlb", "slaves", "ether5,ether6", "disabled", "true"),
		row("name", "bond4", "mode", "active-backup", "slaves", "ether7,ether8"),
		row("name", "bond5", "mode", "balance-xor", "slaves", "ether9,ether10")))
	f := find(t, Evaluate(ModeAudit, s, weakSwitch()), KindBondNotOffloaded)
	if f.Severity != Critical || f.Detail != "bond2 (mode=balance-rr)" {
		t.Fatalf("bond: %+v", f)
	}
	if len(commandsOf(f)) != 0 || !f.Fix[0].Window {
		t.Fatalf("the right mode depends on the other end: words and a window, no command: %+v", f.Fix)
	}
	// A router's CPU forwards its bonds anyway.
	wantKinds(t, Evaluate(ModeAudit, s, router()))
}

func TestOffload(t *testing.T) {
	f := weakSwitch()
	f.Ports = append(chipPorts(),
		// up, carrying traffic, offload requested and not there
		Port{Name: "ether3", Type: "ether", Running: true, Switch: "switch1", Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true, Bps: 800000, RateKnown: true},
		// the same, but idle: nothing is forwarded by the CPU
		Port{Name: "ether4", Type: "ether", Running: true, Switch: "switch1", Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true, RateKnown: true},
		// down: an inactive port is never offloaded
		Port{Name: "ether5", Type: "ether", Switch: "switch1", Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true},
		// just came up: the counters already see it running, but the bridge
		// port table was read while it was still inactive — and an inactive
		// port is never offloaded. The two reads are minutes apart; the rule
		// judges what one of them says.
		Port{Name: "ether7", Type: "ether", Running: true, Switch: "switch1", Bridge: "bridge", BridgeInactive: true, HW: true, HWKnown: true, HWOffloadKnown: true, Bps: 800000, RateKnown: true},
		// hw=no on purpose
		Port{Name: "ether6", Type: "ether", Running: true, Switch: "switch1", Bridge: "bridge", HWKnown: true, HWOffloadKnown: true, Bps: 800000, RateKnown: true},
		// never offloadable: a management port, a VLAN, a wifi interface
		Port{Name: "ether50", Type: "ether", Running: true, Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true, Bps: 800000, RateKnown: true},
		Port{Name: "vlan10", Type: "vlan", Running: true, Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true, Bps: 800000, RateKnown: true},
		Port{Name: "wifi1", Type: "wifi", Running: true, Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true, Bps: 800000, RateKnown: true},
	)
	res := Evaluate(ModeAudit, cleanAudit(), f)
	wantKinds(t, res, KindPortNotOffloaded+":"+Critical)
	fnd := find(t, res, KindPortNotOffloaded)
	if fnd.Detail != "ether3" {
		t.Fatalf("detail = %q, want only ether3", fnd.Detail)
	}
	for _, step := range fnd.Fix {
		if len(step.Commands) > 0 && !step.ReadOnly {
			t.Fatalf("a port finding must not carry a command that changes the device: %+v", step)
		}
	}

	// Horizon and a second bridge with switch ports are warnings.
	g := weakSwitch()
	g.Ports = chipPorts()
	g.Ports[0].Horizon = "1"
	g.Ports[1].Bridge = "bridge2"
	wantKinds(t, Evaluate(ModeAudit, cleanAudit(), g), KindBridgeHorizon+":"+Warn, KindMultipleBridges+":"+Warn)

	// An access point's bridge has wifi ports that are never offloaded: the
	// rule must not speak there at all (v1 raised a critical on every AP).
	a := ap()
	a.Ports = []Port{
		{Name: "ether1", Type: "ether", Running: true, Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true, Bps: 900000, RateKnown: true},
		{Name: "wifi1", Type: "wifi", Running: true, Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true, Bps: 900000, RateKnown: true},
	}
	wantKinds(t, Evaluate(ModeAudit, cleanAudit(), a))
	// Nor on router hardware with a switch chip that does not offload VLANs.
	r := router()
	r.Ports = f.Ports
	wantKinds(t, Evaluate(ModeAudit, cleanAudit(), r))

	// Without the bridge port table the rule cannot judge.
	f.BridgePortsKnown = false
	res = Evaluate(ModeAudit, cleanAudit(), f)
	if len(res.Findings) != 0 || contains(res.Evaluated, KindPortNotOffloaded) {
		t.Fatalf("unknown bridge ports: %v", kinds(res))
	}
}

// ---- switch chip ----

func TestMirrorAndSwitchRules(t *testing.T) {
	s := with(cleanAudit(), SecSwitches, sec(row("name", "switch1", "mirror-source", "ether5", "mirror-target", "cpu")))
	f := find(t, Evaluate(ModeAudit, s, weakSwitch()), KindMirrorToCPU)
	if f.Severity != Critical || f.Detail != "switch1: mirror-target=cpu (mirror-source=ether5)" {
		t.Fatalf("mirror: %+v", f)
	}
	if got := commandsOf(f); !reflect.DeepEqual(got, []string{`/interface ethernet switch set [find name="switch1"] mirror-target=none`}) {
		t.Fatalf("fix = %q", got)
	}
	// A mirror to a port is somebody's capture, not the CPU's problem.
	wantKinds(t, Evaluate(ModeAudit, with(cleanAudit(), SecSwitches, sec(row("name", "switch1", "mirror-source", "ether5", "mirror-target", "ether9"))), weakSwitch()))

	rules := with(cleanAudit(), SecSwitchRules, sec(
		row("switch", "switch1", "ports", "ether1,ether2", "copy-to-cpu", "true"),
		row("switch", "switch1", "ports", "ether3", "redirect-to-cpu", "true", "disabled", "true"), // disabled
		row("switch", "switch1", "ports", "ether4", "redirect-to-cpu", "true", "dynamic", "true"),  // not static
		row("switch", "switch1", "ports", "ether5", "new-vlan-id", "10"),                           // an ordinary ACL
		row("switch", "switch1", "ports", "ether6", "mirror", "true", "redirect-to-cpu", "true")))
	f = find(t, Evaluate(ModeAudit, rules, weakSwitch()), KindSwitchRulesToCPU)
	if f.Severity != Critical || f.Detail != "2 rules: copy-to-cpu on ether1,ether2, redirect-to-cpu+mirror on ether6" {
		t.Fatalf("rules: %+v", f)
	}
	for _, step := range f.Fix {
		if len(step.Commands) > 0 && !step.ReadOnly {
			t.Fatalf("switch rules have no names: only a print may be suggested: %+v", step)
		}
	}
}

func TestPortSettings(t *testing.T) {
	f := weakSwitch()
	for i, name := range []string{"ether1", "ether2", "ether3", "ether4"} {
		f.Ports = append(f.Ports, Port{Name: name, Type: "ether", Running: true, Switch: "switch1", RxFlowControl: "on", TxFlowControl: "off", LoopProtect: "on", Trunk: i == 3})
	}
	f.Ports = append(f.Ports, Port{Name: "sfp1", Type: "ether", Running: true, Switch: "switch1", RxFlowControl: "off", TxFlowControl: "off", LoopProtect: "default", Trunk: true})

	res := Evaluate(ModeAudit, cleanAudit(), f)
	wantKinds(t, res, KindFlowControl+":"+Warn, KindLoopProtectTrunk+":"+Warn)
	fc := find(t, res, KindFlowControl)
	if fc.Detail != "on 4 ports: ether1, ether2, ether3, ether4; links between switches: ether4" {
		t.Fatalf("flow control: %q", fc.Detail)
	}
	lp := find(t, res, KindLoopProtectTrunk)
	if lp.Detail != "ether4" { // loop-protect on the access ports is where it belongs
		t.Fatalf("loop-protect: %q", lp.Detail)
	}
	for _, fnd := range []Finding{fc, lp} {
		if len(commandsOf(fnd)) != 0 || !fnd.Fix[0].Window {
			t.Fatalf("%s: a trunk is not an end-device port — no command, and a window: %+v", fnd.Kind, fnd.Fix)
		}
	}

	// A few access ports with flow control and no trunk among them: nothing.
	g := weakSwitch()
	for _, name := range []string{"ether1", "ether2", "ether3", "ether4"} {
		g.Ports = append(g.Ports, Port{Name: name, Type: "ether", Switch: "switch1", RxFlowControl: "auto", TxFlowControl: "auto"})
	}
	wantKinds(t, Evaluate(ModeAudit, cleanAudit(), g))
	// More than four: "many".
	g.Ports = append(g.Ports, Port{Name: "ether5", Type: "ether", Switch: "switch1", RxFlowControl: "on"})
	wantKinds(t, Evaluate(ModeAudit, cleanAudit(), g), KindFlowControl+":"+Warn)
	// A router's flow control is its own business.
	r := router()
	r.Ports = g.Ports
	wantKinds(t, Evaluate(ModeAudit, cleanAudit(), r))
}

func TestSwitchFilterRules(t *testing.T) {
	s := with(with(cleanAudit(), SecFirewallForward, cnt(12)), SecBridgeFilter, cnt(1))
	f := find(t, Evaluate(ModeAudit, s, weakSwitch()), KindSwitchFilterRules)
	if f.Severity != Warn || f.Detail != "12 forward-chain firewall rules, 1 bridge filter rule" {
		t.Fatalf("%+v", f)
	}
	// The reads are not even sent to a router; were they, it would be fine.
	wantKinds(t, Evaluate(ModeAudit, s, router()))
}

// ---- logging, schedulers ----

func TestDebugLogging(t *testing.T) {
	logging := sec(
		row("topics", "wireless,debug", "action", "remote"),          // built-in remote action
		row("topics", "dhcp,debug", "action", "nms"),                 // a custom action: the action print says where it goes
		row("topics", "bridge,debug", "action", "memory"),            // memory is where debug belongs
		row("topics", "firewall,!debug", "action", "remote"),         // excluded
		row("topics", "debug", "action", "disk", "disabled", "true"), // disabled
		row("topics", "ospf,debug", "action", "flash"))
	actions := sec(row("name", "nms", "target", "remote", "remote", "192.0.2.10"), row("name", "flash", "target", "disk"))
	s := with(with(cleanAudit(), SecLogging, logging), SecLoggingActions, actions)
	f := find(t, Evaluate(ModeAudit, s, router()), KindDebugLogging)
	want := "topics=dhcp,debug → nms (remote), topics=ospf,debug → flash (disk), topics=wireless,debug → remote (remote)"
	if f.Severity != Warn || f.Detail != want {
		t.Fatalf("detail = %q\nwant     %q", f.Detail, want)
	}
}

func TestFastSchedulers(t *testing.T) {
	s := with(cleanAudit(), SecScheduler, sec(
		row("name", "poll", "interval", "10s"),
		row("name", "console-style", "interval", "00:00:30"),
		row("name", "minute", "interval", "1m"),
		row("name", "daily", "interval", "1d"),
		row("name", "at-boot", "interval", "0s"),
		row("name", "off", "interval", "5s", "disabled", "true")))
	f := find(t, Evaluate(ModeAudit, s, router()), KindFastScheduler)
	if f.Detail != "console-style (every 30s), poll (every 10s)" {
		t.Fatalf("detail = %q", f.Detail)
	}
	for in, want := range map[string]time.Duration{"30s": 30 * time.Second, "1m30s": 90 * time.Second, "1d": 24 * time.Hour,
		"00:00:05": 5 * time.Second, "1d 00:00:00": 24 * time.Hour, "01:00:00": time.Hour} {
		if got, ok := parseInterval(in); !ok || got != want {
			t.Errorf("parseInterval(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "soon", "1:2", "x:00:00"} {
		if _, ok := parseInterval(in); ok {
			t.Errorf("parseInterval(%q) parsed", in)
		}
	}
}

// ---- tools, services, accounts ----

func TestSnifferRunning(t *testing.T) {
	f := find(t, Evaluate(ModeAudit, with(cleanAudit(), SecSniffer, sec(row("running", "true"))), weakSwitch()), KindSnifferRunning)
	if f.Severity != Warn || !reflect.DeepEqual(commandsOf(f), []string{"/tool sniffer stop"}) {
		t.Fatalf("%+v", f)
	}
}

func TestCPUServicesOnAWeakSwitch(t *testing.T) {
	s := with(with(with(cleanAudit(), SecDNS, sec(row("allow-remote-requests", "true"))),
		SecDHCPServers, sec(row("interface", "vlan10"), row("interface", "vlan20"), row("interface", "vlan30", "disabled", "true"))),
		SecCAPsMAN, sec(row("enabled", "true")))
	f := find(t, Evaluate(ModeAudit, s, weakSwitch()), KindCPUServices)
	if f.Severity != Warn || f.Detail != "CAPsMAN controller, DNS for clients (allow-remote-requests=yes), 2 DHCP servers" {
		t.Fatalf("%+v", f)
	}
	if !f.Fix[0].Window || len(commandsOf(f)) != 0 {
		t.Fatalf("moving a service interrupts it: %+v", f.Fix)
	}
	// An ARM switch with a capable CPU, and a router, may serve clients.
	strong := baseFacts(devclass.C3, devclass.RoleSwitch, "CRS518-16XS-2XQ")
	strong.Architecture = "arm64"
	wantKinds(t, Evaluate(ModeAudit, s, strong))
	wantKinds(t, Evaluate(ModeAudit, s, router()))
}

func TestNMSAccount(t *testing.T) {
	full := with(cleanAudit(), SecSessions, sec(row("name", "nms", "address", "192.0.2.10", "via", "api", "group", "full")))
	f := find(t, Evaluate(ModeAudit, full, router()), KindNMSAccount)
	if f.Severity != Warn || f.Detail != "user nms in group full: write, policy, password, sensitive, ftp, reboot" {
		t.Fatalf("%+v", f)
	}
	cmds := commandsOf(f)
	if len(cmds) != 2 || !strings.Contains(cmds[1], "address=192.0.2.10") || strings.Contains(strings.Join(cmds, " "), "[find") {
		t.Fatalf("the fix adds an account; it must not touch an existing one: %q", cmds)
	}
	// The read-only group: nothing.
	wantKinds(t, Evaluate(ModeAudit, cleanAudit(), router()))
	// No session of the NMS's own account in the read (another user's
	// credentials, a TLS session listed differently): cannot tell.
	other := with(cleanAudit(), SecSessions, sec(row("name", "admin", "address", "192.0.2.10", "via", "api", "group", "full")))
	res := Evaluate(ModeAudit, other, router())
	if len(res.Findings) != 0 || contains(res.Evaluated, KindNMSAccount) {
		t.Fatalf("unknown group: %v", kinds(res))
	}
}

func TestDeviceMode(t *testing.T) {
	s := with(cleanAudit(), SecDeviceMode, sec(row("mode", "advanced", "fetch", "true", "bandwidth-test", "true", "email", "false", "sniffer", "true", "traffic-gen", "false")))
	for _, f := range []Facts{weakSwitch(), ap()} {
		fnd := find(t, Evaluate(ModeAudit, s, f), KindDeviceMode)
		if fnd.Severity != Info || fnd.Detail != "mode=advanced; allows fetch, bandwidth-test, sniffer" {
			t.Fatalf("%s: %+v", f.Role, fnd)
		}
		if !fnd.Fix[0].Window || !reflect.DeepEqual(commandsOf(fnd), []string{"/system device-mode update fetch=no bandwidth-test=no sniffer=no"}) {
			t.Fatalf("%s: fix %+v", f.Role, fnd.Fix)
		}
	}
	// RouterOS without device-mode: the menu is absent, which is not an error.
	wantKinds(t, Evaluate(ModeAudit, with(cleanAudit(), SecDeviceMode, absentSec), weakSwitch()))
}

func TestForeignAPISessions(t *testing.T) {
	sessions := sec(
		row("name", "nms", "address", "192.0.2.10", "via", "api", "group", "nms-read"),     // ours
		row("name", "zabbix", "address", "198.51.100.20", "via", "api", "group", "read"),   // not ours
		row("name", "zabbix", "address", "198.51.100.20", "via", "api", "group", "read"),   // the same again
		row("name", "script", "address", "::ffff:198.51.100.21", "via", "api-ssl"),         // not ours, TLS
		row("name", "admin", "address", "198.51.100.30", "via", "winbox", "group", "full"), // a person
		row("name", "admin", "address", "", "via", "console", "group", "full"))             // the console
	res := Evaluate(ModeDiagnose, with(cleanAudit(), SecSessions, sessions), router())
	wantKinds(t, res, KindParallelAPIPoller+":"+Warn, KindParallelAPIPoller+":"+Warn)
	// The detail is the login watch's, so both write the same row.
	if d := res.Findings[1].Detail; d != "user zabbix from 198.51.100.20 via api" {
		t.Fatalf("detail = %q", d)
	}
	if !res.Findings[0].Foreign || contains(res.Evaluated, KindParallelAPIPoller) {
		t.Fatal("parallel_api_poller is the login watch's kind: a pass may raise it, never remove it")
	}
	// Without knowing its own addresses the NMS cannot tell whose session it is.
	unknown := router()
	unknown.NMSAddresses = nil
	wantKinds(t, Evaluate(ModeDiagnose, with(cleanAudit(), SecSessions, sessions), unknown))
}

// ---- what a pass could not read ----

// A refused read is reported as such and never read as "clean": the kinds
// behind it are not evaluated, so what was found earlier stays.
func TestDeniedReadIsAFindingNotAClean(t *testing.T) {
	s := with(with(with(cleanAudit(), SecSNMP, sec(row("enabled", "true"))), SecSNMPCommunities, deniedSec), SecUserGroups, deniedSec)
	res := Evaluate(ModeAudit, s, router())
	wantKinds(t, res, KindUnreadable+":"+Info)
	if f := find(t, res, KindUnreadable); f.Detail != "/snmp/community, /user/group" ||
		!strings.Contains(f.Explanation, "not the same as nothing being wrong") {
		t.Fatalf("%+v", f)
	}
	for _, k := range []string{KindSNMPOpen, KindSNMPWrite, KindNMSAccount} {
		if contains(res.Evaluated, k) {
			t.Errorf("%s counts as evaluated although its read was refused", k)
		}
	}
	// What could be read is still judged.
	for _, k := range []string{KindSNMPWeak, KindGraphing, KindUnreadable} {
		if !contains(res.Evaluated, k) {
			t.Errorf("%s was not evaluated", k)
		}
	}
}

func TestFailedReadEvaluatesNothingBehindIt(t *testing.T) {
	s := with(cleanAudit(), SecGraphingQueue, failedSec)
	res := Evaluate(ModeAudit, s, weakSwitch())
	if contains(res.Evaluated, KindGraphing) {
		t.Fatal("graphing counts as evaluated although one of its reads failed")
	}
	if len(res.Findings) != 0 {
		t.Fatalf("findings: %v", kinds(res)) // a failed read is the run's problem, not a finding
	}
	if got := s.Failed(); !reflect.DeepEqual(got, []string{SecGraphingQueue}) {
		t.Fatalf("Failed() = %v", got)
	}
}

// A read the device itself refuses with an error — a property or a query
// this RouterOS version does not have — is not a failure to try again and
// not "nothing found": it is reported, the kinds behind it are left alone,
// and the rest of the audit is whole (the snapshot is stored, with the
// refusal in it).
func TestReadTheRouterOSVersionRefuses(t *testing.T) {
	refused := Section{State: routeros.ConfigUnsupported, Error: "unknown parameter"}
	s := with(with(cleanAudit(), SecDeviceMode, refused), SecUserGroups, deniedSec)
	res := Evaluate(ModeAudit, s, weakSwitch())
	wantKinds(t, res, KindUnreadable+":"+Info)
	f := find(t, res, KindUnreadable)
	if f.Detail != "/user/group; refused by this RouterOS version: /system/device-mode" {
		t.Fatalf("detail = %q", f.Detail)
	}
	if !strings.Contains(f.Explanation, "not enough permissions") || !strings.Contains(f.Explanation, "does not take") {
		t.Fatalf("explanation = %q", f.Explanation)
	}
	if contains(res.Evaluated, KindDeviceMode) || contains(res.Evaluated, KindNMSAccount) {
		t.Fatal("a kind behind a refused read counts as evaluated")
	}
	if got := s.Failed(); len(got) != 0 {
		t.Fatalf("a refused read counts as failed: %v", got)
	}
	snap, complete := SnapshotOf(s)
	if !complete || snap.Sections[SecDeviceMode].State != routeros.ConfigUnsupported {
		t.Fatalf("snapshot: complete=%v device_mode=%+v", complete, snap.Sections[SecDeviceMode])
	}
	// Only the refusal by the device: the finding says so.
	only := Evaluate(ModeAudit, with(cleanAudit(), SecDeviceMode, refused), weakSwitch())
	if f := find(t, only, KindUnreadable); f.Detail != "refused by this RouterOS version: /system/device-mode" {
		t.Fatalf("detail = %q", f.Detail)
	}
}

// Several rules mean something else on a switch than on a router, and some
// apply to one role only. When the pass could not learn the device's role,
// those rules judge nothing — they must not read "no role" as "not a switch"
// and report the switch's findings gone. What does not depend on the role is
// judged as always.
func TestRulesThatDependOnTheRoleJudgeNothingWithoutIt(t *testing.T) {
	byRole := []string{KindBridgeIPFirewall, KindCPUForwarding, KindBondNotOffloaded, KindPortNotOffloaded, KindBridgeHorizon,
		KindMultipleBridges, KindFlowControl, KindSwitchFilterRules, KindCPUServices, KindDeviceMode}
	whatever := []string{KindGraphing, KindSnifferRunning, KindMirrorToCPU, KindSwitchRulesToCPU, KindSNMPOpen, KindSNMPWrite,
		KindSNMPWeak, KindDebugLogging, KindFastScheduler, KindNMSAccount, KindLoopProtectTrunk}

	s, known := everythingWrong(weakSwitch())
	all := Evaluate(ModeAudit, s, known)
	for _, k := range append(append([]string{}, byRole...), whatever...) {
		if !contains(all.Evaluated, k) {
			t.Fatalf("with the role known, %s is not evaluated", k)
		}
	}

	missing := known
	missing.Role, missing.RoleMissing = devclass.RoleUnknown, true
	res := Evaluate(ModeAudit, s, missing)
	for _, k := range byRole {
		if contains(res.Evaluated, k) {
			t.Errorf("%s counts as evaluated although the device's role is not known", k)
		}
		for _, f := range res.Findings {
			if f.Kind == k {
				t.Errorf("%s was reported (%s) although the device's role is not known", k, f.Severity)
			}
		}
	}
	for _, k := range whatever {
		if !contains(res.Evaluated, k) {
			t.Errorf("%s does not depend on the role and was not evaluated", k)
		}
	}

	// Traffic-flow: on C1 hardware it is critical whatever the device does,
	// so the role is not needed; on anything else it is the role that says
	// whether the device may export.
	if f := find(t, res, KindTrafficFlow); f.Severity != Critical {
		t.Errorf("traffic-flow on a C1 device without a role: %+v", f)
	}
	capable := missing
	capable.Class = devclass.C3
	res = Evaluate(ModeAudit, s, capable)
	if contains(res.Evaluated, KindTrafficFlow) || contains(res.Evaluated, KindTrafficFlowTarget) {
		t.Errorf("traffic-flow counts as evaluated on a C3 device whose role is not known: %v", res.Evaluated)
	}
}

// "The NMS may not read this menu" is a statement about the whole audit. An
// audit that stopped early, or one of whose reads got no answer, cannot make
// it: the menu that was refused last time may be among the reads it did not
// get to. Such a pass neither raises, rewrites nor removes the finding.
func TestUnreadableIsJudgedOnlyByAWholeAudit(t *testing.T) {
	// The whole audit, one menu refused: judged.
	whole := Evaluate(ModeAudit, with(cleanAudit(), SecUserGroups, deniedSec), router())
	if !contains(whole.Evaluated, KindUnreadable) || find(t, whole, KindUnreadable).Detail != "/user/group" {
		t.Fatalf("a whole audit: evaluated %v, findings %v", whole.Evaluated, kinds(whole))
	}

	// The audit ended after its first five reads (it made way for a diagnose
	// pass, or lost its session); one of the five was refused.
	early := Sections{}
	for _, r := range Plan(ModeAudit)[:5] {
		early[r.Key] = cleanAudit()[r.Key]
	}
	early[SecGraphingQueue] = deniedSec
	res := Evaluate(ModeAudit, early, router())
	if contains(res.Evaluated, KindUnreadable) {
		t.Error("an audit that stopped early counts as having evaluated audit_unreadable: the stored finding would be removed or rewritten")
	}
	if len(res.Findings) != 0 {
		t.Errorf("an audit that stopped early reported %v", kinds(res))
	}

	// Every read was sent, but one got no answer: the menu behind it may be
	// one the NMS is refused.
	res = Evaluate(ModeAudit, with(with(cleanAudit(), SecUserGroups, deniedSec), SecScheduler, failedSec), router())
	if contains(res.Evaluated, KindUnreadable) || len(res.Findings) != 0 {
		t.Errorf("an audit with a failed read: evaluated %v, findings %v", res.Evaluated, kinds(res))
	}
}

// The diagnose pass reads less; it judges what it read and leaves the rest
// of the device's findings alone.
func TestDiagnoseEvaluatesOnlyWhatItRead(t *testing.T) {
	s := Sections{}
	for _, r := range Plan(ModeDiagnose) {
		s[r.Key] = cleanAudit()[r.Key]
	}
	s[SecGraphingInterface] = sec(row("interface", "all", "store-on-disk", "true"))
	f := weakSwitch()
	f.Ports = chipPorts()
	res := Evaluate(ModeDiagnose, s, f)
	wantKinds(t, res, KindGraphing+":"+Critical)
	for _, k := range []string{KindGraphing, KindTrafficFlow, KindSnifferRunning, KindMirrorToCPU, KindSwitchRulesToCPU,
		KindBridgeIPFirewall, KindBondNotOffloaded, KindSNMPOpen, KindSNMPWeak, KindPortNotOffloaded} {
		if !contains(res.Evaluated, k) {
			t.Errorf("the diagnose pass did not evaluate %s", k)
		}
	}
	for _, k := range []string{KindDebugLogging, KindFastScheduler, KindSwitchFilterRules, KindNMSAccount, KindDeviceMode,
		KindCPUServices, KindUnreadable} {
		if contains(res.Evaluated, k) {
			t.Errorf("the diagnose pass claims to have evaluated %s, which it did not read", k)
		}
	}
}

// ---- every fix, against the hint rules ----

// everythingWrong is a device with every finding the rules know.
func everythingWrong(f Facts) (Sections, Facts) {
	s := cleanAudit()
	s[SecGraphingInterface] = sec(row("interface", "all", "allow-address", "0.0.0.0/0", "store-on-disk", "true"), row("interface", "ether 1", "store-on-disk", "true"))
	s[SecGraphingResource] = sec(row("allow-address", "0.0.0.0/0", "store-on-disk", "true"))
	s[SecGraphingQueue] = sec(row("simple-queue", "all", "allow-address", "0.0.0.0/0", "store-on-disk", "true"))
	s[SecTrafficFlow] = sec(row("enabled", "true", "interfaces", "all"))
	s[SecTrafficFlowTargets] = sec(row("dst-address", "198.51.100.7", "port", "9995", "version", "9"), row("dst-address", "192.0.2.10", "port", "2055"))
	s[SecSniffer] = sec(row("running", "true"))
	s[SecSwitches] = sec(row("name", "switch1", "mirror-source", "ether2", "mirror-target", "cpu"))
	s[SecSwitchRules] = sec(row("switch", "switch1", "ports", "ether1", "copy-to-cpu", "true"))
	s[SecBridgeSettings] = sec(row("use-ip-firewall", "true", "bridge-fast-path-packets", "90000000", "bridge-fast-forward-packets", "0"))
	s[SecBonding] = sec(row("name", "bond1", "mode", "balance-rr", "slaves", "ether7,ether8"))
	s[SecSNMP] = sec(row("enabled", "true"))
	s[SecSNMPCommunities] = sec(row("addresses", "::/0", "security", "none", "read-access", "true", "write-access", "true"))
	s[SecSessions] = sec(row("name", "nms", "address", "192.0.2.10", "via", "api", "group", "full"), row("name", "other", "address", "198.51.100.20", "via", "api"))
	s[SecFirewallForward], s[SecBridgeFilter] = cnt(3), cnt(2)
	s[SecLogging] = sec(row("topics", "debug", "action", "remote"))
	s[SecLoggingActions] = sec(row("name", "remote", "target", "remote"))
	s[SecScheduler] = sec(row("name", "tick", "interval", "5s"))
	s[SecDNS] = sec(row("allow-remote-requests", "true"))
	s[SecDHCPServers] = sec(row("interface", "vlan10"))
	s[SecCAPsMAN] = sec(row("enabled", "true"))
	s[SecDeviceMode] = sec(row("mode", "advanced", "fetch", "true", "sniffer", "true"))

	prev := int64(1000)
	f.Prev = &Runtime{At: f.Now.Add(-time.Hour), CPUForwarded: &prev}
	f.Ports = []Port{
		{Name: "ether1", Type: "ether", Running: true, Switch: "switch1", Bridge: "bridge", HW: true, HWKnown: true, HWOffloadKnown: true, Bps: 1000, RateKnown: true,
			RxFlowControl: "on", LoopProtect: "on", Trunk: true, Horizon: "2"},
		{Name: "ether2", Type: "ether", Running: true, Switch: "switch1", Bridge: "bridge2", HW: true, HWKnown: true, HWOffload: true, HWOffloadKnown: true, Bps: 1000, RateKnown: true},
	}
	return s, f
}

var bareFind = regexp.MustCompile(`\[\s*find\s*\]`)

func TestEveryFixFollowsTheHintRules(t *testing.T) {
	// Kinds whose finding is about ports or links: never a command that
	// changes the device (the ports are not end-device ports).
	portKinds := map[string]bool{KindFlowControl: true, KindLoopProtectTrunk: true, KindPortNotOffloaded: true,
		KindBridgeHorizon: true, KindMultipleBridges: true, KindBondNotOffloaded: true, KindCPUForwarding: true}
	// Kinds whose fix interrupts traffic: a step must carry the window flag.
	windowKinds := map[string]bool{KindBridgeIPFirewall: true, KindBondNotOffloaded: true, KindFlowControl: true,
		KindLoopProtectTrunk: true, KindPortNotOffloaded: true, KindBridgeHorizon: true, KindMultipleBridges: true,
		KindCPUServices: true, KindDeviceMode: true, KindTrafficFlow: true}

	seen := map[string]bool{}
	for _, base := range []Facts{weakSwitch(), router(), ap()} {
		s, f := everythingWrong(base)
		if base.Role == devclass.RoleAPCPE {
			s[SecScheduler] = deniedSec // one device also refuses a read
		}
		res := Evaluate(ModeAudit, s, f)
		for _, fnd := range res.Findings {
			seen[fnd.Kind] = true
			if fnd.Title == "" || fnd.Detail == "" || fnd.Explanation == "" || len(fnd.Fix) == 0 {
				t.Errorf("%s: a finding needs a title, a detail, an explanation and a fix: %+v", fnd.Kind, fnd)
			}
			switch fnd.Severity {
			case Info, Warn, Critical:
			default:
				t.Errorf("%s: severity %q", fnd.Kind, fnd.Severity)
			}
			if !fnd.Foreign && !Owned(fnd.Kind) {
				t.Errorf("%s is neither owned by the audit nor marked foreign", fnd.Kind)
			}
			window := false
			for _, step := range fnd.Fix {
				window = window || step.Window
				if step.Text == "" {
					t.Errorf("%s: a fix step without text", fnd.Kind)
				}
				for _, c := range step.Commands {
					if bareFind.MatchString(c) {
						t.Errorf("%s: bare [find] in %q", fnd.Kind, c)
					}
					if !strings.HasPrefix(c, "/") {
						t.Errorf("%s: %q is not a RouterOS command", fnd.Kind, c)
					}
					isPrint := strings.Contains(c, " print")
					if step.ReadOnly && !isPrint {
						t.Errorf("%s: a read-only step with a command that is not a print: %q", fnd.Kind, c)
					}
					if portKinds[fnd.Kind] && !step.ReadOnly {
						t.Errorf("%s: a command that changes the device on a port finding: %q", fnd.Kind, c)
					}
				}
			}
			if windowKinds[fnd.Kind] && !window {
				t.Errorf("%s: no step carries the maintenance-window flag", fnd.Kind)
			}
		}
	}
	// The three devices together must have shown every kind the audit owns:
	// a rule that is never exercised here is not covered by this test.
	for _, k := range append(Kinds(), KindParallelAPIPoller) {
		if !seen[k] {
			t.Errorf("no test device produced %s", k)
		}
	}
}
