package routeros

import (
	"errors"
	"strings"
	"testing"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
)

// fakeReply is one scripted answer: !re rows, the !done map and/or an error.
type fakeReply struct {
	re   []map[string]string
	done map[string]string
	err  error
}

func rep(rows ...map[string]string) fakeReply { return fakeReply{re: rows} }

// trapErr builds the error go-routeros returns for a !trap.
func trapErr(message string) error {
	return &ros.DeviceError{Sentence: &proto.Sentence{Word: "!trap", Map: map[string]string{"message": message}}}
}

// fakeRunner scripts device replies: queue is consumed in order by any
// command; when it is empty, byCmd answers by command. Every call is recorded.
type fakeRunner struct {
	queue []fakeReply
	byCmd map[string]fakeReply
	calls []string
}

func (f *fakeRunner) Run(command string, args ...string) (*ros.Reply, error) {
	f.calls = append(f.calls, strings.TrimSpace(command+" "+strings.Join(args, " ")))
	var r fakeReply
	switch {
	case len(f.queue) > 0:
		r, f.queue = f.queue[0], f.queue[1:]
	default:
		var ok bool
		if r, ok = f.byCmd[command]; !ok {
			return nil, trapErr("no such command prefix")
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	reply := &ros.Reply{Done: &proto.Sentence{Word: "!done", Map: r.done}}
	if reply.Done.Map == nil {
		reply.Done.Map = map[string]string{}
	}
	for _, m := range r.re {
		reply.Re = append(reply.Re, &proto.Sentence{Word: "!re", Map: m})
	}
	return reply, nil
}

func TestCountOnlyAndTrapClassification(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		"/interface/wifi/print":     {done: map[string]string{"ret": "3"}},
		"/interface/wireless/print": {err: trapErr("not enough permissions (9)")},
		"/ip/dhcp-server/print":     {done: map[string]string{}},
	}}
	n, err := CountOnly(f, "/interface/wifi/print")
	if err != nil || n != 3 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if f.calls[0] != "/interface/wifi/print =count-only=" {
		t.Fatalf("sent %q", f.calls[0])
	}
	if _, err := CountOnly(f, "/ip/dhcp-server/print"); err == nil {
		t.Fatal("missing ret must be an error")
	}
	_, err = CountOnly(f, "/caps-man/interface/print")
	if !IsNoSuchCommand(err) || IsPermissionDenied(err) {
		t.Fatalf("missing menu: IsNoSuchCommand=%v IsPermissionDenied=%v (%v)", IsNoSuchCommand(err), IsPermissionDenied(err), err)
	}
	_, err = CountOnly(f, "/interface/wireless/print")
	if !IsPermissionDenied(err) || IsNoSuchCommand(err) {
		t.Fatalf("permission trap: IsPermissionDenied=%v IsNoSuchCommand=%v (%v)", IsPermissionDenied(err), IsNoSuchCommand(err), err)
	}
	// A transport error is neither: it says nothing about the device.
	plain := errors.New("read tcp: connection reset by peer")
	if IsNoSuchCommand(plain) || IsPermissionDenied(plain) {
		t.Fatal("a transport error must not classify as a trap")
	}
}
