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

func TestLogReaderSharesOneReadPerWindow(t *testing.T) {
	a := logRow("*1", "10:00:00", "wireless,info", "a")
	b := logRow("*2", "10:00:01", "bridge,info", "b")
	c := logRow("*3", "10:00:30", "system,info,account", "c")
	f := &fakeRunner{queue: []fakeReply{rep(a, b), rep(a, b, c)}}
	r := NewLogReader()
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	got, reset, err := r.Entries(logConsumerWifi, "d1", f, t0, 0)
	if err != nil || !reset || !sameIDs(entryIDs(got), []string{"*1", "*2"}) {
		t.Fatalf("wifi first: %v reset=%v err=%v", entryIDs(got), reset, err)
	}
	// A second consumer 5 s later is served from the buffer: same lines,
	// its own first sight, no second device read.
	got, reset, err = r.Entries(logConsumerBridge, "d1", f, t0.Add(5*time.Second), 0)
	if err != nil || !reset || !sameIDs(entryIDs(got), []string{"*1", "*2"}) || f.countCalls("/log/print") != 1 {
		t.Fatalf("bridge first: %v reset=%v err=%v reads=%d", entryIDs(got), reset, err, f.countCalls("/log/print"))
	}
	// Asking again inside the window returns nothing new and reads nothing.
	got, reset, _ = r.Entries(logConsumerWifi, "d1", f, t0.Add(10*time.Second), 0)
	if len(got) != 0 || reset || f.countCalls("/log/print") != 1 {
		t.Fatalf("wifi inside window: %v reset=%v reads=%d", entryIDs(got), reset, f.countCalls("/log/print"))
	}
	// At the window the next asker reads; the cursor yields only the new line.
	got, reset, _ = r.Entries(logConsumerWifi, "d1", f, t0.Add(30*time.Second), 0)
	if !sameIDs(entryIDs(got), []string{"*3"}) || reset || f.countCalls("/log/print") != 2 {
		t.Fatalf("wifi second window: %v reset=%v reads=%d", entryIDs(got), reset, f.countCalls("/log/print"))
	}
	// The other consumer gets that line too, from the buffer.
	got, reset, _ = r.Entries(logConsumerBridge, "d1", f, t0.Add(40*time.Second), 0)
	if !sameIDs(entryIDs(got), []string{"*3"}) || reset || f.countCalls("/log/print") != 2 {
		t.Fatalf("bridge second window: %v reset=%v reads=%d", entryIDs(got), reset, f.countCalls("/log/print"))
	}
	// The first read's lines are gone (both consumers saw them, a newer read
	// exists); the latest read stays until the next one.
	if st := r.state("d1"); len(st.lines) != 1 || st.lines[0].e.ID != "*3" {
		t.Fatalf("buffer not trimmed to the latest read: %+v", st.lines)
	}
}

func TestLogReaderDeviceRestartAndErrors(t *testing.T) {
	a := logRow("*1", "10:00:00", "wireless,info", "a")
	b := logRow("*2", "10:00:01", "bridge,info", "b")
	boot := logRow("*1", "11:00:00", "system,info", "router rebooted")
	f := &fakeRunner{queue: []fakeReply{rep(a, b), rep(boot), {err: errors.New("read: connection reset")}}}
	r := NewLogReader()
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	r.Entries(logConsumerWifi, "d1", f, t0, 0)
	r.Entries(logConsumerBridge, "d1", f, t0, 0)

	// The device rebooted: both consumers see the new buffer flagged reset.
	got, reset, err := r.Entries(logConsumerWifi, "d1", f, t0.Add(time.Minute), 0)
	if err != nil || !reset || !sameIDs(entryIDs(got), []string{"*1"}) {
		t.Fatalf("wifi after reboot: %v reset=%v err=%v", entryIDs(got), reset, err)
	}
	got, reset, err = r.Entries(logConsumerBridge, "d1", f, t0.Add(time.Minute+5*time.Second), 0)
	if err != nil || !reset || !sameIDs(entryIDs(got), []string{"*1"}) {
		t.Fatalf("bridge after reboot: %v reset=%v err=%v", entryIDs(got), reset, err)
	}

	// A failed read with nothing buffered surfaces the error to every
	// consumer of the window, and a consumer that never asked for a device
	// does not pin its buffer.
	if _, _, err := r.Entries(logConsumerWifi, "d1", f, t0.Add(2*time.Minute), 0); err == nil {
		t.Fatal("expected the read error")
	}
	if _, _, err := r.Entries(logConsumerBridge, "d1", f, t0.Add(2*time.Minute+5*time.Second), 0); err == nil {
		t.Fatal("expected the cached read error")
	}

	r.Prune(map[string]struct{}{"other": {}})
	if _, ok := r.devs["d1"]; ok {
		t.Fatal("Prune kept an inactive device")
	}
}
