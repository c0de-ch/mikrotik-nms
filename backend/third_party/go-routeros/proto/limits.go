package proto

// Local patch (mikrotik-nms, see README.md "Local changes"): what a device
// may make the reader allocate is capped, and the cap is checked against the
// declared length before anything is allocated or read.

import "fmt"

const (
	// MaxWordLen is the longest word the reader accepts. The length prefix
	// can declare up to 4 GiB; upstream allocated whatever it said.
	MaxWordLen = 1 << 20
	// MaxReplyLen caps the wire bytes of all the words of one reply (from
	// BeginReply to the next), however they are spread over sentences.
	MaxReplyLen = 32 << 20
)

// What a LimitError is about.
const (
	LimitWord  = "word"
	LimitReply = "reply"
	LimitRows  = "rows"
)

// LimitError reports a reply that passed a cap. The reader stopped in the
// middle of the stream: the session must not be used again.
type LimitError struct {
	What  string // LimitWord, LimitReply or LimitRows
	Size  int64  // what the device sent (or declared)
	Limit int64
}

func (e *LimitError) Error() string {
	switch e.What {
	case LimitWord:
		return fmt.Sprintf("RouterOS reply word of %d bytes exceeds the %d byte limit", e.Size, e.Limit)
	case LimitRows:
		return fmt.Sprintf("RouterOS reply exceeds the limit of %d rows", e.Limit)
	}
	return fmt.Sprintf("RouterOS reply exceeds the limit of %d bytes", e.Limit)
}

// ProtocolError reports bytes that are not a RouterOS API sentence: a word
// that is neither a reply word, a tag nor an attribute, or a length byte the
// protocol reserves. The stream is desynchronised: the session must not be
// used again.
type ProtocolError struct {
	Msg string
}

func (e *ProtocolError) Error() string { return e.Msg }

// quoteLimit bounds how much of an invalid word an error message quotes.
const quoteLimit = 64

func invalidWord(b []byte) error {
	more := ""
	if len(b) > quoteLimit {
		b, more = b[:quoteLimit], "…"
	}
	return &ProtocolError{Msg: fmt.Sprintf("invalid RouterOS sentence word: %#q%s", b, more)}
}
