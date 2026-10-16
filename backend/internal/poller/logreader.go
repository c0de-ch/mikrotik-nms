package poller

import (
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

// Consumers of the shared log read. Each gets every line exactly once.
const (
	logConsumerWifi   = "wifi"   // WifiTracker: join / leave / roam lines
	logConsumerBridge = "bridge" // NetworkHealthPoller: loop / flap lines and the login watch
)

const (
	// logReadMinInterval is the shortest gap between two reads of one device's
	// log buffer, whoever asks. The wifi tracker (30 s default) and the
	// network-health poller (60 s) used to each dump the buffer on their own
	// cycle — two full /log/print per device per minute, plus the device's
	// "logging" CPU burst for each. Now the first poller to ask within a
	// window reads, the others are served from the buffer.
	logReadMinInterval = 30 * time.Second

	// logBufferMaxLines caps the lines kept for a consumer that has stopped
	// asking (a poller that no longer visits the device); the oldest are
	// dropped for everyone.
	logBufferMaxLines = 4000
)

// logLine is one buffered entry with the reader's own sequence number, which
// is what consumers' positions refer to (device ids can restart on reboot).
type logLine struct {
	seq uint64
	e   routeros.LogEntry
}

type logDeviceState struct {
	mu       sync.Mutex
	cursor   routeros.LogCursor
	lines    []logLine // seq ascending; see trim for what is kept
	nextSeq  uint64
	readSeq  uint64 // seq of the first line of the latest read: kept until the next read
	resetSeq uint64 // seq of the first line of the latest reset that brought lines (see LogCursor)
	readAt   time.Time
	lastErr  error
	marks    map[string]uint64 // consumer → seq of its next unseen line
}

// LogReader hands every poller that wants a device's log lines the same
// read. One cursored, proplisted /log/print per device per window
// (logReadMinInterval); each consumer gets the lines it has not seen yet,
// whatever its own cadence. Safe for concurrent use; one device read at a
// time per device, other devices are not blocked.
//
// The read cadence is still driven by whoever asks first; the per-class
// cadences (P0-4) will set it per device.
type LogReader struct {
	mu          sync.Mutex
	devs        map[string]*logDeviceState
	minInterval time.Duration
}

// NewLogReader returns an empty reader.
func NewLogReader() *LogReader {
	return &LogReader{devs: make(map[string]*logDeviceState), minInterval: logReadMinInterval}
}

func (r *LogReader) state(deviceID string) *logDeviceState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.devs[deviceID]
	if st == nil {
		st = &logDeviceState{nextSeq: 1, marks: make(map[string]uint64)}
		r.devs[deviceID] = st
	}
	return st
}

// Entries returns the log lines of deviceID that consumer has not seen yet,
// oldest first, reading the device when the buffered copy is older than the
// minimum interval. A read error is returned only when it left the consumer
// with nothing new; buffered lines are served regardless.
//
// reset is true on the consumer's first call for the device and after the
// device's buffer restarted: the lines may have been processed before, so
// callers keep their fingerprint dedupe (and first-run suppression) for them.
func (r *LogReader) Entries(consumer, deviceID string, run routeros.Runner, now time.Time) (entries []routeros.LogEntry, reset bool, err error) {
	st := r.state(deviceID)
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.readAt.IsZero() || now.Sub(st.readAt) >= r.minInterval {
		fresh, cursorReset, rerr := st.cursor.ReadNew(run)
		st.readAt = now
		st.lastErr = rerr
		if rerr == nil {
			st.readSeq = st.nextSeq
			if cursorReset && len(fresh) > 0 {
				st.resetSeq = st.nextSeq
			}
			for i := range fresh {
				st.lines = append(st.lines, logLine{seq: st.nextSeq, e: fresh[i]})
				st.nextSeq++
			}
			if len(st.lines) > logBufferMaxLines {
				st.lines = st.lines[len(st.lines)-logBufferMaxLines:]
			}
		}
	}

	mark, known := st.marks[consumer]
	if !known {
		reset = true
		mark = 0
		if len(st.lines) > 0 {
			mark = st.lines[0].seq
		}
	}
	if st.resetSeq > 0 && mark <= st.resetSeq {
		reset = true
	}
	for i := range st.lines {
		if st.lines[i].seq >= mark {
			entries = append(entries, st.lines[i].e)
		}
	}
	st.marks[consumer] = st.nextSeq
	st.trim()

	if len(entries) == 0 && st.lastErr != nil {
		return nil, reset, st.lastErr
	}
	return entries, reset, nil
}

// trim drops the lines of earlier reads that every known consumer has seen.
// The latest read stays buffered until the next one, so a consumer whose
// first call for the device lands inside the window gets the same lines the
// first asker did.
func (st *logDeviceState) trim() {
	low := st.readSeq
	for _, m := range st.marks {
		if m < low {
			low = m
		}
	}
	i := 0
	for i < len(st.lines) && st.lines[i].seq < low {
		i++
	}
	if i > 0 {
		st.lines = append(st.lines[:0], st.lines[i:]...)
	}
}

// Forget drops a device's state (deleted device).
func (r *LogReader) Forget(deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.devs, deviceID)
}

// Prune drops the state of every device not in active.
func (r *LogReader) Prune(active map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.devs {
		if _, ok := active[id]; !ok {
			delete(r.devs, id)
		}
	}
}
