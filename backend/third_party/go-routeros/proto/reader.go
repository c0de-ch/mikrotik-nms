package proto

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// Reader reads sentences from a RouterOS device.
type Reader interface {
	ReadSentence() (*Sentence, error)
	// BeginReply starts a new MaxReplyLen budget (local patch): the caller
	// marks where one reply ends and the next begins.
	BeginReply()
	Cancel()
	Close()
}

type reader struct {
	*ctxReader
	// budget is how many wire bytes the current reply may still take
	// (local patch, see limits.go).
	budget int64
}

// NewReader returns a new Reader to read from r.
func NewReader(r io.Reader) Reader {
	return &reader{
		ctxReader: &ctxReader{Reader: bufio.NewReader(r), done: make(chan struct{})},
		budget:    MaxReplyLen,
	}
}

// BeginReply gives the next reply a full budget.
func (r *reader) BeginReply() { r.budget = MaxReplyLen }

// charge takes n wire bytes from the reply budget before they are read.
func (r *reader) charge(n int64) error {
	if r.budget -= n; r.budget < 0 {
		return &LimitError{What: LimitReply, Size: MaxReplyLen - r.budget, Limit: MaxReplyLen}
	}
	return nil
}

// ReadSentence reads a sentence.
func (r *reader) ReadSentence() (*Sentence, error) {
	sen := NewSentence()
	for {
		b, err := r.readWord()
		if err != nil {
			return nil, err
		}
		if len(b) == 0 {
			return sen, nil
		}
		// Ex.: !re, !done
		if sen.Word == "" {
			sen.Word = string(b)
			continue
		}
		// Command tag.
		if bytes.HasPrefix(b, []byte(".tag=")) {
			sen.Tag = string(b[5:])
			continue
		}
		// Ex.: =key=value, =key
		if bytes.HasPrefix(b, []byte("=")) {
			t := bytes.SplitN(b[1:], []byte("="), 2)
			if len(t) == 1 {
				t = append(t, []byte{})
			}
			p := Pair{string(t[0]), string(t[1])}
			sen.List = append(sen.List, p)
			sen.Map[p.Key] = p.Value
			continue
		}
		return nil, invalidWord(b)
	}
}

func (r *reader) readNumber(size int) (int64, error) {
	if err := r.charge(int64(size)); err != nil {
		return -1, err
	}
	b := make([]byte, size)
	_, err := io.ReadFull(r, b)
	if err != nil {
		return -1, err
	}
	var num int64
	for _, ch := range b {
		num = num<<8 | int64(ch)
	}
	return num, nil
}

func (r *reader) readLength() (int64, error) {
	l, err := r.readNumber(1)
	if err != nil {
		return -1, err
	}
	var n int64
	switch {
	case l&0x80 == 0x00:
	case (l & 0xC0) == 0x80:
		n, err = r.readNumber(1)
		l = l & ^0xC0 << 8 | n
	case l&0xE0 == 0xC0:
		n, err = r.readNumber(2)
		l = l & ^0xE0 << 16 | n
	case l&0xF0 == 0xE0:
		n, err = r.readNumber(3)
		l = l & ^0xF0 << 24 | n
	case l&0xF8 == 0xF0:
		l, err = r.readNumber(4)
	default:
		// 0xF8 and above are reserved control bytes, not lengths.
		return -1, &ProtocolError{Msg: fmt.Sprintf("reserved RouterOS length byte %#x", l)}
	}
	if err != nil {
		return -1, err
	}
	return l, nil
}

func (r *reader) readWord() ([]byte, error) {
	l, err := r.readLength()
	if err != nil {
		return nil, err
	}
	// Check what the device declared before allocating or reading it.
	if l > MaxWordLen {
		return nil, &LimitError{What: LimitWord, Size: l, Limit: MaxWordLen}
	}
	if err := r.charge(l); err != nil {
		return nil, err
	}
	b := make([]byte, l)
	_, err = io.ReadFull(r, b)
	if err != nil {
		return nil, err
	}
	return b, nil
}
