package routeros

import (
	"errors"
	"testing"
)

// The resource print is proplisted to the fields its readers use, and the
// sentence comes back whole for the class probe.
func TestReadSystemResourceProplisted(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		"/system/resource/print": rep(map[string]string{
			"board-name": "CRS326-24G-2S+", "version": "7.20.1 (stable)", "platform": "MikroTik",
			"architecture-name": "arm", "cpu": "ARMv7", "cpu-count": "1", "cpu-frequency": "800",
			"cpu-load": "37", "total-memory": "536870912", "free-memory": "402653184", "uptime": "3d4h",
		}),
	}}
	res, err := ReadSystemResource(f)
	if err != nil {
		t.Fatal(err)
	}
	want := "/system/resource/print =.proplist=board-name,version,platform,architecture-name," +
		"cpu,cpu-count,cpu-frequency,cpu-load,total-memory,free-memory,uptime"
	if len(f.calls) != 1 || f.calls[0] != want {
		t.Fatalf("calls = %q, want one proplisted print", f.calls)
	}
	if res.Board != "CRS326-24G-2S+" || res.Version != "7.20.1 (stable)" || res.Platform != "MikroTik" ||
		res.Architecture != "arm" || res.CPULoad != 37 || res.MemoryTotal != 536870912 ||
		res.MemoryFree != 402653184 || res.Uptime != "3d4h" {
		t.Errorf("typed fields = %+v", res)
	}
	if res.Fields["cpu-count"] != "1" || res.Fields["cpu-frequency"] != "800" {
		t.Errorf("Fields must carry the class facts: %v", res.Fields)
	}

	empty := &fakeRunner{byCmd: map[string]fakeReply{"/system/resource/print": rep()}}
	if _, err := ReadSystemResource(empty); !errors.Is(err, ErrEmptyReply) {
		t.Errorf("empty reply err = %v, want ErrEmptyReply", err)
	}
}

// The minute sample asks for three fields and nothing else; the post-poll
// sample for one.
func TestReadLoadSampleAndCPULoad(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		"/system/resource/print": rep(map[string]string{"cpu-load": "42", "free-memory": "9437184", "uptime": "1w2d3h4m5s"}),
	}}
	s, err := ReadLoadSample(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != "/system/resource/print =.proplist=cpu-load,free-memory,uptime" {
		t.Fatalf("calls = %q, want the three-field print", f.calls)
	}
	if s.CPULoad != 42 || s.FreeMemory != 9437184 || !s.HasFreeMemory || s.Uptime != "1w2d3h4m5s" ||
		s.UptimeSeconds != 9*86400+3*3600+4*60+5 || !s.HasUptime {
		t.Errorf("sample = %+v", s)
	}

	cpu, err := ReadCPULoad(f)
	if err != nil || cpu != 42 {
		t.Fatalf("ReadCPULoad = %d, %v", cpu, err)
	}
	if len(f.calls) != 2 || f.calls[1] != "/system/resource/print =.proplist=cpu-load" {
		t.Fatalf("calls = %q, want the one-field print", f.calls)
	}

	// A reply without a usable cpu-load is not a sample: a missing value
	// must never be stored as 0 % CPU.
	for name, row := range map[string]map[string]string{
		"missing": {"free-memory": "1", "uptime": "5s"},
		"garbage": {"cpu-load": "n/a"},
		"range":   {"cpu-load": "250"},
	} {
		bad := &fakeRunner{byCmd: map[string]fakeReply{"/system/resource/print": rep(row)}}
		if _, err := ReadLoadSample(bad); err == nil {
			t.Errorf("%s cpu-load: ReadLoadSample must fail", name)
		}
		if _, err := ReadCPULoad(bad); err == nil {
			t.Errorf("%s cpu-load: ReadCPULoad must fail", name)
		}
	}
	empty := &fakeRunner{byCmd: map[string]fakeReply{"/system/resource/print": rep()}}
	if _, err := ReadLoadSample(empty); !errors.Is(err, ErrEmptyReply) {
		t.Errorf("empty reply err = %v, want ErrEmptyReply", err)
	}
	// Memory and uptime are optional: the sample stands without them.
	only := &fakeRunner{byCmd: map[string]fakeReply{"/system/resource/print": rep(map[string]string{"cpu-load": "0"})}}
	if s, err := ReadLoadSample(only); err != nil || s.CPULoad != 0 || s.HasFreeMemory || s.HasUptime {
		t.Errorf("cpu-only sample = %+v, %v", s, err)
	}
}

func TestParseUptime(t *testing.T) {
	for in, want := range map[string]int64{
		"5s": 5, "4m5s": 245, "3h4m5s": 11045, "2d3h4m5s": 183845, "1w2d3h4m5s": 788645,
		"1w": 604800, "10w6d23h59m59s": 10*604800 + 6*86400 + 23*3600 + 59*60 + 59, " 1d ": 86400, "0s": 0,
	} {
		if got, ok := ParseUptime(in); !ok || got != want {
			t.Errorf("ParseUptime(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "5", "5x", "h", "1d2", "-5s", "1.5h"} {
		if got, ok := ParseUptime(in); ok {
			t.Errorf("ParseUptime(%q) = %d, want not ok", in, got)
		}
	}
}
