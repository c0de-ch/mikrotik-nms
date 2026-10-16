package flow

import (
	"strings"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// recordingRunner answers every command with rows and records it.
type recordingRunner struct {
	rows  []map[string]string
	calls []string
}

func (r *recordingRunner) Run(command string, args ...string) (*ros.Reply, error) {
	r.calls = append(r.calls, strings.TrimSpace(command+" "+strings.Join(args, " ")))
	reply := &ros.Reply{Done: &proto.Sentence{Word: "!done", Map: map[string]string{}}}
	for _, m := range r.rows {
		reply.Re = append(reply.Re, &proto.Sentence{Word: "!re", Map: m})
	}
	return reply, nil
}

type fakeIfaceSource map[string][]routeros.InterfaceSnapshot

func (f fakeIfaceSource) DeviceSnapshot(id string) ([]routeros.InterfaceSnapshot, time.Time, bool) {
	rows, ok := f[id]
	return rows, time.Now(), ok
}

// The ifIndex sync takes the interface rows from the port-stats snapshot;
// it prints the interface table itself only while no snapshot exists.
func TestExporterIfacesPrefersSnapshot(t *testing.T) {
	run := &recordingRunner{rows: []map[string]string{{".id": "*1", "name": "ether1", "type": "ether"}}}
	src := fakeIfaceSource{"r1": {
		{InterfaceStats: routeros.InterfaceStats{ID: "*1", Name: "eth1-wan", Type: "ether"}},
		{InterfaceStats: routeros.InterfaceStats{ID: "*D", Name: "net28", Type: "vlan"}},
	}}

	stats, err := exporterIfaces(src, run, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("calls = %q: a snapshot must spare the device print", run.calls)
	}
	if got := ifacesFromStats(stats, nil); len(got) != 2 || got[1].IfIndex != 13 || got[1].Name != "net28" {
		t.Fatalf("ifaces from the snapshot = %+v", got)
	}

	// No snapshot (cold start, or no source wired): one Tier 0 print.
	for _, s := range []IfaceSource{src, nil} {
		run.calls = nil
		stats, err := exporterIfaces(s, run, "r2")
		if err != nil {
			t.Fatal(err)
		}
		if len(run.calls) != 1 || !strings.HasPrefix(run.calls[0], "/interface/print =.proplist=.id,name,type,") {
			t.Fatalf("fallback calls = %q, want one proplisted /interface/print", run.calls)
		}
		if len(stats) != 1 || stats[0].Name != "ether1" {
			t.Fatalf("fallback rows = %+v", stats)
		}
	}
}

func TestUseIfaceSourceNilSafe(t *testing.T) {
	var c *Collector
	c.UseIfaceSource(fakeIfaceSource{}) // must not panic
}
