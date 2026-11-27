package hygiene

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

// StateSkipped marks a read the pass decided not to send (Read.When): the
// setting it would have shown does not apply to this device right now.
const StateSkipped routeros.ConfigState = "skipped"

// Section is the answer to one read.
type Section struct {
	State routeros.ConfigState `json:"state"`
	Rows  []map[string]string  `json:"rows,omitempty"`
	// Count is the row count of a count-only read.
	Count *int `json:"count,omitempty"`
	// Error says why a read failed. It is not part of the snapshot.
	Error string `json:"-"`
}

// Sections are the answers of a pass by section key. A key that is missing
// was not part of the pass.
type Sections map[string]Section

// SectionOf turns a read's reply into its section.
func SectionOf(r Read, reply routeros.ConfigReply) Section {
	s := Section{State: reply.State, Rows: reply.Rows}
	if r.CountOnly && reply.State == routeros.ConfigOK {
		n := reply.Count
		s.Count = &n
	}
	if reply.Err != nil {
		s.Error = reply.Err.Error()
	}
	return s
}

// rows returns a section's rows (nil unless it was read).
func (s Sections) rows(key string) []map[string]string {
	if sec, ok := s[key]; ok && sec.State == routeros.ConfigOK {
		return sec.Rows
	}
	return nil
}

// first returns the first row of a section — the one row of a settings
// menu — or an empty map.
func (s Sections) first(key string) map[string]string {
	if rows := s.rows(key); len(rows) > 0 {
		return rows[0]
	}
	return map[string]string{}
}

// count returns the count of a count-only section (0 unless it was read).
func (s Sections) count(key string) int {
	if sec, ok := s[key]; ok && sec.State == routeros.ConfigOK && sec.Count != nil {
		return *sec.Count
	}
	return 0
}

// known reports that a section says what the device has there: it was read,
// the menu does not exist, or the read was deliberately not sent. A section
// that is missing, denied or failed says nothing.
func (s Sections) known(keys ...string) bool {
	for _, key := range keys {
		sec, ok := s[key]
		if !ok {
			return false
		}
		switch sec.State {
		case routeros.ConfigOK, routeros.ConfigAbsent, StateSkipped:
		default:
			return false
		}
	}
	return true
}

// Denied lists the commands the NMS account was not allowed to read, sorted.
func (s Sections) Denied() []string {
	var out []string
	for key, sec := range s {
		if sec.State != routeros.ConfigDenied {
			continue
		}
		if r, ok := readOf(key); ok {
			out = append(out, strings.TrimSuffix(r.Command, "/print"))
		}
	}
	sort.Strings(out)
	return out
}

// Failed lists the sections whose read failed (a transport error, a
// timeout, an unexpected trap), sorted.
func (s Sections) Failed() []string {
	var out []string
	for key, sec := range s {
		if sec.State == routeros.ConfigFailed {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Snapshot is a device's configuration as an audit read it: what is stored
// in device_audit, and what the hash is taken over. It holds configuration
// only — no counters, no sessions, nothing a read failed on.
type Snapshot struct {
	Schema   int                `json:"schema"`
	Sections map[string]Section `json:"sections"`
}

// snapshotSchema changes when the meaning of the stored sections does; it is
// part of the hash, so such a change stores every device's snapshot anew.
const snapshotSchema = 1

// SnapshotOf builds the stored snapshot from the answers of a pass: what the
// device has configured in the menus that were read, without the runtime
// sections and without volatile fields, and the menus the NMS was refused.
// complete is false when a read failed — a partial answer is not the
// device's configuration, and is not stored as if it were.
//
// A menu with nothing in it is left out, whether the table was empty, the
// menu does not exist on the device, or the read was not sent because it
// did not apply: all three say "nothing configured there", and which of them
// it was can change from one audit to the next without anyone touching the
// device (the interface snapshot was not at hand, so a read was sent that is
// otherwise skipped). The hash must not move for that.
func SnapshotOf(s Sections) (snap Snapshot, complete bool) {
	snap = Snapshot{Schema: snapshotSchema, Sections: map[string]Section{}}
	complete = true
	for key, sec := range s {
		r, ok := readOf(key)
		if !ok || r.Runtime {
			continue
		}
		switch sec.State {
		case routeros.ConfigFailed:
			complete = false
			continue
		case StateSkipped, routeros.ConfigAbsent:
			continue
		case routeros.ConfigOK:
			if len(sec.Rows) == 0 && sec.Count == nil {
				continue
			}
		}
		out := Section{State: sec.State, Count: sec.Count}
		for _, row := range sec.Rows {
			kept := make(map[string]string, len(row))
			for k, v := range row {
				if !contains(r.Volatile, k) {
					kept[k] = v
				}
			}
			out.Rows = append(out.Rows, kept)
		}
		snap.Sections[key] = out
	}
	return snap, complete
}

// JSON is the snapshot as stored: map keys sorted, so equal snapshots are
// equal bytes.
func (s Snapshot) JSON() string {
	b, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Hash identifies the snapshot's content (SHA-256, hex).
func (s Snapshot) Hash() string {
	sum := sha256.Sum256([]byte(s.JSON()))
	return hex.EncodeToString(sum[:])
}

// Runtime is what a pass saw that is state, not configuration, and that the
// next pass compares against: the bridge's fast-path packet counters.
type Runtime struct {
	At time.Time `json:"at"`
	// CPUForwarded is bridge-fast-path-packets + bridge-fast-forward-packets:
	// packets the CPU forwarded for the bridge since boot. nil = not read.
	CPUForwarded *int64 `json:"cpu_forwarded,omitempty"`
}

// RuntimeOf picks the runtime values out of a pass's answers.
func RuntimeOf(s Sections, at time.Time) Runtime {
	rt := Runtime{At: at.UTC()}
	row := s.first(SecBridgeSettings)
	fp, okFP := parseInt(row["bridge-fast-path-packets"])
	ff, okFF := parseInt(row["bridge-fast-forward-packets"])
	if okFP || okFF {
		total := fp + ff
		rt.CPUForwarded = &total
	}
	return rt
}

// JSON is the runtime as stored with a run.
func (r Runtime) JSON() string {
	b, err := json.Marshal(r)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ParseRuntime reads a stored Runtime; ok is false for an empty or damaged
// value.
func ParseRuntime(raw string) (Runtime, bool) {
	var r Runtime
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &r) != nil || r.At.IsZero() {
		return Runtime{}, false
	}
	return r, true
}

func parseInt(v string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n, err == nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
