package hygiene_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/hygiene"
	"github.com/mikrotik-nms/backend/internal/hygiene/hygienetest"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

var now = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

func kinds(res hygiene.Result) []string {
	out := []string{}
	for _, f := range res.Findings {
		out = append(out, f.Kind+":"+f.Severity)
	}
	return out
}

// The golden fixtures: a clean access point and a clean 48-port switch run
// through the real catalogue and the real rules. Neither may yield a
// critical finding — v1 of the rules raised one on every access point's
// bridge, because its wifi ports are never hardware-offloaded.
func TestGoldenCleanDevicesHaveNoCriticalFinding(t *testing.T) {
	for name, want := range map[string][]string{
		// Device-mode is the one thing a factory-fresh device has to report.
		"ap_clean":       {hygiene.KindDeviceMode + ":" + hygiene.Info},
		"switch48_clean": {hygiene.KindDeviceMode + ":" + hygiene.Info},
	} {
		p := hygienetest.Load(t, name)
		for _, mode := range []hygiene.Mode{hygiene.ModeAudit, hygiene.ModeDiagnose} {
			facts := p.Facts(now)
			sections, _ := p.Pass(mode, facts)
			if failed := sections.Failed(); len(failed) > 0 {
				t.Fatalf("%s/%s: reads failed: %v", name, mode, failed)
			}
			res := hygiene.Evaluate(mode, sections, facts)
			for _, f := range res.Findings {
				if f.Severity == hygiene.Critical {
					t.Errorf("%s/%s: critical finding on a clean device: %s — %s", name, mode, f.Kind, f.Detail)
				}
			}
			if mode == hygiene.ModeDiagnose {
				want = []string{}
			}
			if got := kinds(res); !reflect.DeepEqual(got, want) {
				t.Errorf("%s/%s: findings = %v, want %v", name, mode, got, want)
			}
		}
	}
}

// What each clean device is actually sent per pass: the device budget of the
// audit (design §9.6: about 15–18 reads per 6 h) and of the diagnose pass
// (about 12). A change here is a change to what devices receive.
func TestGoldenReadCounts(t *testing.T) {
	for _, c := range []struct {
		name            string
		audit, diagnose int
	}{
		// No switch chip, no bond, SNMP and traffic-flow off, not a switch:
		// 3 graphing, traffic-flow, sniffer, bridge settings, SNMP, sessions;
		// then logging, scheduler, user groups, device-mode.
		{"ap_clean", 12, 8},
		// A switch chip (+ its rules) and bonds on top, and the reads only a
		// switch gets: forward-chain and bridge-filter counts, DNS, DHCP
		// servers. No wifi menu, so no CAPsMAN read.
		{"switch48_clean", 19, 11},
	} {
		p := hygienetest.Load(t, c.name)
		_, run := p.Pass(hygiene.ModeAudit, p.Facts(now))
		if len(run.Calls) != c.audit {
			t.Errorf("%s: the audit sent %d commands, want %d:\n  %s", c.name, len(run.Calls), c.audit, strings.Join(run.Calls, "\n  "))
		}
		_, run = p.Pass(hygiene.ModeDiagnose, p.Facts(now))
		if len(run.Calls) != c.diagnose {
			t.Errorf("%s: the diagnose pass sent %d commands, want %d:\n  %s", c.name, len(run.Calls), c.diagnose, strings.Join(run.Calls, "\n  "))
		}
		for _, call := range run.Calls {
			if !strings.Contains(call, "=.proplist=") && !strings.Contains(call, "=count-only=") {
				t.Errorf("%s: %q carries neither a proplist nor count-only", c.name, call)
			}
		}
	}
}

// The fast-path rule on the 48-port switch (design §9.6): with its 10/100
// management port bridged and up, growing counters are expected — info and
// nothing else; with only switch-chip members the same growth is a warning.
// Never a critical.
func TestGoldenSwitchFastPath(t *testing.T) {
	base := hygienetest.Load(t, "switch48_clean")
	growing := base.With("/interface/bridge/settings/print", map[string]string{
		"use-ip-firewall": "false", "use-ip-firewall-for-vlan": "false", "allow-fast-path": "true",
		"bridge-fast-path-active": "true", "bridge-fast-path-packets": "95230000", "bridge-fast-forward-packets": "880000",
	}) // +54 M packets since the previous pass
	prev := int64(41230000 + 880000)

	run := func(p hygienetest.Personality) hygiene.Result {
		facts := p.Facts(now)
		facts.Prev = &hygiene.Runtime{At: now.Add(-6 * time.Hour), CPUForwarded: &prev} // 54 M in 6 h = 2,500/s
		sections, _ := p.Pass(hygiene.ModeAudit, facts)
		return hygiene.Evaluate(hygiene.ModeAudit, sections, facts)
	}

	mgmtBridged := growing.WithPort("ether49", func(port *hygiene.Port) {
		port.Running, port.Bridge, port.HW, port.HWKnown, port.HWOffloadKnown = true, "bridge", true, true, true
		port.Bps, port.RateKnown = 40000, true
	})
	want := []string{hygiene.KindCPUForwarding + ":" + hygiene.Info, hygiene.KindDeviceMode + ":" + hygiene.Info}
	if got := kinds(run(mgmtBridged)); !reflect.DeepEqual(got, want) {
		t.Errorf("management port bridged: findings = %v, want %v", got, want)
	}

	want = []string{hygiene.KindCPUForwarding + ":" + hygiene.Warn, hygiene.KindDeviceMode + ":" + hygiene.Info}
	if got := kinds(run(growing)); !reflect.DeepEqual(got, want) {
		t.Errorf("switch-chip members only: findings = %v, want %v", got, want)
	}
}

// The reason the audit exists: a forgotten graphing rule on the weak switch.
func TestGoldenSwitchWithGraphing(t *testing.T) {
	p := hygienetest.Load(t, "switch48_clean").With("/tool/graphing/interface/print",
		map[string]string{".id": "*1", "interface": "all", "allow-address": "0.0.0.0/0", "store-on-disk": "true", "disabled": "false"})
	for _, mode := range []hygiene.Mode{hygiene.ModeAudit, hygiene.ModeDiagnose} {
		facts := p.Facts(now)
		sections, _ := p.Pass(mode, facts)
		res := hygiene.Evaluate(mode, sections, facts)
		if len(res.Findings) == 0 || res.Findings[0].Kind != hygiene.KindGraphing || res.Findings[0].Severity != hygiene.Critical {
			t.Fatalf("%s: findings = %v, want graphing first and critical", mode, kinds(res))
		}
	}
}

// A device answers with whole rows — community names, passwords, scripts.
// None of it may reach the snapshot or a finding.
func TestGoldenNothingSecretIsKept(t *testing.T) {
	for _, name := range hygienetest.Names() {
		p := hygienetest.Load(t, name).
			With("/snmp/print", map[string]string{"enabled": "true", "trap-community": "DO-NOT-STORE-trap"}).
			With("/system/scheduler/print", map[string]string{"name": "backup", "interval": "5s",
				"on-event": "/tool fetch url=ftp://198.51.100.9 user=x password=DO-NOT-STORE-script"}).
			With("/ip/traffic-flow/print", map[string]string{"enabled": "true", "interfaces": "all"})
		facts := p.Facts(now)
		sections, _ := p.Pass(hygiene.ModeAudit, facts)
		snap, _ := hygiene.SnapshotOf(sections)
		res := hygiene.Evaluate(hygiene.ModeAudit, sections, facts)
		kept := snap.JSON()
		for _, f := range res.Findings {
			kept += f.Title + f.Detail + f.Explanation + f.FixJSON()
		}
		if strings.Contains(kept, "DO-NOT-STORE") {
			t.Errorf("%s: a secret was kept:\n%s", name, kept)
		}
		// And no stored row has a field whose name says "secret", whatever
		// its value: the community's name, a password, a script.
		for key, section := range snap.Sections {
			for _, row := range section.Rows {
				for field := range row {
					if routeros.IsSecretField(field) || field == "on-event" || (key == hygiene.SecSNMPCommunities && field == "name") {
						t.Errorf("%s: the snapshot's %s section holds the field %q", name, key, field)
					}
				}
			}
		}
		if len(snap.Sections[hygiene.SecSNMPCommunities].Rows) == 0 {
			t.Fatalf("%s: the community read was not part of the pass — the test checks nothing", name)
		}
	}
}
