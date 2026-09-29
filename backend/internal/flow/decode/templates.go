package decode

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/netsampler/goflow2/v2/decoders/netflow"
)

// Template kinds of TemplateRecord (flow_templates.kind).
const (
	KindData    = 0
	KindOptions = 1
)

// TemplateRecord is a template in its persisted form (flow_templates):
// Fields is the blob — scope_count u16 BE, then per field type u16 (bit 15 =
// enterprise), length u16, pen u32 (all BE).
type TemplateRecord struct {
	Version   uint16
	ObsDomain uint32
	ID        uint16
	Kind      int // KindData | KindOptions
	Fields    []byte
}

// Key returns the template's key.
func (r TemplateRecord) Key() TemplateKey {
	return TemplateKey{Version: r.Version, ObsDomain: r.ObsDomain, ID: r.ID}
}

// ErrBadTemplateBlob is returned for a blob that does not decode.
var ErrBadTemplateBlob = errors.New("decode: corrupt template blob")

// encodeTemplateBlob serialises fields (scopes first) with their scope count.
func encodeTemplateBlob(scopes, options []netflow.Field) []byte {
	b := make([]byte, 2, 2+8*(len(scopes)+len(options)))
	binary.BigEndian.PutUint16(b, uint16(len(scopes)))
	for _, list := range [][]netflow.Field{scopes, options} {
		for _, f := range list {
			t := f.Type
			if f.PenProvided {
				t |= 0x8000
			}
			b = binary.BigEndian.AppendUint16(b, t)
			b = binary.BigEndian.AppendUint16(b, f.Length)
			b = binary.BigEndian.AppendUint32(b, f.Pen)
		}
	}
	return b
}

// decodeTemplateBlob parses a blob into scope and option fields. It never
// panics on arbitrary input: the length must be exactly 2 + 8n with n <=
// maxTemplateFields and scope_count <= n.
func decodeTemplateBlob(b []byte) (scopes, options []netflow.Field, err error) {
	if len(b) < 2 || (len(b)-2)%8 != 0 {
		return nil, nil, ErrBadTemplateBlob
	}
	n := (len(b) - 2) / 8
	sc := int(binary.BigEndian.Uint16(b))
	if n > maxTemplateFields || sc > n {
		return nil, nil, ErrBadTemplateBlob
	}
	fields := make([]netflow.Field, n)
	for i := range fields {
		p := b[2+8*i:]
		t := binary.BigEndian.Uint16(p)
		fields[i] = netflow.Field{
			PenProvided: t&0x8000 != 0,
			Type:        t &^ 0x8000,
			Length:      binary.BigEndian.Uint16(p[2:]),
			Pen:         binary.BigEndian.Uint32(p[4:]),
		}
	}
	return fields[:sc], fields[sc:], nil
}

// templateToRecord converts a stored goflow2 template to its persisted form.
func templateToRecord(k TemplateKey, t interface{}) (TemplateRecord, bool) {
	r := TemplateRecord{Version: k.Version, ObsDomain: k.ObsDomain, ID: k.ID}
	switch t := t.(type) {
	case netflow.TemplateRecord:
		r.Kind, r.Fields = KindData, encodeTemplateBlob(nil, t.Fields)
	case netflow.IPFIXOptionsTemplateRecord:
		r.Kind, r.Fields = KindOptions, encodeTemplateBlob(t.Scopes, t.Options)
	case netflow.NFv9OptionsTemplateRecord:
		r.Kind, r.Fields = KindOptions, encodeTemplateBlob(t.Scopes, t.Options)
	default:
		return r, false
	}
	return r, true
}

// recordToTemplate rebuilds the goflow2 template of a persisted record.
func recordToTemplate(r TemplateRecord) (interface{}, error) {
	scopes, options, err := decodeTemplateBlob(r.Fields)
	if err != nil {
		return nil, err
	}
	n := len(scopes) + len(options)
	if n == 0 {
		return nil, fmt.Errorf("%w: no fields", ErrBadTemplateBlob) // would be a withdrawal
	}
	switch {
	case r.Kind == KindData && len(scopes) == 0:
		return netflow.TemplateRecord{TemplateId: r.ID, FieldCount: uint16(n), Fields: options}, nil
	case r.Kind == KindOptions && r.Version == 10:
		return netflow.IPFIXOptionsTemplateRecord{TemplateId: r.ID, FieldCount: uint16(n),
			ScopeFieldCount: uint16(len(scopes)), Scopes: scopes, Options: options}, nil
	case r.Kind == KindOptions && r.Version == 9:
		return netflow.NFv9OptionsTemplateRecord{TemplateId: r.ID, ScopeLength: uint16(4 * len(scopes)),
			OptionLength: uint16(4 * len(options)), Scopes: scopes, Options: options}, nil
	}
	return nil, fmt.Errorf("%w: kind %d version %d scopes %d", ErrBadTemplateBlob, r.Kind, r.Version, len(scopes))
}

// Export returns the persisted form of a stored template.
func (s *SafeTemplates) Export(k TemplateKey) (TemplateRecord, bool) {
	t, err := s.inner.GetTemplate(k.Version, k.ObsDomain, k.ID)
	if err != nil {
		return TemplateRecord{}, false
	}
	return templateToRecord(k, t)
}

// Import installs a persisted template through the same guards as a received
// one and marks it as loaded from the database. A template received from the
// exporter always replaces it (it is stored under the same key).
func (s *SafeTemplates) Import(r TemplateRecord) error {
	if r.Version != 9 && r.Version != 10 {
		return fmt.Errorf("%w: version %d", ErrBadTemplateBlob, r.Version)
	}
	t, err := recordToTemplate(r)
	if err != nil {
		return err
	}
	return s.add(r.Version, r.ObsDomain, r.ID, t, true)
}
