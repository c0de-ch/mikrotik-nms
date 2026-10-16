package routeros

import (
	"testing"
)

func logRow(id, time, topics, msg string) map[string]string {
	return map[string]string{".id": id, "time": time, "topics": topics, "message": msg}
}

func ids(entries []LogEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func eq(a, b []string) bool {
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

func TestLogCursorAdvanceRestartAndRotation(t *testing.T) {
	a := logRow("*1", "10:00:00", "system,info", "a")
	b := logRow("*2", "10:00:01", "bridge,info", "b")
	c := logRow("*3", "10:00:02", "wireless,info", "c")
	boot := logRow("*1", "11:00:00", "system,info", "router rebooted")
	x := logRow("*2", "11:00:05", "system,info", "x")
	y := logRow("*3", "11:00:06", "system,info", "y")
	z := logRow("*4", "11:00:07", "system,info", "z")
	w := logRow("*5", "11:00:08", "system,info", "w")
	f := &fakeRunner{queue: []fakeReply{
		rep(a, b),    // 1: first read — everything, reset
		rep(a, b, c), // 2: one new line
		rep(a, b, c), // 3: nothing new
		rep(boot),    // 4: ids went backwards — the device rebooted
		rep(boot, x), // 5: one new line after the reboot
		rep(x, y, z), // 6: *1 rotated out; the cursor line *2 is still there
		rep(y, z, w), // 7: cursor *4 present, one new
		rep(logRow("*5", "12:00:00", "system,info", "other"), logRow("*6", "12:00:01", "system,info", "v")), // 8: *5 holds a different line
	}}
	var cur LogCursor
	steps := []struct {
		want  []string
		reset bool
	}{
		{[]string{"*1", "*2"}, true},
		{[]string{"*3"}, false},
		{nil, false},
		{[]string{"*1"}, true},
		{[]string{"*2"}, false},
		{[]string{"*3", "*4"}, false},
		{[]string{"*5"}, false},
		{[]string{"*5", "*6"}, true},
	}
	for i, s := range steps {
		got, reset, err := cur.ReadNew(f)
		if err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if !eq(ids(got), s.want) || reset != s.reset {
			t.Fatalf("step %d: got %v reset=%v, want %v reset=%v", i+1, ids(got), reset, s.want, s.reset)
		}
	}
	if f.calls[0] != "/log/print =.proplist=.id,time,topics,message" {
		t.Fatalf("sent %q", f.calls[0])
	}
}

func TestLogCursorEmptyAndUnparsable(t *testing.T) {
	f := &fakeRunner{queue: []fakeReply{
		rep(), // empty buffer
		rep(logRow("*1", "10:00:00", "system,info", "a")),
		rep(logRow("", "10:00:00", "system,info", "a"), logRow("", "10:00:01", "system,info", "b")), // no ids
		rep(logRow("*1", "10:00:00", "system,info", "a"), logRow("*2", "10:00:01", "system,info", "b")),
	}}
	var cur LogCursor
	got, reset, _ := cur.ReadNew(f)
	if len(got) != 0 || !reset {
		t.Fatalf("empty first read: %v reset=%v", ids(got), reset)
	}
	got, reset, _ = cur.ReadNew(f)
	if !eq(ids(got), []string{"*1"}) || reset {
		t.Fatalf("first line: %v reset=%v", ids(got), reset)
	}
	// Unparsable ids: everything is returned and the position does not move.
	got, reset, _ = cur.ReadNew(f)
	if len(got) != 2 || reset {
		t.Fatalf("unparsable: %d lines reset=%v", len(got), reset)
	}
	got, _, _ = cur.ReadNew(f)
	if !eq(ids(got), []string{"*2"}) {
		t.Fatalf("after unparsable read the cursor must still be at *1, got %v", ids(got))
	}
}

func TestLogCursorReadErrorAndReadLog(t *testing.T) {
	f := &fakeRunner{queue: []fakeReply{{err: trapErr("not enough permissions (9)")}}}
	var cur LogCursor
	if _, _, err := cur.ReadNew(f); err == nil {
		t.Fatal("expected the trap")
	}
	f = &fakeRunner{queue: []fakeReply{rep(logRow("*1", "10:00:00", "wireless,info", "w"), logRow("*2", "10:00:01", "bridge,info", "b"))}}
	entries, err := ReadLog(f)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ReadLog: %d entries, %v", len(entries), err)
	}
	if entries[1].Fingerprint() != "10:00:01\x1fb" {
		t.Fatalf("fingerprint = %q", entries[1].Fingerprint())
	}
}

func TestHasTopicAndParsers(t *testing.T) {
	if !hasTopic("wireless,info", "wifi", "wireless") || hasTopic("", "wifi") || hasTopic("system,info", "wifi") {
		t.Fatal("hasTopic")
	}
	entries := []LogEntry{
		{ID: "*1", Time: "10:00:00", Topics: "wireless,info", Message: "00:00:5E:00:53:A0@ap1(Net) connected, signal strength -30"},
		{ID: "*2", Time: "10:00:01", Topics: "bridge,info", Message: "bridge: loop detected on ether5"},
		{ID: "*3", Time: "10:00:02", Topics: "system,info,account", Message: "user admin logged in from 192.0.2.10 via api"},
	}
	w := WirelessLogEventsFrom(entries)
	if len(w) != 1 || w[0].Event != "connected" || w[0].Fingerprint() != "10:00:00\x1f"+entries[0].Message {
		t.Fatalf("wireless: %+v", w)
	}
	b := BridgeLogEventsFrom(entries)
	if len(b) != 1 || b[0].Kind != "loop_detected" || b[0].Port != "ether5" {
		t.Fatalf("bridge: %+v", b)
	}
}
