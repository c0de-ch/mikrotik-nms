package poller

import (
	"errors"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

func entryIDs(entries []routeros.LogEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// One read per window serves every consumer, each line exactly once — and
// the buffer the first read finds is the device's history: it primes the
// cursor and is handed to nobody.
func TestLogReaderSharesOneReadPerWindow(t *testing.T) {
	a := logRow("*1", "10:00:00", "wireless,info", "a")
	b := logRow("*2", "10:00:01", "bridge,info", "b")
	c := logRow("*3", "10:00:30", "system,info,account", "c")
	d := logRow("*4", "10:01:10", "bridge,info", "d")
	f := &fakeRunner{queue: []fakeReply{rep(a, b), rep(a, b, c), rep(a, b, c, d)}}
	r := NewLogReader()
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	got, err := r.Entries(logConsumerWifi, "d1", f, t0, 0)
	if err != nil || len(got) != 0 || f.countCalls("/log/print") != 1 {
		t.Fatalf("first read: %v err=%v reads=%d, want the buffer kept back", entryIDs(got), err, f.countCalls("/log/print"))
	}
	// A second consumer 5 s later is served from the same read: nothing.
	got, err = r.Entries(logConsumerBridge, "d1", f, t0.Add(5*time.Second), 0)
	if err != nil || len(got) != 0 || f.countCalls("/log/print") != 1 {
		t.Fatalf("bridge first: %v err=%v reads=%d", entryIDs(got), err, f.countCalls("/log/print"))
	}
	// At the window the next asker reads; the cursor yields only the new line.
	got, _ = r.Entries(logConsumerWifi, "d1", f, t0.Add(30*time.Second), 0)
	if !sameIDs(entryIDs(got), []string{"*3"}) || f.countCalls("/log/print") != 2 {
		t.Fatalf("wifi second window: %v reads=%d", entryIDs(got), f.countCalls("/log/print"))
	}
	// The other consumer gets that line too, from the buffer.
	got, _ = r.Entries(logConsumerBridge, "d1", f, t0.Add(40*time.Second), 0)
	if !sameIDs(entryIDs(got), []string{"*3"}) || f.countCalls("/log/print") != 2 {
		t.Fatalf("bridge second window: %v reads=%d", entryIDs(got), f.countCalls("/log/print"))
	}
	// A consumer that skips a window still gets every line once, in order.
	got, _ = r.Entries(logConsumerWifi, "d1", f, t0.Add(70*time.Second), 0)
	if !sameIDs(entryIDs(got), []string{"*4"}) {
		t.Fatalf("wifi third window: %v", entryIDs(got))
	}
	if st := r.state("d1"); len(st.lines) != 1 || st.lines[0].e.ID != "*4" {
		t.Fatalf("the bridge consumer has not seen *4 yet; buffer = %+v", st.lines)
	}
	got, _ = r.Entries(logConsumerBridge, "d1", f, t0.Add(75*time.Second), 0)
	if !sameIDs(entryIDs(got), []string{"*4"}) {
		t.Fatalf("bridge third window: %v", entryIDs(got))
	}
}

// A consumer that first asks after the baseline read gets what was logged
// since — it is news to it, not the device's history.
func TestLogReaderLateConsumerGetsLinesLoggedSinceThePrime(t *testing.T) {
	a := logRow("*1", "10:00:00", "system,info", "a")
	b := logRow("*2", "10:00:40", "system,info,account", "user other logged in from 192.0.2.50 via api")
	f := &fakeRunner{queue: []fakeReply{rep(a), rep(a, b)}}
	r := NewLogReader()
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	r.Entries(logConsumerWifi, "d1", f, t0, 0)
	got, _ := r.Entries(logConsumerBridge, "d1", f, t0.Add(time.Minute), 0)
	if !sameIDs(entryIDs(got), []string{"*2"}) {
		t.Fatalf("late consumer: %v, want the line logged after the prime", entryIDs(got))
	}
}

// The lines a device logs after a reboot are news for every consumer; a
// failed read neither primes nor moves anything.
func TestLogReaderDeviceRestartAndErrors(t *testing.T) {
	a := logRow("*1", "10:00:00", "wireless,info", "a")
	b := logRow("*2", "10:00:01", "bridge,info", "b")
	boot := logRow("*1", "11:00:00", "system,info", "router rebooted")
	f := &fakeRunner{queue: []fakeReply{
		{err: errors.New("read: connection reset")}, // never primed
		rep(a, b), // the baseline
		rep(boot), // the device rebooted
		{err: errors.New("read: connection reset")},
	}}
	r := NewLogReader()
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	if _, err := r.Entries(logConsumerWifi, "d1", f, t0, 0); err == nil {
		t.Fatal("expected the read error")
	}
	if got, err := r.Entries(logConsumerWifi, "d1", f, t0.Add(time.Minute), 0); err != nil || len(got) != 0 {
		t.Fatalf("baseline after a failed read: %v err=%v, want it kept back", entryIDs(got), err)
	}
	r.Entries(logConsumerBridge, "d1", f, t0.Add(time.Minute), 0)

	got, err := r.Entries(logConsumerWifi, "d1", f, t0.Add(2*time.Minute), 0)
	if err != nil || !sameIDs(entryIDs(got), []string{"*1"}) {
		t.Fatalf("wifi after reboot: %v err=%v", entryIDs(got), err)
	}
	got, err = r.Entries(logConsumerBridge, "d1", f, t0.Add(2*time.Minute+5*time.Second), 0)
	if err != nil || !sameIDs(entryIDs(got), []string{"*1"}) {
		t.Fatalf("bridge after reboot: %v err=%v", entryIDs(got), err)
	}

	// A failed read with nothing buffered surfaces the error to every
	// consumer of the window.
	if _, err := r.Entries(logConsumerWifi, "d1", f, t0.Add(3*time.Minute), 0); err == nil {
		t.Fatal("expected the read error")
	}
	if _, err := r.Entries(logConsumerBridge, "d1", f, t0.Add(3*time.Minute+5*time.Second), 0); err == nil {
		t.Fatal("expected the cached read error")
	}

	r.Prune(map[string]struct{}{"other": {}})
	if _, ok := r.devs["d1"]; ok {
		t.Fatal("Prune kept an inactive device")
	}
}
