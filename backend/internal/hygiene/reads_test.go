package hygiene

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

// The catalogue is the list of commands the NMS may send a device for the
// audit. This test spells it out, so a change to what devices receive is a
// change to this file — reviewed, not incidental.
func TestCatalogueIsExactlyTheseReads(t *testing.T) {
	want := []string{
		// the diagnose pass (and the audit)
		"/tool/graphing/interface/print =.proplist=interface,allow-address,store-on-disk,disabled",
		"/tool/graphing/resource/print =.proplist=allow-address,store-on-disk,disabled",
		"/tool/graphing/queue/print =.proplist=simple-queue,allow-address,store-on-disk,disabled",
		"/ip/traffic-flow/print =.proplist=enabled,interfaces,cache-entries,active-flow-timeout",
		"/ip/traffic-flow/target/print =.proplist=dst-address,port,version,disabled",
		"/tool/sniffer/print =.proplist=running",
		"/interface/ethernet/switch/print =.proplist=name,type,l3-hw-offloading,mirror-source,mirror-target,mirror-egress-target",
		"/interface/ethernet/switch/rule/print =.proplist=switch,ports,copy-to-cpu,redirect-to-cpu,mirror,disabled,dynamic",
		"/interface/bridge/settings/print =.proplist=use-ip-firewall,use-ip-firewall-for-vlan,allow-fast-path,bridge-fast-path-active,bridge-fast-path-packets,bridge-fast-forward-packets",
		"/interface/bonding/print =.proplist=name,mode,slaves,disabled",
		"/snmp/print =.proplist=enabled",
		"/snmp/community/print =.proplist=addresses,security,read-access,write-access,disabled",
		"/user/active/print =.proplist=name,address,via,group",
		// the audit only
		"/ip/firewall/filter/print =count-only= ?chain=forward",
		"/interface/bridge/filter/print =count-only=",
		"/system/logging/print =.proplist=topics,action,disabled",
		"/system/logging/action/print =.proplist=name,target,remote",
		"/system/scheduler/print =.proplist=name,interval,disabled",
		"/user/group/print =.proplist=name,policy",
		"/ip/dns/print =.proplist=allow-remote-requests",
		"/ip/dhcp-server/print =.proplist=interface,disabled",
		"/interface/wifi/capsman/print =.proplist=enabled",
		"/caps-man/manager/print =.proplist=enabled",
		"/system/device-mode/print =.proplist=mode,fetch,bandwidth-test,email,traffic-gen,sniffer",
	}
	var got []string
	for _, r := range Plan(ModeAudit) {
		got = append(got, r.ConfigRead.String())
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("audit plan =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	var diagnose []string
	for _, r := range Plan(ModeDiagnose) {
		diagnose = append(diagnose, r.ConfigRead.String())
	}
	if !reflect.DeepEqual(diagnose, want[:13]) {
		t.Fatalf("diagnose plan =\n  %s\nwant the first 13 reads", strings.Join(diagnose, "\n  "))
	}
}

// Every read names its fields (or asks for a count), is a print, and can
// never bring a secret back.
func TestNoReadCanReturnASecret(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range catalogue {
		if seen[r.Key] {
			t.Errorf("section key %q is used twice", r.Key)
		}
		seen[r.Key] = true
		if !strings.HasSuffix(r.Command, "/print") {
			t.Errorf("%s: not a print", r.Command)
		}
		if !r.CountOnly && len(r.Fields) == 0 {
			t.Errorf("%s: no .proplist", r.Command)
		}
		for _, f := range r.Fields {
			if routeros.IsSecretField(f) {
				t.Errorf("%s: asks for the secret field %q", r.Command, f)
			}
		}
		for _, v := range r.Volatile {
			if !contains(r.Fields, v) {
				t.Errorf("%s: volatile field %q is not read", r.Command, v)
			}
		}
		// Fields that are secrets although their name does not say so.
		forbidden := map[string][]string{
			"/snmp/community/print":   {"name"},     // the community string itself
			"/system/scheduler/print": {"on-event"}, // scripts hold credentials
		}
		for _, f := range forbidden[r.Command] {
			if contains(r.Fields, f) {
				t.Errorf("%s: must not ask for %q", r.Command, f)
			}
		}
		// Menus whose every row is a secret are never read at all.
		for _, menu := range []string{"/ppp/secret", "/ip/ipsec", "/interface/wireguard", "/interface/wifi/security",
			"/interface/wireless/security-profiles", "/user/print", "/user/ssh-keys", "/certificate", "/system/script"} {
			if strings.HasPrefix(r.Command, menu) {
				t.Errorf("%s: this menu holds secrets and is not audited", r.Command)
			}
		}
	}
}

func TestGatesDecideWhatIsSent(t *testing.T) {
	sent := func(mode Mode, f Facts, answers Sections) []string {
		var out []string
		got := Sections{}
		for _, r := range Plan(mode) {
			if r.When != nil && !r.When(f, got) {
				got[r.Key] = Section{State: StateSkipped}
				continue
			}
			sec, ok := answers[r.Key]
			if !ok {
				sec = Section{State: routeros.ConfigOK}
			}
			got[r.Key] = sec
			out = append(out, r.Key)
		}
		return out
	}

	// Nothing known about the device: everything a device of unknown role
	// can be asked, and nothing that only applies to a switch.
	got := sent(ModeAudit, Facts{}, Sections{})
	want := []string{SecGraphingInterface, SecGraphingResource, SecGraphingQueue, SecTrafficFlow, SecSniffer, SecSwitches,
		SecBridgeSettings, SecBonding, SecSNMP, SecSessions, SecLogging, SecScheduler, SecUserGroups}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unknown device: sent %v\nwant %v", got, want)
	}

	// A router without bridge, bond or switch chip, everything off.
	router := Facts{Role: "router", Class: "C3", InterfacesKnown: true, EthernetKnown: true}
	got = sent(ModeAudit, router, Sections{})
	want = []string{SecGraphingInterface, SecGraphingResource, SecGraphingQueue, SecTrafficFlow, SecSniffer,
		SecSNMP, SecSessions, SecLogging, SecScheduler, SecUserGroups}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plain router: sent %v\nwant %v", got, want)
	}

	// The follow-up reads are sent only when the first one says they matter.
	answers := Sections{
		SecTrafficFlow: sec(row("enabled", "true")),
		SecSNMP:        sec(row("enabled", "yes")),
		SecLogging:     sec(row("topics", "wireless,debug", "action", "remote")),
		SecSwitches:    sec(row("name", "switch1")),
	}
	weakSwitch := Facts{Role: "switch", Class: "C1", Architecture: "mipsbe", InterfacesKnown: true, EthernetKnown: true,
		HasBridge: true, HasBond: true, WifiPackage: true, Ports: []Port{{Name: "ether1", Type: "ether", Switch: "switch1"}}}
	got = sent(ModeAudit, weakSwitch, answers)
	if len(got) != len(Plan(ModeAudit))-1 { // everything but the legacy CAPsMAN menu
		t.Errorf("weak switch with everything on: sent %d reads %v", len(got), got)
	}
	if got := sent(ModeDiagnose, weakSwitch, answers); len(got) != 13 {
		t.Errorf("diagnose on the same switch: %d reads %v, want 13", len(got), got)
	}
	// A debug rule that is disabled, or one that excludes debug, does not
	// cost the action read.
	for _, topics := range [][]string{{"topics", "debug", "disabled", "true"}, {"topics", "info,!debug"}} {
		answers[SecLogging] = sec(row(topics...))
		if contains(sent(ModeAudit, weakSwitch, answers), SecLoggingActions) {
			t.Errorf("logging %v: the action print was sent", topics)
		}
	}
}
