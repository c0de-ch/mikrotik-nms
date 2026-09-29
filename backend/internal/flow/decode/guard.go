// Package decode is the flow collector's guarded decoding layer: goflow2
// v2.2.6 decoder packages (decoders/netflow, decoders/netflowlegacy,
// decoders/sflow — stdlib-only) behind our own guards, plus a normaliser into
// one flat Flow type. It is the only importer of goflow2, exposes no goflow2
// types and does no DB I/O; moving to goflow2 v3 (or dropping it) only
// touches this package.
//
// goflow2 v2.2.6 has two remote DoS bugs a single small datagram triggers
// (fatal OOM, not recoverable): a zero-width template makes DecodeDataSet
// loop forever, and an sFlow expanded flow sample's record count is not
// capped before make(). SafeTemplates and PrecheckSFlow neutralise both;
// FuzzDecode keeps it that way.
package decode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/netsampler/goflow2/v2/decoders/netflow"
)

// ErrRejected is matched (errors.Is) by every error that rejects a datagram
// before decoding: too short, bad version, failed prechecks. Other errors are
// decode errors of an accepted datagram.
var ErrRejected = errors.New("decode: datagram rejected")

// rejectError is a rejection with its own identity (so errors.Is works on
// both the specific error and ErrRejected).
type rejectError struct{ msg string }

func (e *rejectError) Error() string        { return e.msg }
func (e *rejectError) Is(target error) bool { return target == ErrRejected }

func rejectf(format string, args ...any) error {
	return &rejectError{msg: fmt.Sprintf(format, args...)}
}

// Guard errors.
var (
	ErrDegenerateTemplate = errors.New("decode: template has no decodable width")
	ErrTooManyTemplates   = errors.New("decode: per-exporter template cap reached")
	ErrTemplateTooWide    = errors.New("decode: template has too many fields")
	ErrTooManyDomains     = errors.New("decode: per-exporter observation domain cap reached")
	ErrMalformedV9        = errors.New("decode: malformed netflow v9 flowset")
	ErrBadV5              = &rejectError{"decode: netflow v5 count/length mismatch"}
	ErrSFlowRecordCount   = &rejectError{"decode: sflow sample record count too large"}
	ErrShort              = &rejectError{"decode: datagram too short"}
)

// Limits (normative, contract §8.13).
const (
	MaxTemplates      = 64  // templates per exporter
	maxTemplateFields = 128 // fields per template
	MaxDomains        = 16  // observation domains / v9 source ids / sFlow agents per exporter
	// Record budgets: real exporters put fewer than ~30 records in a
	// datagram. They bound what one datagram, or one replay of buffered sets,
	// can make goflow2 and the normaliser allocate (every record costs a
	// DataField per template field plus a Flow).
	maxRecordsPerDatagram = 1024
	maxReplayRecords      = 4096
)

// TemplateKey identifies a template inside one exporter.
type TemplateKey struct {
	Version   uint16
	ObsDomain uint32
	ID        uint16
}

func (k TemplateKey) u64() uint64 {
	return uint64(k.Version)<<48 | uint64(k.ObsDomain)<<16 | uint64(k.ID)
}

// SafeTemplates implements goflow2's netflow.NetFlowTemplateSystem around
// the stock BasicTemplateSystem and adds the checks goflow2 v2.2.6 lacks:
//   - rejects templates with a zero-length field or no field at all
//     (DecodeDataSet would loop forever on a zero-width one, and zero-length
//     fields amplify every data byte into a record of DataFields),
//   - treats an IPFIX/v9 template record with fieldCount 0 as a withdrawal
//     (RFC 7011 §8.1) instead of storing an empty template,
//   - caps the fields per template and the templates per exporter (goflow2
//     never expires them),
//   - records which templates were (re)learned from the wire so buffered
//     data sets can be replayed and the template persisted, and which were
//     only loaded from the database.
//
// One SafeTemplates per exporter: goflow2's key is only (version,
// obsDomainId, templateId) — it has no notion of the sender.
type SafeTemplates struct {
	inner     netflow.NetFlowTemplateSystem
	mu        sync.Mutex
	keys      map[uint64]bool // value: loaded from the database and not received since
	max       int
	added     []TemplateKey // learned from the wire; drained by the owner after each packet
	withdrawn []TemplateKey // stored templates the exporter withdrew; drained likewise
}

// NewSafeTemplates returns a guarded template store holding at most max templates.
func NewSafeTemplates(max int) *SafeTemplates {
	return &SafeTemplates{inner: netflow.CreateTemplateSystem(), keys: map[uint64]bool{}, max: max}
}

func templateFields(t interface{}) ([]netflow.Field, bool, error) {
	switch t := t.(type) {
	case netflow.TemplateRecord:
		return t.Fields, t.FieldCount == 0, nil
	case netflow.IPFIXOptionsTemplateRecord:
		f := append(append([]netflow.Field{}, t.Scopes...), t.Options...)
		return f, t.FieldCount == 0, nil
	case netflow.NFv9OptionsTemplateRecord:
		f := append(append([]netflow.Field{}, t.Scopes...), t.Options...)
		return f, len(f) == 0, nil
	}
	return nil, false, fmt.Errorf("decode: unknown template type %T", t)
}

// progresses reports whether decoding one record with these fields always
// consumes at least one byte per field (so DecodeDataSet terminates and a
// record never costs more DataFields than input bytes). A field of length 0
// is refused outright: goflow2 allocates a DataField for it on every record,
// so 127 of them next to a 1-byte field turn each input byte into a
// 128-field record (~6 KB). A variable-length field reads at least its
// 1-byte length prefix.
func progresses(fields []netflow.Field) bool {
	for _, f := range fields {
		if f.Length == 0 {
			return false
		}
	}
	return len(fields) > 0
}

// minRecordLen is the fewest bytes one record of fields consumes: the fixed
// lengths plus 1 per variable-length field (its length prefix). 0 for a
// template progresses rejects.
func minRecordLen(fields []netflow.Field) int {
	n := 0
	for _, f := range fields {
		switch f.Length {
		case 0:
			return 0
		case 0xffff:
			n++
		default:
			n += int(f.Length)
		}
	}
	return n
}

// AddTemplate implements netflow.NetFlowTemplateSystem (templates from the wire).
func (s *SafeTemplates) AddTemplate(version uint16, obs uint32, id uint16, t interface{}) error {
	return s.add(version, obs, id, t, false)
}

func (s *SafeTemplates) add(version uint16, obs uint32, id uint16, t interface{}, persisted bool) error {
	fields, withdrawal, err := templateFields(t)
	if err != nil {
		return err
	}
	k := TemplateKey{version, obs, id}
	s.mu.Lock()
	defer s.mu.Unlock()
	if withdrawal {
		if _, ok := s.keys[k.u64()]; ok {
			delete(s.keys, k.u64())
			_, _ = s.inner.RemoveTemplate(version, obs, id)
			if len(s.withdrawn) < s.max {
				s.withdrawn = append(s.withdrawn, k)
			}
		}
		return nil
	}
	if len(fields) > maxTemplateFields {
		return ErrTemplateTooWide
	}
	if !progresses(fields) {
		return ErrDegenerateTemplate
	}
	if _, ok := s.keys[k.u64()]; !ok && len(s.keys) >= s.max {
		return ErrTooManyTemplates
	}
	s.keys[k.u64()] = persisted
	if !persisted {
		s.added = append(s.added, k)
		s.withdrawn = slices.DeleteFunc(s.withdrawn, func(w TemplateKey) bool { return w == k })
	}
	return s.inner.AddTemplate(version, obs, id, t)
}

// Has reports whether a template is stored under k.
func (s *SafeTemplates) Has(k TemplateKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.keys[k.u64()]
	return ok
}

// minRecord returns the minimum record length of a stored template (see
// minRecordLen); ok is false when none is stored under the key.
func (s *SafeTemplates) minRecord(version uint16, obs uint32, id uint16) (int, bool) {
	t, err := s.inner.GetTemplate(version, obs, id)
	if err != nil {
		return 0, false
	}
	fields, _, err := templateFields(t)
	if err != nil {
		return 0, false
	}
	return minRecordLen(fields), true
}

// GetTemplate implements netflow.NetFlowTemplateSystem.
func (s *SafeTemplates) GetTemplate(version uint16, obs uint32, id uint16) (interface{}, error) {
	return s.inner.GetTemplate(version, obs, id)
}

// RemoveTemplate implements netflow.NetFlowTemplateSystem.
func (s *SafeTemplates) RemoveTemplate(version uint16, obs uint32, id uint16) (interface{}, error) {
	s.mu.Lock()
	delete(s.keys, TemplateKey{version, obs, id}.u64())
	s.mu.Unlock()
	return s.inner.RemoveTemplate(version, obs, id)
}

// GetTemplates implements netflow.NetFlowTemplateSystem.
func (s *SafeTemplates) GetTemplates() netflow.FlowBaseTemplateSet { return s.inner.GetTemplates() }

// Reset drops every template (exporter reboot detected).
func (s *SafeTemplates) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner = netflow.CreateTemplateSystem()
	s.keys = map[uint64]bool{}
	s.added, s.withdrawn = nil, nil
}

// Len returns the number of stored templates.
func (s *SafeTemplates) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// PersistedOnly reports whether at least one template is stored and every
// stored template was loaded from the database without being received from
// the exporter since.
func (s *SafeTemplates) PersistedOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keys) == 0 {
		return false
	}
	for _, persisted := range s.keys {
		if !persisted {
			return false
		}
	}
	return true
}

func (s *SafeTemplates) drainAdded() []TemplateKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.added
	s.added = nil
	return a
}

func (s *SafeTemplates) drainWithdrawn() []TemplateKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.withdrawn
	s.withdrawn = nil
	return w
}

// PrecheckV5 validates the NetFlow v5 header count against the datagram
// length before goflow2 allocates count*52 bytes (up to 3.4 MB per 24-byte
// datagram) and pads missing records with zero values.
func PrecheckV5(b []byte) error {
	if len(b) < 24 {
		return ErrShort
	}
	n := int(binary.BigEndian.Uint16(b[2:4]))
	if n == 0 || n > 30 || len(b) < 24+48*n {
		return ErrBadV5
	}
	return nil
}

// PrecheckSFlow walks an sFlow v5 datagram without allocating and rejects
// expanded flow samples (format 3) whose record count exceeds 1000:
// goflow2 v2.2.6 caps formats 1/2/4/5 but not 3 and pre-allocates
// make([]FlowRecord, count) — 0xFFFFFFFF records is ~100 GB.
func PrecheckSFlow(b []byte) error {
	off := 0
	u32 := func() (uint32, bool) {
		if len(b)-off < 4 {
			return 0, false
		}
		v := binary.BigEndian.Uint32(b[off:])
		off += 4
		return v, true
	}
	if v, ok := u32(); !ok || v != 5 {
		return rejectf("decode: not sflow v5")
	}
	ipv, ok := u32()
	if !ok {
		return ErrShort
	}
	switch ipv {
	case 1:
		off += 4
	case 2:
		off += 16
	default:
		return rejectf("decode: sflow agent ip version %d", ipv)
	}
	off += 12 // sub-agent, sequence, uptime
	n, ok := u32()
	if !ok {
		return ErrShort
	}
	if n > 1000 {
		return rejectf("decode: sflow %d samples", n)
	}
	for i := uint32(0); i < n && len(b)-off >= 8; i++ {
		format, _ := u32()
		length, _ := u32()
		if uint64(length) > uint64(len(b)-off) {
			return nil // goflow2 stops here too
		}
		body := b[off : off+int(length)]
		off += int(length)
		if format == 3 && len(body) >= 44 && binary.BigEndian.Uint32(body[40:44]) > 1000 {
			return ErrSFlowRecordCount
		}
	}
	return nil
}

// precheckTemplated walks the sets of a NetFlow v9 (already normalised) or
// IPFIX datagram without allocating per field and rejects it before goflow2
// sees it when
//   - a template record announces more than maxTemplateFields fields (goflow2
//     allocates make([]Field, count) before reading them: a 25-byte datagram
//     could cost ~725 KB), or
//   - its data sets could hold more than maxRecordsPerDatagram records: a
//     data set holds at most len/minRecordLen records of its template, taken
//     from a template announced earlier in the same datagram, else from the
//     stored one (sets of unknown templates are only buffered).
func (e *Exporter) precheckTemplated(pkt []byte, version uint16, obs uint32) error {
	hdr, tmplSet, optSet := 16, uint16(2), uint16(3)
	if version == 9 {
		hdr, tmplSet, optSet = 20, 0, 1
	}
	local := map[uint16]int{} // template id -> min record length, announced in this datagram
	records := 0
	for off := hdr; len(pkt)-off >= 4; {
		id := binary.BigEndian.Uint16(pkt[off:])
		l := int(binary.BigEndian.Uint16(pkt[off+2:]))
		if l < 4 {
			return nil // goflow2 stops here with an error
		}
		end := min(off+l, len(pkt))
		body := pkt[off+4 : end]
		off = end
		switch {
		case id == tmplSet || id == optSet:
			if err := walkTemplateSet(body, version, id == optSet, local); err != nil {
				return err
			}
		case id >= 256:
			n, ok := local[id]
			if !ok {
				n, ok = e.Templates.minRecord(version, obs, id)
			}
			if ok && n > 0 {
				if records += len(body) / n; records > maxRecordsPerDatagram {
					return rejectf("decode: datagram holds up to %d records (> %d)", records, maxRecordsPerDatagram)
				}
			}
		}
	}
	return nil
}

// walkTemplateSet reads the template records of one (options) template set
// the way goflow2 v2.2.6 does, recording each template's minimum record
// length in local (withdrawn and degenerate ones are removed). A truncated
// record ends the walk (goflow2 errors out there).
func walkTemplateSet(body []byte, version uint16, options bool, local map[uint16]int) error {
	for len(body) >= 4 {
		id := binary.BigEndian.Uint16(body)
		count, scopes, hdr := int(binary.BigEndian.Uint16(body[2:])), 0, 4
		if options {
			if len(body) < 6 {
				return nil
			}
			hdr = 6
			if version == 9 { // scope / option lengths in bytes, 4 per field
				scopes = count / 4
				count = scopes + int(binary.BigEndian.Uint16(body[4:]))/4
			} else {
				scopes = int(binary.BigEndian.Uint16(body[4:]))
			}
		}
		if count > maxTemplateFields || scopes > maxTemplateFields {
			return rejectf("decode: template %d announces %d fields (> %d)", id, max(count, scopes), maxTemplateFields)
		}
		body = body[hdr:]
		minRec, degenerate := 0, count == 0
		for i := 0; i < count; i++ {
			if len(body) < 4 {
				return nil
			}
			typ, flen := binary.BigEndian.Uint16(body), binary.BigEndian.Uint16(body[2:])
			body = body[4:]
			if version == 10 && typ&0x8000 != 0 {
				if len(body) < 4 {
					return nil
				}
				body = body[4:]
			}
			switch flen {
			case 0:
				degenerate = true
			case 0xffff:
				minRec++
			default:
				minRec += int(flen)
			}
		}
		if degenerate {
			delete(local, id)
		} else {
			local[id] = minRec
		}
	}
	return nil
}

// normalizeV9 returns a copy of a NetFlow v9 datagram that goflow2 v2.2.6
// decodes without spurious errors: goflow2 treats the header count as a
// flowset count (exporters put the record count there) and reports the
// trailing padding after the last flowset as an EOF error (upstream #499).
// The copy is trimmed to the last complete flowset and its count set to the
// number of complete flowsets. Fewer than 4 trailing bytes are padding; a
// flowset whose length is < 4 or runs past the datagram is malformed: the
// flowsets before it are kept and ErrMalformedV9 is returned with them.
func normalizeV9(pkt []byte) ([]byte, error) {
	const hdr = 20
	off, n := hdr, 0
	var err error
	for len(pkt)-off >= 4 {
		l := int(binary.BigEndian.Uint16(pkt[off+2 : off+4]))
		if l < 4 || l > len(pkt)-off {
			err = ErrMalformedV9
			break
		}
		off += l
		n++
	}
	out := make([]byte, off)
	copy(out, pkt[:off])
	binary.BigEndian.PutUint16(out[2:4], uint16(min(n, 0xffff)))
	return out, err
}
