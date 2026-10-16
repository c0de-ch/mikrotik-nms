package routeros

import (
	"strconv"
	"strings"
)

// LogEntry is one /log/print row. The .id of a RouterOS log line is a hex
// counter ("*1A3") that only grows while the device runs; the in-memory log
// buffer drops its oldest lines once full, but their ids are never reused
// until the device reboots.
type LogEntry struct {
	ID      string
	Time    string // RouterOS time field, e.g. "13:48:32" or "apr/08 13:48:32"
	Topics  string // comma-separated, e.g. "wireless,info"
	Message string
}

// Fingerprint identifies the line within one device's buffer, the same way
// the wireless and bridge parsers do (time + message), so a line keeps its
// identity across reads even when the id is unavailable.
func (e *LogEntry) Fingerprint() string {
	return e.Time + "\x1f" + e.Message
}

// logProplist is the only /log/print the NMS sends: four fields, so the
// device neither formats nor sends the rest.
const logProplist = "=.proplist=.id,time,topics,message"

// LogCursor remembers how far into a device's log buffer a reader has got, so
// each ReadNew returns only the lines logged since the previous read instead
// of the whole buffer. One cursor per device; not safe for concurrent use.
//
// The device still formats the full buffer on every read (the API has no
// portable "since id" filter), so the cursor saves parsing and dedupe work on
// our side; the number of reads is what the shared poller.LogReader cuts.
type LogCursor struct {
	primed  bool
	last    uint64 // numeric .id of the newest line returned so far
	lastKey string // that line's fingerprint: detects a restarted buffer reusing ids
}

// parseLogID turns "*1A3" into 0x1A3. ok is false for anything else.
func parseLogID(id string) (uint64, bool) {
	s := strings.TrimPrefix(strings.TrimSpace(id), "*")
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// ReadNew reads the device's log buffer and returns the lines after the
// cursor, oldest first.
//
// reset is true when every returned line may have been seen before: on the
// first read (the buffer predates us) and after a buffer restart (the device
// rebooted: ids went backwards, or the id we stopped at now holds a different
// line). Callers keep their own fingerprint dedupe for those cases; the
// cursor only promises that with reset=false nothing is returned twice.
// Lines whose id does not parse disable the cursor for that read: everything
// is returned and the position does not move.
func (c *LogCursor) ReadNew(run Runner) (entries []LogEntry, reset bool, err error) {
	reply, err := run.Run("/log/print", logProplist)
	if err != nil {
		return nil, false, err
	}
	all := make([]LogEntry, 0, len(reply.Re))
	ids := make([]uint64, 0, len(reply.Re))
	parsable := true
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		e := LogEntry{ID: m[".id"], Time: m["time"], Topics: m["topics"], Message: m["message"]}
		all = append(all, e)
		n, ok := parseLogID(e.ID)
		if !ok {
			parsable = false
		}
		ids = append(ids, n)
	}
	first := !c.primed
	c.primed = true
	if !parsable || len(all) == 0 {
		return all, first, nil
	}

	maxIdx := 0
	for i, id := range ids {
		if id > ids[maxIdx] {
			maxIdx = i
		}
	}
	maxID := ids[maxIdx]
	if first {
		c.last, c.lastKey = maxID, all[maxIdx].Fingerprint()
		return all, true, nil
	}

	restart := maxID < c.last
	for i, id := range ids {
		if id == c.last {
			restart = restart || all[i].Fingerprint() != c.lastKey
			break
		}
	}
	if restart {
		c.last, c.lastKey = maxID, all[maxIdx].Fingerprint()
		return all, true, nil
	}

	fresh := make([]LogEntry, 0, 8)
	for i, id := range ids {
		if id > c.last {
			fresh = append(fresh, all[i])
		}
	}
	if len(fresh) > 0 {
		c.last, c.lastKey = maxID, all[maxIdx].Fingerprint()
	}
	return fresh, false, nil
}

// ReadLog returns the device's whole log buffer, oldest first. For callers
// that have no cursor (one-shot diagnostics); pollers use poller.LogReader.
func ReadLog(run Runner) ([]LogEntry, error) {
	entries, _, err := (&LogCursor{}).ReadNew(run)
	return entries, err
}

// hasTopic reports whether the comma-separated topics field contains any of
// the wanted topics.
func hasTopic(topics string, wanted ...string) bool {
	if topics == "" {
		return false
	}
	for _, t := range strings.Split(topics, ",") {
		t = strings.TrimSpace(t)
		for _, w := range wanted {
			if t == w {
				return true
			}
		}
	}
	return false
}
