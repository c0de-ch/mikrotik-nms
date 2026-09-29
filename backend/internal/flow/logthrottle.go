package flow

import (
	"fmt"
	"time"
)

// wireLogEvery is how often a log line triggered by wire data (reboots,
// ifIndex overflow, new observation points, auto-accept failures) may repeat
// per exporter: a sender at the rate limit must not flood the journal.
const wireLogEvery = 10 * time.Minute

// logThrottle lets one log line through per interval and counts the lines
// it held back. The zero value is ready to use; not safe for concurrent use.
type logThrottle struct {
	last       time.Time
	suppressed int
}

// allow reports whether a line may be logged at now. When it may, held is
// the number of lines suppressed since the previous one (and is reset).
func (t *logThrottle) allow(now time.Time, every time.Duration) (ok bool, held int) {
	if !t.last.IsZero() && now.Sub(t.last) < every {
		t.suppressed++
		return false, 0
	}
	held, t.last, t.suppressed = t.suppressed, now, 0
	return true, held
}

// heldNote renders a suppressed count as a log-line suffix ("" for 0).
func heldNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d similar line(s) suppressed)", n)
}
