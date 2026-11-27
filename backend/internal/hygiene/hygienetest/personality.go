// Package hygienetest holds device personalities for tests of the hygiene
// audit: what a kind of device — a clean access point, a clean 48-port
// switch — answers to each read of the audit, and what the NMS's scheduled
// reads already know about it. The golden tests run the real read catalogue
// and the real rules over them; a personality can also script a
// routerostest fake, so the poller is tested against the same answers.
//
// The answers are written the way a RouterOS device gives them, and they
// deliberately carry fields the audit must never keep (an SNMP community
// name, a scheduler's script, a user's password hash): a device that
// ignores the proplist must not get a secret into the database.
package hygienetest

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/hygiene"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

//go:embed fixtures/*.json
var fixtures embed.FS

// Reply is a device's answer to one command: rows, a count (for
// =count-only=), or a trap.
type Reply struct {
	Rows  []map[string]string `json:"rows,omitempty"`
	Count *int                `json:"count,omitempty"`
	Trap  string              `json:"trap,omitempty"`
}

// Personality is one kind of device.
type Personality struct {
	Name         string         `json:"name"`
	Board        string         `json:"board"`
	Architecture string         `json:"architecture"`
	Version      string         `json:"version"`
	Class        devclass.Class `json:"class"`
	Role         devclass.Role  `json:"role"`
	NMSUser      string         `json:"nms_user"`
	WifiPackage  bool           `json:"wifi_package"`
	// Ports is what the scheduled reads know: the interface snapshot with
	// its ethernet properties, the bridge port table, the links.
	Ports []hygiene.Port `json:"ports"`
	// Replies answers each command; a command that is not listed traps as
	// "no such command".
	Replies map[string]Reply `json:"replies"`
}

// Names lists the personalities.
func Names() []string {
	entries, err := fixtures.ReadDir("fixtures")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	return out
}

// Load reads a personality by name ("ap_clean", "switch48_clean").
func Load(t testing.TB, name string) Personality {
	t.Helper()
	raw, err := fixtures.ReadFile("fixtures/" + name + ".json")
	if err != nil {
		t.Fatalf("hygienetest: %v", err)
	}
	var p Personality
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("hygienetest: %s: %v", name, err)
	}
	if p.Replies == nil {
		p.Replies = map[string]Reply{}
	}
	return p
}

// NMSAddress is the address the NMS's own sessions come from in every
// personality (documentation range).
const NMSAddress = "192.0.2.10"

// Facts are the facts a pass at now would be given for this device.
func (p Personality) Facts(now time.Time) hygiene.Facts {
	f := hygiene.Facts{
		Class: p.Class, Role: p.Role, RoleReason: "fixture", Board: p.Board, Architecture: p.Architecture,
		Version: p.Version, NMSUser: p.NMSUser, WifiPackage: p.WifiPackage,
		InterfacesKnown: true, EthernetKnown: true, BridgePortsKnown: true,
		Ports: append([]hygiene.Port(nil), p.Ports...), Now: now,
	}
	f.NMSAddresses = append(f.NMSAddresses, netip.MustParseAddr(NMSAddress))
	for _, port := range p.Ports {
		switch port.Type {
		case "bridge":
			f.HasBridge = true
		case "bond":
			f.HasBond = true
		}
		if port.Bridge != "" {
			f.HasBridge = true
		}
	}
	return f
}

// Runner answers like the device: every row whole, whatever the proplist
// says. Calls records each command as sent.
type Runner struct {
	p     Personality
	Calls []string
}

// Runner returns a fresh scripted device link.
func (p Personality) Runner() *Runner { return &Runner{p: p} }

// Run implements routeros.Runner.
func (r *Runner) Run(command string, args ...string) (*ros.Reply, error) {
	r.Calls = append(r.Calls, strings.TrimSpace(command+" "+strings.Join(args, " ")))
	reply, ok := r.p.Replies[command]
	if !ok {
		return nil, trap("no such command prefix")
	}
	if reply.Trap != "" {
		return nil, trap(reply.Trap)
	}
	out := &ros.Reply{Done: &proto.Sentence{Word: "!done", Map: map[string]string{}}}
	for _, a := range args {
		if a == "=count-only=" {
			n := len(reply.Rows)
			if reply.Count != nil {
				n = *reply.Count
			}
			out.Done.Map["ret"] = fmt.Sprint(n)
			return out, nil
		}
	}
	for _, row := range reply.Rows {
		out.Re = append(out.Re, &proto.Sentence{Word: "!re", Map: row})
	}
	return out, nil
}

func trap(message string) error {
	return &ros.DeviceError{Sentence: &proto.Sentence{Word: "!trap", Map: map[string]string{"message": message}}}
}

// With returns a copy of the personality in which command is answered with
// rows.
func (p Personality) With(command string, rows ...map[string]string) Personality {
	return p.WithReply(command, Reply{Rows: rows})
}

// WithReply returns a copy of the personality in which command is answered
// with reply.
func (p Personality) WithReply(command string, reply Reply) Personality {
	replies := make(map[string]Reply, len(p.Replies)+1)
	for k, v := range p.Replies {
		replies[k] = v
	}
	replies[command] = reply
	p.Replies = replies
	return p
}

// WithPort returns a copy of the personality in which the named port is
// changed by edit (a port that does not exist is added).
func (p Personality) WithPort(name string, edit func(*hygiene.Port)) Personality {
	ports := append([]hygiene.Port(nil), p.Ports...)
	found := false
	for i := range ports {
		if ports[i].Name == name {
			edit(&ports[i])
			found = true
		}
	}
	if !found {
		port := hygiene.Port{Name: name}
		edit(&port)
		ports = append(ports, port)
	}
	p.Ports = ports
	return p
}

// Script makes a routerostest fake answer like this personality. A command
// the personality does not list traps as "no such command", as a device
// without that menu does.
func (p Personality) Script(d *routerostest.Device) {
	for _, read := range hygiene.Plan(hygiene.ModeAudit) {
		reply, ok := p.Replies[read.Command]
		switch {
		case !ok:
			d.SetTrap(read.Command, "no such command prefix")
		case reply.Trap != "":
			d.SetTrap(read.Command, reply.Trap)
		default:
			d.SetReply(read.Command, reply.Rows...)
			if read.CountOnly {
				n := len(reply.Rows)
				if reply.Count != nil {
					n = *reply.Count
				}
				d.SetCount(read.Command, n)
			}
		}
	}
}

// Pass runs a whole pass against the personality — the real catalogue, the
// real gates, the real read — and returns what it read.
func (p Personality) Pass(mode hygiene.Mode, f hygiene.Facts) (hygiene.Sections, *Runner) {
	run := p.Runner()
	got := hygiene.Sections{}
	for _, read := range hygiene.Plan(mode) {
		if read.When != nil && !read.When(f, got) {
			got[read.Key] = hygiene.Section{State: hygiene.StateSkipped}
			continue
		}
		got[read.Key] = hygiene.SectionOf(read, routeros.ReadConfig(run, read.ConfigRead))
	}
	return got, run
}
