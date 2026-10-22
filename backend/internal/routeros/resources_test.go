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
