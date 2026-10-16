package poller

import (
	"strings"

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

func trapErr(message string) error {
	return &ros.DeviceError{Sentence: &proto.Sentence{Word: "!trap", Map: map[string]string{"message": message}}}
}

func logRow(id, time, topics, msg string) map[string]string {
	return map[string]string{".id": id, "time": time, "topics": topics, "message": msg}
}

// fakeRunner scripts device replies: queue is consumed in order by any
// command; when it is empty, byCmd answers by command (a missing command
// traps like a missing package). Every call is recorded.
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

// countCalls returns how many recorded calls start with prefix.
func (f *fakeRunner) countCalls(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}
