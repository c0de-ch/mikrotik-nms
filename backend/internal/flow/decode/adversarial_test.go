package decode

import (
	"bytes"
	"errors"
	"runtime"
	"testing"

	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
	"github.com/netsampler/goflow2/v2/decoders/netflow"
)

// allocOf returns the bytes f allocates.
func allocOf(f func()) uint64 {
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

func TestZeroLengthFieldTemplatesRefused(t *testing.T) {
	for name, last := range map[string]pktgen.Field{"fixed": {ID: 4, Len: 1}, "varlen": {ID: 1, Len: 0xffff}} {
		e := NewExporter(addrROS)
		tpl := pktgen.IPFIXTemplate(exportSecs, 1, 0, 256, pktgen.ZeroLengthFieldsTemplate(last))
		if _, err := e.Decode(tpl, tExport); !errors.Is(err, ErrDegenerateTemplate) || e.Templates.Len() != 0 {
			t.Fatalf("%s: err %v templates %d", name, err, e.Templates.Len())
		}
		var res Result
		payload := make([]byte, 1380)
		alloc := allocOf(func() { res, _ = e.Decode(pktgen.IPFIXDataSet(exportSecs, 2, 0, 256, payload), tExport) })
		if len(res.Flows) != 0 || alloc > 64<<10 {
			t.Fatalf("%s: data for a refused template: %d flows, %d B allocated", name, len(res.Flows), alloc)
		}
	}
	// The persisted form goes through the same guard.
	blob := encodeTemplateBlob(nil, []netflow.Field{{Type: 100, Length: 0}, {Type: 1, Length: 4}})
	if err := NewSafeTemplates(MaxTemplates).Import(TemplateRecord{Version: 10, ID: 256, Kind: KindData, Fields: blob}); !errors.Is(err, ErrDegenerateTemplate) {
		t.Fatalf("persisted zero-length field: %v", err)
	}
}

func TestRecordBudgetPerDatagram(t *testing.T) {
	tiny := []pktgen.Field{{ID: 1, Len: 1}} // octetDeltaCount, 1 byte: one record per data byte
	e := NewExporter(addrROS)
	mustDecode(t, e, pktgen.IPFIXTemplate(exportSecs, 1, 0, 256, tiny), tExport)
	// Within the budget: decoded.
	res := mustDecode(t, e, pktgen.IPFIXDataSet(exportSecs, 2, 0, 256, bytes.Repeat([]byte{1}, maxRecordsPerDatagram)), tExport)
	if len(res.Flows) != maxRecordsPerDatagram {
		t.Fatalf("budget-sized set: %d flows", len(res.Flows))
	}
	// Beyond it: rejected before goflow2 allocates the records.
	var err error
	big := pktgen.IPFIXDataSet(exportSecs, 3, 0, 256, bytes.Repeat([]byte{1}, 9000))
	alloc := allocOf(func() { res, err = e.Decode(big, tExport) })
	if !errors.Is(err, ErrRejected) || len(res.Flows) != 0 || alloc > 256<<10 {
		t.Fatalf("over budget: err %v flows %d alloc %d", err, len(res.Flows), alloc)
	}
	// A template announced in the same datagram counts too.
	both := pktgen.IPFIXTemplateThenData(exportSecs, 1, 0, 300, tiny, bytes.Repeat([]byte{1}, 2000))
	if _, err := NewExporter(addrROS).Decode(both, tExport); !errors.Is(err, ErrRejected) {
		t.Fatalf("template + oversized data in one datagram: %v", err)
	}
}

func TestRecordBudgetPerReplay(t *testing.T) {
	tiny := []pktgen.Field{{ID: 1, Len: 1}}
	e := NewExporter(addrROS)
	data := pktgen.IPFIXDataSet(exportSecs, 2, 0, 256, bytes.Repeat([]byte{1}, 1380))
	for i := 0; i < MaxPendingSets; i++ {
		mustDecode(t, e, data, tExport) // template unknown: buffered
	}
	if e.PendingSets() != MaxPendingSets {
		t.Fatalf("pending %d", e.PendingSets())
	}
	var res Result
	alloc := allocOf(func() { res = mustDecode(t, e, pktgen.IPFIXTemplate(exportSecs, 3, 0, 256, tiny), tExport) })
	want := (maxReplayRecords / 1380) * 1380
	if len(res.Flows) != want || e.Stats.PendingDropped != uint64(MaxPendingSets-want/1380) || e.PendingSets() != 0 {
		t.Fatalf("replay: %d flows (want %d), dropped %d, pending %d", len(res.Flows), want, e.Stats.PendingDropped, e.PendingSets())
	}
	if alloc > 16<<20 {
		t.Fatalf("replay allocated %d B", alloc)
	}
}

func TestHugeFieldCountRejectedCheaply(t *testing.T) {
	var err error
	alloc := allocOf(func() { _, err = NewExporter(addrROS).Decode(pktgen.IPFIXHugeFieldCount(), tExport) })
	if !errors.Is(err, ErrRejected) || alloc > 64<<10 {
		t.Fatalf("err %v, %d B allocated", err, alloc)
	}
}

func TestIPFIXSeqWrapIsNotReboot(t *testing.T) {
	e := NewExporter(addrROS)
	mustDecode(t, e, pktgen.RouterOSIPFIXTemplates(exportSecs, 0xFFFFFFF0, 0, true), tExport)
	ten := make([]pktgen.IPFIXFlow, 10)
	for i := range ten {
		ten[i] = rosFlows(true)[0]
	}
	mustDecode(t, e, pktgen.RouterOSIPFIXData(exportSecs, 0xFFFFFFF0, 0, ten...), tExport)
	mustDecode(t, e, pktgen.RouterOSIPFIXData(exportSecs, 0xFFFFFFFA, 0, ten...), tExport)
	res := mustDecode(t, e, pktgen.RouterOSIPFIXData(exportSecs, 4, 0, ten...), tExport)
	if res.Rebooted || len(res.Flows) != 10 || e.Templates.Len() != 2 || e.Stats.SeqLost != 0 {
		t.Fatalf("wrap: rebooted %v flows %d templates %d seq_lost %d", res.Rebooted, len(res.Flows), e.Templates.Len(), e.Stats.SeqLost)
	}
}

func TestJunkDomainsDoNotLockOut(t *testing.T) {
	e := NewExporter(addrROS)
	for obs := uint32(100); obs < 100+2*MaxDomains; obs++ {
		_, _ = e.Decode(pktgen.IPFIXDataSet(exportSecs, 1, obs, 999, nil)[:16], tExport) // header only
		_, _ = e.Decode(pktgen.IPFIXDataSet(exportSecs, 1, obs, 999, []byte{1, 2, 3, 4}), tExport)
	}
	if len(e.domains) != 0 || len(e.ipfixSeq) != 0 {
		t.Fatalf("junk admitted: domains %d ipfix seq state %d", len(e.domains), len(e.ipfixSeq))
	}
	res := mustDecode(t, e, pktgen.RouterOSIPFIXMessage(exportSecs, 1, 0, true, rosFlows(true)...), tExport)
	if len(res.Flows) != len(rosFlows(true)) {
		t.Fatalf("real domain after junk: %d flows", len(res.Flows))
	}
	// Productive domains fill the cap; a reboot releases all but the kept ones.
	for obs := uint32(1); obs < MaxDomains; obs++ {
		mustDecode(t, e, pktgen.RouterOSIPFIXTemplates(exportSecs, 1, obs, true), tExport)
	}
	if _, err := e.Decode(pktgen.RouterOSIPFIXTemplates(exportSecs, 1, 99, true), tExport); !errors.Is(err, ErrTooManyDomains) {
		t.Fatalf("17th domain: %v", err)
	}
	e.reboot(nil)
	if len(e.domains) != 0 {
		t.Fatalf("domains after reboot: %d", len(e.domains))
	}
	mustDecode(t, e, pktgen.RouterOSIPFIXTemplates(exportSecs, 1, 99, true), tExport)
}

func TestWithdrawalsReported(t *testing.T) {
	e := learnedROS(t)
	w := pktgen.IPFIXTemplate(exportSecs, 2, 0, pktgen.RouterOSTemplateV4, nil) // field count 0
	res := mustDecode(t, e, w, tExport)
	if len(res.Withdrawn) != 1 || res.Withdrawn[0] != (TemplateKey{10, 0, pktgen.RouterOSTemplateV4}) || e.Templates.Len() != 1 {
		t.Fatalf("withdrawn %v templates %d", res.Withdrawn, e.Templates.Len())
	}
	if res := mustDecode(t, e, w, tExport); len(res.Withdrawn) != 0 {
		t.Fatalf("withdrawal of an unknown template reported: %v", res.Withdrawn)
	}
	// Withdrawn, then re-announced: the re-announcement is only learned.
	e = learnedROS(t)
	_, _ = e.Decode(w, tExport)
	res = mustDecode(t, e, pktgen.RouterOSIPFIXTemplates(exportSecs, 3, 0, true), tExport)
	if len(res.Withdrawn) != 0 || len(res.Learned) != 2 {
		t.Fatalf("re-announced: withdrawn %v learned %d", res.Withdrawn, len(res.Learned))
	}
}
