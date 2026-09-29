package decode

import (
	"bytes"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
)

// fuzzSeeds are every checked-in fixture plus the hostile generators.
func fuzzSeeds(t testing.TB) [][]byte {
	var out [][]byte
	for _, fx := range fixtures {
		out = append(out, loadFixture(t, fx.name))
	}
	return append(out, pktgen.IPFIXZeroFieldTemplateThenData(), pktgen.SFlowExpandedHugeCount(),
		pktgen.DegenerateTemplatePacket(), pktgen.IPFIXHugeFieldCount(),
		pktgen.IPFIXTemplateThenData(exportSecs, 1, 0, 300, pktgen.ZeroLengthFieldsTemplate(pktgen.Field{ID: 4, Len: 1}),
			bytes.Repeat([]byte{6}, 200)),
		pktgen.IPFIXTemplateThenData(exportSecs, 1, 0, 300, pktgen.ZeroLengthFieldsTemplate(pktgen.Field{ID: 1, Len: 0xffff}),
			make([]byte, 200)),
		pktgen.IPFIXTemplateThenData(exportSecs, 1, 0, 300, []pktgen.Field{{ID: 1, Len: 1}}, bytes.Repeat([]byte{1}, 1500)))
}

// ampLimit is the most FuzzAmplify lets one decode allocate per input byte
// (plus a fixed 2 MB of slack).
const ampLimit = 400

// FuzzAmplify decodes each input twice on one exporter (templates of the
// first pass are used by the second, and its buffered sets replayed) and
// fails when that allocates more than ampLimit bytes per input byte: the
// record budgets and the zero-length-field guard keep hostile templates from
// turning a datagram into hundreds of MB.
func FuzzAmplify(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s)
	}
	now := tExport
	f.Fuzz(func(t *testing.T, b []byte) {
		e := NewExporter(addrROS)
		runtime.GC()
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		in := append([]byte(nil), b...)
		_, _ = e.Decode(in, now)
		_, _ = e.Decode(in, now)
		runtime.ReadMemStats(&m1)
		if alloc, limit := m1.TotalAlloc-m0.TotalAlloc, uint64(ampLimit*2*len(b)+2<<20); alloc > limit {
			t.Fatalf("allocated %d B for a %d B datagram", alloc, len(b))
		}
	})
}

// overlaps reports whether b shares memory with in.
func overlaps(b, in []byte) bool {
	if len(b) == 0 || len(in) == 0 {
		return false
	}
	bs, be := uintptr(unsafe.Pointer(&b[0])), uintptr(unsafe.Pointer(&b[len(b)-1]))
	is, ie := uintptr(unsafe.Pointer(&in[0])), uintptr(unsafe.Pointer(&in[len(in)-1]))
	return bs <= ie && is <= be
}

// FuzzDecode feeds arbitrary datagrams through the guarded decoder, both on
// a fresh exporter and on warm ones that already learned the RouterOS and
// OPNsense templates (so mutated data sets are actually decoded).
// Invariants: no decoder panic (recovered panics count as failures), bounded
// state, Bytes == RawBytes × SamplingRate, and nothing returned or buffered
// aliases the input. Hangs and OOM kill the fuzz worker and are reported by
// the fuzzer; `go test -race ./...` runs the seeds.
func FuzzDecode(f *testing.F) {
	for _, s := range fuzzSeeds(f) {
		f.Add(s)
	}
	now := tExport.Add(tRecvLag)
	warmROS := NewExporter(addrROS)
	_, _ = warmROS.Decode(loadFixture(f, "ros_ipfix_templates"), now)
	_, _ = warmROS.Decode(loadFixture(f, "ipfix_options_sampling"), now)
	warmOPN := NewExporter(addrOPN)
	warmOPN.SkipV9Seq = true
	_, _ = warmOPN.Decode(loadFixture(f, "opn_v9_templates"), now)
	_, _ = warmOPN.Decode(loadFixture(f, "v9_options_sampling"), now)

	f.Fuzz(func(t *testing.T, b []byte) {
		for _, e := range []*Exporter{NewExporter(addrROS), warmROS, warmOPN} {
			in := append([]byte(nil), b...)
			res, _ := e.Decode(in, now)
			if e.Stats.Panics > 0 {
				t.Fatalf("decoder panic: %s", e.Stats.LastError)
			}
			if e.Templates.Len() > MaxTemplates || len(e.pending) > MaxPendingSets || e.pendSize > MaxPendingBytes ||
				len(e.domains) > MaxDomains {
				t.Fatalf("state bound exceeded: templates %d pending %d/%d domains %d",
					e.Templates.Len(), len(e.pending), e.pendSize, len(e.domains))
			}
			for _, fl := range res.Flows {
				if fl.Bytes != fl.RawBytes*uint64(fl.SamplingRate) || fl.SamplingRate == 0 {
					t.Fatalf("bytes %d != raw %d x rate %d", fl.Bytes, fl.RawBytes, fl.SamplingRate)
				}
			}
			for _, p := range e.pending {
				if overlaps(p.raw, in) {
					t.Fatal("a pending set aliases the input buffer")
				}
			}
			for _, l := range res.Learned {
				if overlaps(l.Fields, in) {
					t.Fatal("a learned template blob aliases the input buffer")
				}
			}
			snapshot := append([]Flow(nil), res.Flows...)
			for i := range in {
				in[i] ^= 0xff
			}
			if !reflect.DeepEqual(snapshot, res.Flows) {
				t.Fatal("decoded flows changed when the input buffer was reused")
			}
		}
	})
}

// FuzzTemplateBlob fuzzes the persisted-template blob decoder: arbitrary
// (version, kind, blob) never panics; an accepted blob round-trips exactly
// and decoding a data set with the imported template terminates.
func FuzzTemplateBlob(f *testing.F) {
	now := tExport
	for _, fx := range []string{"ros_ipfix_templates", "opn_v9_templates", "ipfix_options_sampling", "v9_options_sampling"} {
		res, _ := NewExporter(addrROS).Decode(loadFixture(f, fx), now)
		for _, r := range res.Learned {
			f.Add(r.Version, uint8(r.Kind), r.Fields)
		}
	}
	f.Add(uint16(10), uint8(0), []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0}) // zero width
	f.Add(uint16(10), uint8(0), []byte{0, 0})                         // no fields
	f.Add(uint16(9), uint8(1), []byte{0, 5, 0, 1, 0, 4, 0, 0, 0, 0})  // scope > n
	f.Fuzz(func(t *testing.T, version uint16, kind uint8, blob []byte) {
		r := TemplateRecord{Version: version, ObsDomain: 1, ID: 300, Kind: int(kind), Fields: blob}
		st := NewSafeTemplates(MaxTemplates)
		if err := st.Import(r); err != nil {
			if st.Len() != 0 {
				t.Fatal("rejected blob left a template behind")
			}
			return
		}
		got, ok := st.Export(r.Key())
		if !ok || got.Kind != r.Kind || !bytes.Equal(got.Fields, blob) {
			t.Fatalf("round trip: %+v -> %+v", r, got)
		}
		if !st.PersistedOnly() {
			t.Fatal("imported template not marked persisted")
		}
		// Decode a data set for the imported template: must terminate.
		e := NewExporter(addrROS)
		if n, _ := e.LoadTemplates([]TemplateRecord{r}); n != 1 {
			t.Fatal("LoadTemplates refused an importable blob")
		}
		pkt := dataSetFor(version, 300, bytes.Repeat([]byte{0xab}, 200))
		_, _ = e.Decode(pkt, now)
		if e.Stats.Panics > 0 {
			t.Fatalf("decoder panic: %s", e.Stats.LastError)
		}
	})
}

// dataSetFor wraps payload in a v9 (version 9) or IPFIX message as the data
// set of template id, observation domain 1.
func dataSetFor(version uint16, id uint16, payload []byte) []byte {
	set := append([]byte{byte(id >> 8), byte(id), 0, 0}, payload...)
	set[2], set[3] = byte(len(set)>>8), byte(len(set))
	var hdr []byte
	if version == 9 {
		hdr = []byte{0, 9, 0, 1, 0, 0, 0, 1, 0x6a, 0xb0, 0x5c, 0x80, 0, 0, 0, 1, 0, 0, 0, 1}
	} else {
		hdr = []byte{0, 10, 0, 0, 0x6a, 0xb0, 0x5c, 0x80, 0, 0, 0, 1, 0, 0, 0, 1}
		l := len(hdr) + len(set)
		hdr[2], hdr[3] = byte(l>>8), byte(l)
	}
	return append(hdr, set...)
}

// TestFuzzSeedsDirect runs every seed once outside the fuzz engine with a
// deadline, so a seed that hangs fails fast in plain `go test`.
func TestFuzzSeedsDirect(t *testing.T) {
	seeds := append(fuzzSeeds(t), crasher(t))
	done := make(chan struct{})
	go func() {
		defer close(done)
		e := NewExporter(addrROS)
		for _, s := range seeds {
			_, _ = e.Decode(s, time.Now())
			_, _ = NewExporter(addrOPN).Decode(s, time.Now())
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("seeds did not decode within 5 s")
	}
}
