package routeros

// Tests of the local patch (see README.md, "Local changes").

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/go-routeros/routeros/v3/proto"
)

// deferCloser is the helper upstream's proto_test.go uses; it lived in
// client_test.go, which this copy drops (it dialled a live device).
func deferCloser(t *testing.T, c io.Closer) {
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// scripted is a connection whose peer already wrote everything it will ever
// say: reads drain reply, writes are discarded.
type scripted struct {
	reply  *bytes.Reader
	closed bool
}

func (s *scripted) Read(p []byte) (int, error)  { return s.reply.Read(p) }
func (s *scripted) Write(p []byte) (int, error) { return len(p), nil }
func (s *scripted) Close() error                { s.closed = true; return nil }

// encode puts sentences on the wire. It encodes by hand rather than through
// proto.Writer, which starts a goroutine for every word it writes: a
// 50,000-row reply would take minutes under the race detector on a small
// CI runner. TestEncodeMatchesTheWriter keeps the two in step.
func encode(sentences ...[]string) []byte {
	var buf bytes.Buffer
	for _, sen := range sentences {
		for _, word := range sen {
			buf.Write(encodeLength(len(word)))
			buf.WriteString(word)
		}
		buf.WriteByte(0)
	}
	return buf.Bytes()
}

func encodeLength(l int) []byte {
	switch {
	case l < 0x80:
		return []byte{byte(l)}
	case l < 0x4000:
		return []byte{byte(l>>8) | 0x80, byte(l)}
	case l < 0x200000:
		return []byte{byte(l>>16) | 0xC0, byte(l >> 8), byte(l)}
	case l < 0x10000000:
		return []byte{byte(l>>24) | 0xE0, byte(l >> 16), byte(l >> 8), byte(l)}
	default:
		return []byte{0xF0, byte(l >> 24), byte(l >> 16), byte(l >> 8), byte(l)}
	}
}

// The hand encoder above writes what proto.Writer writes, for every length
// class a word can have.
func TestEncodeMatchesTheWriter(t *testing.T) {
	var sens [][]string
	for _, n := range []int{0, 1, 0x7F, 0x80, 0x3FFF, 0x4000, 0x1FFFFF, 0x200000} {
		sens = append(sens, []string{"!re", "=k=" + string(bytes.Repeat([]byte{'x'}, n))})
	}
	sens = append(sens, []string{"!done"})

	var want bytes.Buffer
	w := proto.NewWriter(&want)
	for _, sen := range sens {
		w.BeginSentence()
		for _, word := range sen {
			w.WriteWord(word)
		}
		if err := w.EndSentence(); err != nil {
			t.Fatal(err)
		}
	}
	if got := encode(sens...); !bytes.Equal(got, want.Bytes()) {
		t.Errorf("encode differs from proto.Writer (%d vs %d bytes)", len(got), want.Len())
	}
}

func scriptedClient(t *testing.T, reply []byte) (*Client, *scripted) {
	t.Helper()
	conn := &scripted{reply: bytes.NewReader(reply)}
	c, err := NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	return c, conn
}

// wantFatal asserts err ends the session: typed, the connection closed, and
// the client refusing every later command with the same verdict.
func wantFatal(t *testing.T, c *Client, conn *scripted, err error) *SessionFatalError {
	t.Helper()
	var fatal *SessionFatalError
	if !errors.As(err, &fatal) {
		t.Fatalf("err = %v (%T), want a *SessionFatalError", err, err)
	}
	if !conn.closed {
		t.Error("the connection was left open after a session-fatal error")
	}
	if _, again := c.Run("/system/identity/print"); !errors.As(again, &fatal) {
		t.Errorf("a command after the fatal error returned %v, want a *SessionFatalError", again)
	}
	return fatal
}

// A !fatal ends the session and still reads as the device's error.
func TestFatalIsSessionFatal(t *testing.T) {
	c, conn := scriptedClient(t, encode([]string{"!fatal", "=message=session terminated on request"}))
	_, err := c.Run("/ip/address/print")
	wantFatal(t, c, conn, err)
	var de *DeviceError
	if !errors.As(err, &de) || de.Sentence.Word != "!fatal" {
		t.Errorf("err = %v, want it to unwrap to the !fatal DeviceError", err)
	}
}

// A !trap is the device declining one command: the session stays usable.
func TestTrapIsNotSessionFatal(t *testing.T) {
	c, conn := scriptedClient(t, encode(
		[]string{"!trap", "=message=no such command"}, []string{"!done"},
		[]string{"!re", "=name=router"}, []string{"!done"},
	))
	_, err := c.Run("/nope")
	var de *DeviceError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v, want a DeviceError", err)
	}
	var fatal *SessionFatalError
	if errors.As(err, &fatal) || conn.closed {
		t.Fatalf("a trap ended the session (err %v, closed %v)", err, conn.closed)
	}
	reply, err := c.Run("/system/identity/print")
	if err != nil || len(reply.Re) != 1 {
		t.Fatalf("the command after the trap = %v, %v; want one row", reply, err)
	}
}

// An invalid sentence word, an unknown reply word and a transport error each
// leave the stream at an unknown position: session-fatal.
func TestDesynchronisedStreamIsSessionFatal(t *testing.T) {
	invalid := append([]byte{}, encode([]string{"!re"})...)
	invalid = append(invalid[:len(invalid)-1], 7, 'g', 'a', 'r', 'b', 'a', 'g', 'e', 0) // "!re", "garbage"

	t.Run("invalid word", func(t *testing.T) {
		c, conn := scriptedClient(t, invalid)
		_, err := c.Run("/x")
		wantFatal(t, c, conn, err)
		var pe *proto.ProtocolError
		if !errors.As(err, &pe) {
			t.Errorf("err = %v, want it to unwrap to a *proto.ProtocolError", err)
		}
	})
	t.Run("unknown reply word", func(t *testing.T) {
		c, conn := scriptedClient(t, encode([]string{"!what"}, []string{"!done"}))
		_, err := c.Run("/x")
		wantFatal(t, c, conn, err)
		var ue *UnknownReplyError
		if !errors.As(err, &ue) {
			t.Errorf("err = %v, want it to unwrap to an *UnknownReplyError", err)
		}
	})
	t.Run("end of stream mid-reply", func(t *testing.T) {
		c, conn := scriptedClient(t, encode([]string{"!re", "=name=ether1"}))
		_, err := c.Run("/x")
		wantFatal(t, c, conn, err)
		if !errors.Is(err, io.EOF) {
			t.Errorf("err = %v, want it to unwrap to io.EOF", err)
		}
	})
	t.Run("oversized word", func(t *testing.T) {
		c, conn := scriptedClient(t, []byte{0xF0, 0x7F, 0xFF, 0xFF, 0xFF})
		_, err := c.Run("/x")
		wantFatal(t, c, conn, err)
		var le *proto.LimitError
		if !errors.As(err, &le) || le.What != proto.LimitWord {
			t.Errorf("err = %v, want it to unwrap to a word *proto.LimitError", err)
		}
	})
}

// One command may return at most MaxReplyRows rows; the row after that ends
// the session instead of growing the reply.
func TestRowCap(t *testing.T) {
	var rows [][]string
	for i := 0; i < MaxReplyRows; i++ {
		rows = append(rows, []string{"!re", "=n=" + strconv.Itoa(i)})
	}
	atCap := encode(append(rows, []string{"!done"})...)

	c, _ := scriptedClient(t, atCap)
	reply, err := c.Run("/x")
	if err != nil || len(reply.Re) != MaxReplyRows {
		t.Fatalf("a reply of exactly %d rows = %d rows, %v", MaxReplyRows, len(reply.Re), err)
	}

	over := encode(append(rows, []string{"!re", "=n=one-too-many"}, []string{"!done"})...)
	c, conn := scriptedClient(t, over)
	_, err = c.Run("/x")
	wantFatal(t, c, conn, err)
	var le *proto.LimitError
	if !errors.As(err, &le) || le.What != proto.LimitRows || le.Limit != MaxReplyRows {
		t.Errorf("err = %v, want it to unwrap to a rows *proto.LimitError", err)
	}
}

// The reply budget is per command: many commands on one session may each
// return a large reply.
func TestReplyBudgetIsPerCommand(t *testing.T) {
	big := "=k=" + string(bytes.Repeat([]byte{'x'}, proto.MaxWordLen-3))
	var one [][]string
	for i := 0; i < 20; i++ { // 20 MiB per reply
		one = append(one, []string{"!re", big})
	}
	one = append(one, []string{"!done"})
	stream := bytes.Repeat(encode(one...), 3) // 60 MiB over three commands

	c, _ := scriptedClient(t, stream)
	for i := 0; i < 3; i++ {
		reply, err := c.Run("/x")
		if err != nil || len(reply.Re) != 20 {
			t.Fatalf("command %d = %v rows, %v; want 20 rows", i, reply, err)
		}
	}
}

// One reply over the cumulative cap ends the session.
func TestReplyCap(t *testing.T) {
	big := "=k=" + string(bytes.Repeat([]byte{'x'}, proto.MaxWordLen-3))
	var rows [][]string
	for i := 0; i < proto.MaxReplyLen/proto.MaxWordLen+1; i++ {
		rows = append(rows, []string{"!re", big})
	}
	c, conn := scriptedClient(t, encode(append(rows, []string{"!done"})...))
	_, err := c.Run("/x")
	wantFatal(t, c, conn, err)
	var le *proto.LimitError
	if !errors.As(err, &le) || le.What != proto.LimitReply {
		t.Errorf("err = %v, want it to unwrap to a reply *proto.LimitError", err)
	}
}
