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
	// log buffer, whoever asks, when the caller passes no window. The wifi
	// tracker and the network-health poller used to each dump the buffer on
	// their own cycle — two full /log/print per device per minute, plus the
	// device's "logging" CPU burst for each. Now the first poller to ask
	// within a window reads, the others are served from the buffer; callers
	// pass the device's own window (logReadWindow of its TierLogs cadence).
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
	mu     sync.Mutex
	cursor routeros.LogCursor
	// primed is set by the first successful read: the buffer that read
	// found is the device's history, not news (see Entries).
	primed  bool
	lines   []logLine // seq ascending; see trim for what is kept
	nextSeq uint64
	readSeq uint64 // seq of the first line of the latest read: kept until the next read
	readAt  time.Time
	lastErr error
	marks   map[string]uint64 // consumer → seq of its next unseen line
}

// LogReader hands every poller that wants a device's log lines the same
// read. One cursored, proplisted /log/print per device per window — the
// device's own, from its TierLogs cadence (logReadWindow); each consumer gets
// the lines it has not seen yet, whatever its own cadence. Safe for
// concurrent use; one device read at a time per device, other devices are
// not blocked.
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

// logReadWindow is the read window of a device whose TierLogs runs every
// interval: nine tenths of it, as a consumer on the ±10 % jittered schedule
// may ask up to a tenth early and must still get a fresh read. 0 = the
// reader's default.
func logReadWindow(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return interval - interval/10
}

// Entries returns the log lines of deviceID that consumer has not seen yet,
// oldest first, reading the device when the buffered copy is at least window
// old (window ≤ 0: the reader's minimum interval). A read error is returned
// only when it left the consumer with nothing new; buffered lines are served
// regardless.
//
// Every line returned is news: it was logged after the reader's first
// successful read of the device. That read only primes the cursor — the
// buffer it finds predates this process, and a backend restart must not
// replay it as new events — and is handed to nobody. A buffer the device
// restarted (a reboot) is all news and is handed out like any read. So a
// consumer applies whatever it gets, whenever it first asks, and needs no
// first-run suppression of its own (one keyed on its own dedupe state would
// silently drop real events once that state is pruned).
func (r *LogReader) Entries(consumer, deviceID string, run routeros.Runner, now time.Time, window time.Duration) ([]routeros.LogEntry, error) {
	if window <= 0 {
		window = r.minInterval
	}
	st := r.state(deviceID)
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.readAt.IsZero() || now.Sub(st.readAt) >= window {
		fresh, _, rerr := st.cursor.ReadNew(run)
		st.readAt = now
		st.lastErr = rerr
		if rerr == nil {
			if !st.primed {
				st.primed = true
				fresh = nil // the device's history: primes the cursor only
			}
			st.readSeq = st.nextSeq
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
		// A consumer new to the device gets what is still buffered — all
		// of it logged since the reader primed.
		mark = st.nextSeq
		if len(st.lines) > 0 {
			mark = st.lines[0].seq
		}
	}
	var entries []routeros.LogEntry
	for i := range st.lines {
		if st.lines[i].seq >= mark {
			entries = append(entries, st.lines[i].e)
		}
	}
	st.marks[consumer] = st.nextSeq
	st.trim()

	if len(entries) == 0 && st.lastErr != nil {
		return nil, st.lastErr
	}
	return entries, nil
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
