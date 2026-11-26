package routeros

import (
	"errors"
	"fmt"

	"github.com/go-routeros/routeros/v3/proto"
)

var (
	errAlreadyAsync   = errors.New("method Async() has already been called")
	errAsyncLoopEnded = errors.New("method Async(): loop has ended - probably read error")
)

// UnknownReplyError records the sentence whose Word is unknown.
type UnknownReplyError struct {
	Sentence *proto.Sentence
}

func (err *UnknownReplyError) Error() string {
	return "unknown RouterOS reply word: " + err.Sentence.Word
}

// DeviceError records the sentence containing the error received from the device.
// The sentence may have Word !trap or !fatal.
type DeviceError struct {
	Sentence *proto.Sentence
}

func (err *DeviceError) fetchMessage() string {
	if m := err.Sentence.Map["message"]; m != "" {
		return m
	}

	return "unknown error: " + err.Sentence.String()
}

func (err *DeviceError) Error() string {
	return fmt.Sprintf("from RouterOS device: %s", err.fetchMessage())
}

// SessionFatalError (local patch) wraps an error after which the session
// must not be used again: a !fatal, a reply over a cap (proto.LimitError),
// bytes that are not a sentence (proto.ProtocolError), an unknown reply word
// or a transport error in the middle of a command. The reply stream is at an
// unknown position, so reading on would hand one command the answer of
// another. The client closes its connection before returning it, and answers
// every later command with it.
type SessionFatalError struct {
	Err error
}

// Error is the wrapped error's text, unchanged: the type is what marks the
// session as ended.
func (e *SessionFatalError) Error() string { return e.Err.Error() }

func (e *SessionFatalError) Unwrap() error { return e.Err }
