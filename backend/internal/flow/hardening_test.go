package flow

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow/decode"
	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
)

// Regression tests for the collector's resource bounds and aggregation edge
// cases (review of phase 2).

// ifSprayPacket is an IPFIX message with the (in_if, out_if, octets)
// template 400 and one 100-byte record per ifIndex in idxs.
func ifSprayPacket(seq uint32, idxs []uint32) []byte {
	var payload []byte
	for _, i := range idxs {
		payload = binary.BigEndian.AppendUint32(payload, i)
		payload = binary.BigEndian.AppendUint32(payload, i)
		payload = binary.BigEndian.AppendUint32(payload, 100)
	}
	return pktgen.IPFIXTemplateThenData(uint32(tLab.Unix()), seq, 0, 400,
		[]pktgen.Field{{ID: 10, Len: 4}, {ID: 14, Len: 4}, {ID: 1, Len: 4}}, payload)
}

func span(from, n uint32) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = from + uint32(i)
	}
	return out
}

func TestIfIndexSprayIsCapped(t *testing.T) {
	db := testDB(t)
	addr := netip.MustParseAddr("192.0.2.3")
	id := seedExporter(t, db, queries.FlowExporter{Name: "spray", Address: addr.String(), Kind: "other"})
	c := newTestCollector(t, db)
	recv := tLab.Add(lag)
	c.now = func() time.Time { return recv }
	c.reconcile(recv)

	feed(c, addr, ifSprayPacket(1, span(1, 150)), recv)
	feed(c, addr, ifSprayPacket(2, span(151, 150)), recv)
	c.flushDue(recv.Add(5 * time.Minute))
	c.reconcile(recv.Add(5 * time.Minute))
	// A later minute with only new indexes: all over the cap.
	feed(c, addr, ifSprayPacket(3, span(1000, 100)), recv.Add(5*time.Minute))
	c.flushDue(recv.Add(10 * time.Minute))

	if n := countTable(t, db, "flow_exporter_ifaces"); n != maxIfacesPerExporter {
		t.Errorf("interface rows = %d, want %d", n, maxIfacesPerExporter)
	}
	if n := countTable(t, db, "flow_points"); n != maxIfacesPerExporter {
		t.Errorf("native points = %d, want %d", n, maxIfacesPerExporter)
	}
	if st := statusOf(t, c, id); st.Counters["iface_overflow"] != 300+100-maxIfacesPerExporter {
		t.Errorf("iface_overflow = %d", st.Counters["iface_overflow"])
	}
	var total int64
	if err := db.QueryRow(`SELECT SUM(bytes) FROM flow_1m WHERE exporter_id = ?`, id).Scan(&total); err != nil ||
		total != 400*100 {
		t.Errorf("stored bytes = %d, %v (every record must still be stored)", total, err)
	}
}

// ipfixChurn is an IPFIX message withdrawing the templates withdraw and
// announcing add (one field each).
func ipfixChurn(seq uint32, withdraw, add []uint16) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint16(b, 10)
	binary.BigEndian.PutUint32(b[4:], uint32(tLab.Unix()))
	binary.BigEndian.PutUint32(b[8:], seq)
	set := []byte{0, 2, 0, 0}
	for _, id := range withdraw {
		set = binary.BigEndian.AppendUint16(set, id)
		set = append(set, 0, 0)
	}
	for _, id := range add {
		set = binary.BigEndian.AppendUint16(set, id)
		set = append(set, 0, 1, 0, 8, 0, 4) // sourceIPv4Address
	}
	binary.BigEndian.PutUint16(set[2:], uint16(len(set)))
	b = append(b, set...)
	binary.BigEndian.PutUint16(b[2:], uint16(len(b)))
	return b
}

func TestTemplateChurnIsBounded(t *testing.T) {
	db := testDB(t)
	addr := netip.MustParseAddr("192.0.2.4")
	id := seedExporter(t, db, queries.FlowExporter{Name: "churn", Address: addr.String(), Kind: "other"})
	c := newTestCollector(t, db)
	recv := tLab.Add(lag)
	c.reconcile(recv)

	next := uint16(256)
	var prev []uint16
	for round := 0; round < 100; round++ {
		add := make([]uint16, 60)
		for i := range add {
			add[i] = next
			next++
		}
		feed(c, addr, ipfixChurn(uint32(round+1), prev, add), recv.Add(time.Duration(round)*100*time.Millisecond))
		prev = add
	}
	st := c.exps[id]
	if n := countTable(t, db, "flow_templates"); n != 60 {
		t.Errorf("flow_templates = %d rows, want the 60 current ones", n)
	}
	if len(st.tmplSaved) > decode.MaxTemplates {
		t.Errorf("tmplSaved = %d entries", len(st.tmplSaved))
	}
	rows, err := queries.LoadFlowTemplates(db, id, recv.Add(-time.Hour), decode.MaxTemplates)
	if err != nil || len(rows) != 60 || rows[0].TemplateID < prev[0] {
		t.Fatalf("load = %d rows (first %d), %v", len(rows), rows[0].TemplateID, err)
	}

	// Template ops only take spare writer capacity; flush batches always may.
	c.syncWrites = false
	for i := 0; i < writerCap/2; i++ {
		c.writeCh <- writeOp{status: []statusDelta{{}}}
	}
	if c.submitMaint(writeOp{deleteTemplates: id}) {
		t.Error("template op queued into a half-full writer")
	}
	if !c.submit(writeOp{flush: &flushBatch{}}) {
		t.Error("flush batch refused")
	}
}

func TestRebootFlapIsThrottled(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "rb", Address: rosAddr.String(), Kind: "routeros"})
	c := newTestCollector(t, db)
	c.syncWrites = false
	recv := tLab.Add(lag)
	c.reconcile(recv)
	deletes := 0
	drain := func() {
		for {
			select {
			case op := <-c.writeCh:
				if op.deleteTemplates != 0 {
					deletes++
				}
				c.applyWrite(op)
			default:
				return
			}
		}
	}
	sysInit := uint64(tLab.Add(-72 * time.Hour).UnixMilli())
	for i := 0; i < 20; i++ {
		si := sysInit
		if i%2 == 1 {
			si += 60_000 // IE 160 flapping: a "reboot" every datagram
		}
		flows := pktgen.LabRouterOS(tLab, si, true, rosAddr.String(), "192.168.79.216", 2055)
		feed(c, rosAddr, pktgen.RouterOSIPFIXMessage(uint32(tLab.Unix()), uint32(1+i*10), 0, true, flows...),
			recv.Add(time.Duration(i)*time.Second))
		drain()
	}
	st := statusOf(t, c, id)
	if st.Counters["reboots"] != 19 || deletes != 1 {
		t.Fatalf("reboots %d, template deletes %d (want 19, 1)", st.Counters["reboots"], deletes)
	}
	// A minute later the pending delete goes through once.
	flows := pktgen.LabRouterOS(tLab, sysInit+60_000, true, rosAddr.String(), "192.168.79.216", 2055)
	feed(c, rosAddr, pktgen.RouterOSIPFIXMessage(uint32(tLab.Unix()), 500, 0, true, flows...), recv.Add(2*time.Minute))
	drain()
	if deletes != 2 {
		t.Fatalf("template deletes after a minute = %d, want 2", deletes)
	}
}

func TestOversizedDatagramRejected(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "rb", Address: rosAddr.String(), Kind: "routeros"})
	c := newTestCollector(t, db)
	c.reconcile(tLab)
	pkt := make([]byte, maxDatagram+1)
	copy(pkt, pktgen.RouterOSIPFIXTemplates(uint32(tLab.Unix()), 1, 0, true))
	c.handlePacket(pkt, rosAddr, tLab)
	if len(c.queue) != 0 || c.global.rejected.Load() != 1 {
		t.Fatalf("queued %d, rejected %d", len(c.queue), c.global.rejected.Load())
	}
	if st := statusOf(t, c, id); st.Counters["oversized"] != 1 {
		t.Fatalf("oversized = %d", st.Counters["oversized"])
	}
}

// Two ng_netflow nodes export the same routed flow; node B's datagram is
// sent in the same uptime second after the wall second ticked (unix+1), and
// a third copy arrives with a receive skew over 5 s: all but one dropped.
func TestDupFilterIgnoresHeaderPhaseAndSkew(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "firewall003", Address: opnAddr.String(), Kind: "opnsense"})
	c := newTestCollector(t, db)
	recv := tLab.Add(lag)
	c.now = func() time.Time { return recv }
	c.reconcile(recv)

	up := uint32(500_000_000)
	unix := uint32(tLab.Unix())
	f := pktgen.V9Flow{Src: "192.168.78.60", Dst: "192.168.28.25", NextHop: "0.0.0.0", InIf: 1, OutIf: 2,
		Packets: 100, Bytes: 100_000, First: up - 40_000, Last: up - 1_000, SrcPort: 50000, DstPort: 22, Proto: 6}
	feed(c, opnAddr, pktgen.OPNsenseV9TemplatesAndData(unix, up, 1, f), recv)
	feed(c, opnAddr, pktgen.OPNsenseV9Data(unix+1, up, 1, f), recv.Add(200*time.Millisecond))
	feed(c, opnAddr, pktgen.OPNsenseV9Data(unix+1, up, 1, f), recv.Add(7*time.Second))
	c.flushDue(recv.Add(5 * time.Minute))
	var total int64
	for _, r := range aggAll(t, db, id) {
		total += r.Bytes
	}
	if st := statusOf(t, c, id); total != 100_000 || st.Counters["dup_dropped"] != 2 {
		t.Fatalf("stored %d bytes, dup_dropped %d (want 100000, 2)", total, st.Counters["dup_dropped"])
	}
}

func minuteRows(id int64, from time.Time, n int) ([]queries.FlowRow, []queries.FlowMeta) {
	var rows []queries.FlowRow
	var meta []queries.FlowMeta
	for m := 0; m < n; m++ {
		b := from.Add(time.Duration(m) * time.Minute)
		rows = append(rows, queries.FlowRow{ExporterID: id, Bucket: b, InIf: 1, OutIf: 13,
			Src: netip.MustParseAddr("1.1.1.1"), Dst: netip.MustParseAddr("192.168.28.33"), Proto: 6, Port: 443,
			Bytes: 1000, Packets: 1, Flows: 1})
		meta = append(meta, queries.FlowMeta{ExporterID: id, Bucket: b, Datagrams: 1, Records: 1, Bytes: 1000, Sampling: 1})
	}
	return rows, meta
}

func TestStartupRollsHoursLeftUnrolled(t *testing.T) {
	h := func(hour int) time.Time { return time.Date(2026, 9, 28, hour, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name  string
		fill  func(id int64) ([]queries.FlowRow, []queries.FlowMeta)
		hours []int
	}{
		// Stopped at 10:30: the shutdown flush wrote 10:00-10:29.
		{"partial hour at shutdown", func(id int64) ([]queries.FlowRow, []queries.FlowMeta) { return minuteRows(id, h(10), 30) }, []int{10}},
		// Power lost at 10:01: hour 09's rollup (due 10:02) never ran.
		{"crash before xx:02", func(id int64) ([]queries.FlowRow, []queries.FlowMeta) {
			r1, m1 := minuteRows(id, h(9), 57)
			r2, m2 := minuteRows(id, h(10), 1)
			return append(r1, r2...), append(m1, m2...)
		}, []int{9, 10}},
	} {
		db := testDB(t)
		id := seedExporter(t, db, queries.FlowExporter{Name: "rb", Address: rosAddr.String(), Kind: "routeros"})
		rows, meta := tc.fill(id)
		if err := queries.InsertFlows1m(db, rows, meta); err != nil {
			t.Fatal(err)
		}
		c := newTestCollector(t, db)
		c.submit(writeOp{startupRollup: h(14).Add(10 * time.Minute)}) // restart at 14:10
		for _, hour := range tc.hours {
			got, err := queries.AggregateFlows(db, queries.Flows1h, id, []uint32{1, 13}, h(hour), h(hour+1), 0)
			if err != nil || len(got) == 0 {
				t.Errorf("%s: hour %02d not in flow_1h (%v)", tc.name, hour, err)
			}
		}
		if left, _ := queries.UnrolledFlowHours(db, h(0), h(14)); len(left) != 0 {
			t.Errorf("%s: still unrolled: %v", tc.name, left)
		}
	}
}

func TestClockStepBackKeepsMinutesFlushable(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "rb", Address: rosAddr.String(), Kind: "routeros"})
	c := newTestCollector(t, db)
	t0 := tLab
	c.reconcile(t0)
	st := c.exps[id]
	c.nextFlush = floorMinute(t0.Unix()) - 120
	nf := normFlow{key: aggKey{inIf: 1, outIf: 13, proto: 6, port: 443}, bytes: 1000, packets: 1, sampling: 1, endpoints: true}
	step := func(now time.Time) {
		c.flushDue(now)
		st.agg.markAlive(now, c.nextFlush)
		nf.start, nf.end = now.Add(-10*time.Second), now
		st.agg.add(nf, now, c.nextFlush)
	}
	for i := 0; i < 5; i++ {
		step(t0.Add(time.Duration(i) * time.Minute))
	}
	back := t0.Add(4*time.Minute - 10*time.Minute) // NTP steps the clock back 10 min
	for i := 0; i < 10; i++ {
		step(back.Add(time.Duration(i) * time.Minute))
	}
	for i := 0; i < 20; i++ {
		c.flushDue(back.Add(time.Duration(10+i) * time.Minute))
	}
	for m := range st.agg.minutes {
		if m < c.nextFlush {
			t.Fatalf("minute %s stranded below nextFlush %s", time.Unix(m, 0).UTC().Format("15:04"),
				time.Unix(c.nextFlush, 0).UTC().Format("15:04"))
		}
	}
	var total int64
	if err := db.QueryRow(`SELECT SUM(bytes) FROM flow_1m WHERE exporter_id = ?`, id).Scan(&total); err != nil || total != 15*1000 {
		t.Fatalf("flushed bytes = %d, %v (want 15000)", total, err)
	}
}
