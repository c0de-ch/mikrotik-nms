package proto

// Tests of the local patch (see README.md, "Local changes").

import (
	"bytes"
	"errors"
	"testing"
)

// word encodes one word with its length prefix.
func word(s string) []byte {
	return append(encodeLength(len(s)), s...)
}

// sentence encodes words followed by the end-of-sentence marker.
func sentence(words ...string) []byte {
	var b []byte
	for _, w := range words {
		b = append(b, word(w)...)
	}
	return append(b, 0)
}

// A declared word length over the cap is refused from the length prefix
// alone: the body is never asked for, so nothing of that size is allocated.
func TestReadWordRefusesOversizedLengthBeforeReading(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix []byte
		size   int64
	}{
		{"one byte over", encodeLength(MaxWordLen + 1), MaxWordLen + 1},
		{"four-byte form, 256 MiB", []byte{0xE0 | 0x0F, 0xFF, 0xFF, 0xFF}, 0x0FFFFFFF},
		{"five-byte form, 4 GiB", []byte{0xF0, 0xFF, 0xFF, 0xFF, 0xFF}, 0xFFFFFFFF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Only the prefix is available: a reader that tried to read the
			// body would report an unexpected EOF instead of the limit.
			r := NewReader(bytes.NewReader(tc.prefix))
			_, err := r.ReadSentence()
			var le *LimitError
			if !errors.As(err, &le) {
				t.Fatalf("err = %v, want a *LimitError", err)
			}
			if le.What != LimitWord || le.Size != tc.size || le.Limit != MaxWordLen {
				t.Errorf("limit error = %+v, want word %d over %d", le, tc.size, MaxWordLen)
			}
		})
	}
}

// A word of exactly the cap is still read.
func TestReadWordAtTheCap(t *testing.T) {
	big := "=k=" + string(bytes.Repeat([]byte{'x'}, MaxWordLen-3))
	r := NewReader(bytes.NewReader(sentence("!re", big)))
	sen, err := r.ReadSentence()
	if err != nil {
		t.Fatalf("ReadSentence: %v", err)
	}
	if got := len(sen.Map["k"]); got != MaxWordLen-3 {
		t.Errorf("value length = %d, want %d", got, MaxWordLen-3)
	}
}

// The words of one reply together may not exceed MaxReplyLen, however they
// are spread over sentences; BeginReply starts a new budget.
func TestReplyBudget(t *testing.T) {
	chunk := "=k=" + string(bytes.Repeat([]byte{'x'}, MaxWordLen-3))
	one := sentence("!re", chunk)
	n := MaxReplyLen/len(one) + 1 // enough sentences to pass the budget

	var stream bytes.Buffer
	for i := 0; i < n; i++ {
		stream.Write(one)
	}
	r := NewReader(&stream)
	r.BeginReply()
	var err error
	read := 0
	for read < n {
		if _, err = r.ReadSentence(); err != nil {
			break
		}
		read++
	}
	var le *LimitError
	if !errors.As(err, &le) || le.What != LimitReply || le.Limit != MaxReplyLen {
		t.Fatalf("after %d sentences err = %v, want a reply *LimitError", read, err)
	}
	if read != n-1 {
		t.Errorf("read %d sentences before the cap, want %d", read, n-1)
	}

	// A new reply gets a fresh budget.
	stream.Reset()
	stream.Write(one)
	r = NewReader(&stream)
	for i := 0; i < 3; i++ {
		r.BeginReply()
		if _, err := r.ReadSentence(); err != nil {
			t.Fatalf("ReadSentence after BeginReply: %v", err)
		}
		stream.Write(one)
	}
}

// A stream of empty sentences is not free either: every word costs its
// length byte, so an endless run of them ends at the budget.
func TestEmptySentencesCountAgainstTheBudget(t *testing.T) {
	r := NewReader(bytes.NewReader(make([]byte, 16))).(*reader)
	r.budget = 3
	for i := 0; i < 3; i++ {
		if _, err := r.ReadSentence(); err != nil {
			t.Fatalf("empty sentence %d: %v", i, err)
		}
	}
	_, err := r.ReadSentence()
	var le *LimitError
	if !errors.As(err, &le) || le.What != LimitReply {
		t.Fatalf("err = %v, want a reply *LimitError", err)
	}
}

// A word that is neither a reply word, a tag nor an attribute, and a length
// byte the protocol reserves, are protocol errors — not lengths to trust.
func TestProtocolErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream []byte
	}{
		{"second word without = or .tag=", sentence("!re", "garbage")},
		{"reserved control byte 0xF8", []byte{0xF8, 0x01, 0x02}},
		{"reserved control byte 0xFF", []byte{0xFF}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewReader(bytes.NewReader(tc.stream)).ReadSentence()
			var pe *ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v, want a *ProtocolError", err)
			}
		})
	}
}

// The error text of an invalid word quotes at most a short prefix of it.
func TestProtocolErrorIsShort(t *testing.T) {
	long := string(bytes.Repeat([]byte{'g'}, 100_000))
	_, err := NewReader(bytes.NewReader(sentence("!re", long))).ReadSentence()
	if err == nil || len(err.Error()) > 200 {
		t.Fatalf("err = %.80v… (%d bytes), want a short protocol error", err, len(err.Error()))
	}
}

// FuzzReadSentence: whatever the bytes, the reader returns sentences within
// the caps or an error — it never panics and never allocates past a cap.
func FuzzReadSentence(f *testing.F) {
	f.Add(sentence("!done"))
	f.Add(sentence("!re", "=name=ether1", ".tag=r1"))
	f.Add([]byte{0xF0, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte{0xF8})
	f.Add(sentence("!re", "garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bytes.NewReader(data))
		for {
			sen, err := r.ReadSentence()
			if err != nil {
				return
			}
			if len(sen.Word) > MaxWordLen {
				t.Fatalf("word of %d bytes", len(sen.Word))
			}
			for _, p := range sen.List {
				if len(p.Key)+len(p.Value) > MaxWordLen {
					t.Fatalf("attribute of %d bytes", len(p.Key)+len(p.Value))
				}
			}
		}
	})
}
