package routeros

import (
	"errors"
	"reflect"
	"testing"
)

func TestReadTrafficFlow(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/traffic-flow/print": rep(map[string]string{"enabled": "true", "interfaces": "all", "active-flow-timeout": "1m"}),
		"/ip/traffic-flow/target/print": rep(
			map[string]string{"dst-address": "198.51.100.5", "port": "2055", "src-address": "192.0.2.10", "version": "ipfix", "disabled": "false"},
			map[string]string{"dst-address": "198.51.100.9", "port": "9995", "version": "9", "disabled": "true"},
		),
	}}
	st, err := ReadTrafficFlow(f)
	if err != nil {
		t.Fatal(err)
	}
	want := TrafficFlowState{Enabled: true, Interfaces: "all", Targets: []TrafficFlowTarget{
		{DstAddress: "198.51.100.5", Port: 2055, SrcAddress: "192.0.2.10", Version: "ipfix"},
		{DstAddress: "198.51.100.9", Port: 9995, Version: "9", Disabled: true},
	}}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("state = %+v\nwant    %+v", st, want)
	}
	if !st.Exporting() {
		t.Error("enabled with an enabled target must count as exporting")
	}
	// Exactly two read-only, proplisted prints.
	wantCalls := []string{
		"/ip/traffic-flow/print =.proplist=enabled,interfaces,active-flow-timeout",
		"/ip/traffic-flow/target/print =.proplist=dst-address,port,src-address,version,disabled",
	}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Fatalf("commands = %q\nwant       %q", f.calls, wantCalls)
	}
	if got := st.Summary(); got != "traffic-flow enabled (interfaces=all); targets: 198.51.100.5:2055 (ipfix), 198.51.100.9:9995 (9, disabled)" {
		t.Errorf("summary = %q", got)
	}
}

func TestTrafficFlowExporting(t *testing.T) {
	target := TrafficFlowTarget{DstAddress: "198.51.100.5", Port: 2055}
	off := TrafficFlowTarget{DstAddress: "198.51.100.5", Port: 2055, Disabled: true}
	for _, c := range []struct {
		name string
		st   TrafficFlowState
		want bool
	}{
		{"disabled, no target (the default)", TrafficFlowState{}, false},
		{"disabled with a target left behind", TrafficFlowState{Targets: []TrafficFlowTarget{target}}, false},
		{"enabled without a target sends nothing", TrafficFlowState{Enabled: true}, false},
		{"enabled, only a disabled target", TrafficFlowState{Enabled: true, Targets: []TrafficFlowTarget{off}}, false},
		{"enabled with a target", TrafficFlowState{Enabled: true, Targets: []TrafficFlowTarget{off, target}}, true},
	} {
		if got := c.st.Exporting(); got != c.want {
			t.Errorf("%s: Exporting() = %v, want %v", c.name, got, c.want)
		}
	}
	if s := (TrafficFlowState{}).Summary(); s != "traffic-flow disabled; no targets" {
		t.Errorf("summary of the default state = %q", s)
	}
}

// "yes"/"no" (the console's words) are read like the API's true/false, and
// an empty settings reply is not mistaken for "disabled".
func TestReadTrafficFlowTolerantAndStrict(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/traffic-flow/print":        rep(map[string]string{"enabled": "yes"}),
		"/ip/traffic-flow/target/print": rep(),
	}}
	st, err := ReadTrafficFlow(f)
	if err != nil || !st.Enabled || len(st.Targets) != 0 {
		t.Fatalf("yes: %+v %v", st, err)
	}
	f = &fakeRunner{byCmd: map[string]fakeReply{"/ip/traffic-flow/print": rep(), "/ip/traffic-flow/target/print": rep()}}
	if _, err := ReadTrafficFlow(f); !errors.Is(err, ErrEmptyReply) {
		t.Fatalf("empty settings reply: %v, want ErrEmptyReply", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("the target print ran after the settings print failed: %q", f.calls)
	}
	// A trap (no permission, no such menu) is an error, never "not exporting".
	f = &fakeRunner{byCmd: map[string]fakeReply{"/ip/traffic-flow/print": {err: trapErr("not enough permissions (9)")}}}
	if _, err := ReadTrafficFlow(f); !IsPermissionDenied(err) {
		t.Fatalf("trap: %v", err)
	}
	f = &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/traffic-flow/print":        rep(map[string]string{"enabled": "true"}),
		"/ip/traffic-flow/target/print": {err: errors.New("i/o timeout")},
	}}
	if _, err := ReadTrafficFlow(f); err == nil {
		t.Fatal("a failed target print must fail the read")
	}
}
