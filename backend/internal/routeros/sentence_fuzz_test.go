package routeros

import (
	"bytes"
	"errors"
	"testing"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
)

// replayConn is a connection whose peer already said everything it ever
// will: reads drain data, writes vanish.
type replayConn struct {
	data   *bytes.Reader
	closed bool
}

func (c *replayConn) Read(p []byte) (int, error)  { return c.data.Read(p) }
func (c *replayConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *replayConn) Close() error                { c.closed = true; return nil }

// wire encodes sentences as a device would send them.
func wire(sentences ...[]string) []byte {
	var buf bytes.Buffer
	w := proto.NewWriter(&buf)
	for _, sen := range sentences {
		w.BeginSentence()
		for _, word := range sen {
			w.WriteWord(word)
		}
		_ = w.EndSentence()
	}
	return buf.Bytes()
}

// FuzzSentence feeds arbitrary bytes to the vendored go-routeros reader and
// to its reply loop as if a device had sent them (`make fuzz-ros`; plain
// `go test` replays the seeds). Whatever arrives:
//
//   - nothing panics, and nothing larger than the caps is ever built — one
//     word ≤ 1 MiB, one reply ≤ 50,000 rows;
//   - a command ends in a reply with its !done, in the device's own refusal
//     (a !trap, after which the session goes on), or in a session-fatal
//     error — and after that one the connection is closed and no later
//     command reads another byte of the stream.
func FuzzSentence(f *testing.F) {
	f.Add(wire([]string{"!re", "=name=ether1", "=rx-byte=1"}, []string{"!re", "=name=ether2"}, []string{"!done"}))
	f.Add(wire([]string{"!done", "=ret=42"}))
	f.Add(wire([]string{"!trap", "=message=no such command prefix"}, []string{"!done"}, []string{"!done"}))
	f.Add(wire([]string{"!fatal", "=message=session terminated on request"}))
	f.Add(wire([]string{"!re", ".tag=r1", "=k"}, []string{"!empty"}, []string{}, []string{"!done"}))
	f.Add(wire([]string{"!re", "not an attribute"}, []string{"!done"}))
	f.Add(wire([]string{"!what"}, []string{"!done"}))
	f.Add([]byte{0xF0, 0xFF, 0xFF, 0xFF, 0xFF})                      // a word of 4 GiB
	f.Add([]byte{0xE0 | 0x0F, 0xFF, 0xFF, 0xFF})                     // a word of 256 MiB
	f.Add(append([]byte{0xC0 | 0x10, 0x00, 0x01}, "!re"...))         // a word just over 1 MiB, cut short
	f.Add([]byte{0xF8, 0x00})                                        // a reserved control byte
	f.Add(append(wire([]string{"!re", "=a=b"}), 0x85, 0x00, 'x'))    // ends inside a word
	f.Add(bytes.Repeat([]byte{0}, 64))                               // empty sentences only
	f.Add(wire([]string{"/login", "=name=nms", "=password=secret"})) // a request, not a reply

	f.Fuzz(func(t *testing.T, data []byte) {
		// The reader on its own.
		r := proto.NewReader(bytes.NewReader(data))
		for {
			r.BeginReply()
			sen, err := r.ReadSentence()
			if err != nil {
				break
			}
			if len(sen.Word) > proto.MaxWordLen || len(sen.Tag) > proto.MaxWordLen {
				t.Fatalf("a word of %d bytes passed the reader", max(len(sen.Word), len(sen.Tag)))
			}
			for _, p := range sen.List {
				if len(p.Key)+len(p.Value) > proto.MaxWordLen {
					t.Fatalf("an attribute of %d bytes passed the reader", len(p.Key)+len(p.Value))
				}
			}
		}

		// The reply loop, as RunCommand drives it.
		conn := &replayConn{data: bytes.NewReader(data)}
		client, err := ros.NewClient(conn)
		if err != nil {
			t.Fatal(err)
		}
		client.SetLogHandler(clientLogHandler{})
		defer client.Close()
		for range 8 {
			reply, err := client.RunArgs([]string{"/fuzz/print"})
			if reply != nil && len(reply.Re) > ros.MaxReplyRows {
				t.Fatalf("a reply of %d rows", len(reply.Re))
			}
			if err == nil {
				if reply == nil || reply.Done == nil {
					t.Fatalf("a command succeeded without a !done: %v", reply)
				}
				continue
			}
			var fatal *ros.SessionFatalError
			if !errors.As(err, &fatal) {
				// Only the device's own refusal leaves the session usable.
				var de *ros.DeviceError
				if !errors.As(err, &de) || de.Sentence.Word != "!trap" {
					t.Fatalf("an error that is neither a trap nor session-fatal: %v (%T)", err, err)
				}
				if IsSessionFatal(err) {
					t.Fatalf("a trap classified as session-fatal: %v", err)
				}
				continue
			}
			if !IsSessionFatal(err) {
				t.Fatalf("IsSessionFatal(%v) = false for a SessionFatalError", err)
			}
			if !conn.closed {
				t.Fatalf("the connection stayed open after a session-fatal error: %v", err)
			}
			left := conn.data.Len()
			if _, again := client.RunArgs([]string{"/fuzz/print"}); !errors.As(again, &fatal) {
				t.Fatalf("a command after a session-fatal error returned %v", again)
			}
			if conn.data.Len() != left {
				t.Fatal("a command read from the stream after a session-fatal error")
			}
			return
		}
	})
}
