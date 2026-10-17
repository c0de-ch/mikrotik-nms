package flow

import (
	"strings"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"github.com/mikrotik-nms/backend/internal/database/queries"
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

// fakeIfaceSource serves interface snapshots and VLAN reads by device id.
type fakeIfaceSource struct {
	snaps map[string][]routeros.InterfaceSnapshot
	vlans map[string][]routeros.VLANInfo
}

func (f fakeIfaceSource) DeviceSnapshot(id string) ([]routeros.InterfaceSnapshot, time.Time, bool) {
	rows, ok := f.snaps[id]
	return rows, time.Now(), ok
}

func (f fakeIfaceSource) DeviceVLANs(id string) ([]routeros.VLANInfo, time.Time, bool) {
	v, ok := f.vlans[id]
	return v, time.Now(), ok
}

// The ifIndex sync takes the interface rows from the port-stats snapshot;
// it prints the interface table itself only while no snapshot exists.
func TestExporterIfacesPrefersSnapshot(t *testing.T) {
	run := &recordingRunner{rows: []map[string]string{{".id": "*1", "name": "ether1", "type": "ether"}}}
	src := fakeIfaceSource{snaps: map[string][]routeros.InterfaceSnapshot{"r1": {
		{InterfaceStats: routeros.InterfaceStats{ID: "*1", Name: "eth1-wan", Type: "ether"}},
		{InterfaceStats: routeros.InterfaceStats{ID: "*D", Name: "net28", Type: "vlan"}},
	}}}

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

// The VLAN rows come from the collector's stacking read (Tier 3c), which
// already prints /interface/vlan; the sync prints it itself only before that
// first read.
func TestExporterVLANsPreferStackingRead(t *testing.T) {
	run := &recordingRunner{rows: []map[string]string{{"name": "net28", "vlan-id": "28", "interface": "bridge"}}}
	src := fakeIfaceSource{vlans: map[string][]routeros.VLANInfo{
		"r1": {{Name: "net28", VLANID: 28, Parent: "bridge"}},
		"r3": {}, // read, and the device has no VLANs: still no print
	}}
	for _, id := range []string{"r1", "r3"} {
		if _, err := exporterVLANs(src, run, id); err != nil {
			t.Fatal(err)
		}
	}
	if len(run.calls) != 0 {
		t.Fatalf("calls = %q: the stacking read must spare the VLAN print", run.calls)
	}
	for _, s := range []IfaceSource{src, nil} {
		run.calls = nil
		vlans, err := exporterVLANs(s, run, "r2")
		if err != nil {
			t.Fatal(err)
		}
		if len(run.calls) != 1 || run.calls[0] != "/interface/vlan/print =.proplist=name,vlan-id,interface" {
			t.Fatalf("fallback calls = %q, want one proplisted /interface/vlan/print", run.calls)
		}
		if len(vlans) != 1 || vlans[0] != (routeros.VLANInfo{Name: "net28", VLANID: 28, Parent: "bridge"}) {
			t.Fatalf("fallback rows = %+v", vlans)
		}
	}
}

// Once the collector has read a device, the whole sync runs from memory: no
// pooled connection is even looked up, and the stored rows carry the VLAN
// ids. Without the reads and without a connection it is a silent skip.
func TestSyncExporterIfacesFromTheCollectorsReads(t *testing.T) {
	db := testDB(t)
	if err := queries.CreateDevice(db, &queries.Device{ID: "rb", Identity: "rb", Address: "192.0.2.1", Username: "nms", APIPort: 8728, Status: "online"}); err != nil {
		t.Fatal(err)
	}
	id := seedExporter(t, db, queries.FlowExporter{Name: "rb", Address: "192.0.2.1", Kind: "routeros", DeviceID: "rb"})
	exp, err := queries.GetFlowExporter(db, id)
	if err != nil || exp == nil {
		t.Fatalf("exporter: %v %v", exp, err)
	}
	src := fakeIfaceSource{
		snaps: map[string][]routeros.InterfaceSnapshot{"rb": {
			{InterfaceStats: routeros.InterfaceStats{ID: "*1", Name: "eth1-wan", Type: "ether"}},
			{InterfaceStats: routeros.InterfaceStats{ID: "*D", Name: "net28", Type: "vlan"}},
		}},
		vlans: map[string][]routeros.VLANInfo{"rb": {{Name: "net28", VLANID: 28, Parent: "bridge"}}},
	}
	// A nil pool: any print would fail with errNoClient.
	if err := syncExporterIfaces(db, nil, src, *exp); err != nil {
		t.Fatalf("sync from the collector's reads: %v", err)
	}
	ifs, err := queries.ListFlowIfaces(db, id)
	if err != nil {
		t.Fatal(err)
	}
	byIdx := map[uint32]queries.FlowIface{}
	for _, f := range ifs {
		byIdx[f.IfIndex] = f
	}
	if byIdx[1].Name != "eth1-wan" || byIdx[13].Name != "net28" || byIdx[13].VLANID != 28 || byIdx[13].Parent != "bridge" {
		t.Fatalf("synced ifaces = %+v", ifs)
	}

	// The snapshot without the stacking read yet: the VLAN print would be
	// needed and there is no connection — skipped, the stored VLAN ids kept.
	src.vlans = nil
	if err := syncExporterIfaces(db, nil, src, *exp); err != errNoClient {
		t.Fatalf("sync without the VLAN read = %v, want errNoClient", err)
	}
	after, err := queries.ListFlowIfaces(db, id)
	if err != nil || len(after) != 2 {
		t.Fatalf("ifaces after the skipped sync = %+v (%v)", after, err)
	}
	for _, f := range after {
		if f.IfIndex == 13 && f.VLANID != 28 {
			t.Errorf("the skipped sync wiped net28's VLAN id: %+v", f)
		}
	}
}

func TestUseIfaceSourceNilSafe(t *testing.T) {
	var c *Collector
	c.UseIfaceSource(fakeIfaceSource{}) // must not panic
	c.UseUplinkAge(nil)
}
