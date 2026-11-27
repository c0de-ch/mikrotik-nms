package routeros

import (
	"strings"

	"github.com/go-routeros/routeros/v3/proto"
)

// MaxReplyRows caps the !re sentences of one reply (local patch): the row
// after it ends the session instead of growing the reply.
const MaxReplyRows = 50000

// Reply has all the sentences from a reply.
type Reply struct {
	Re   []*proto.Sentence
	Done *proto.Sentence
}

func (r *Reply) String() string {
	var sb strings.Builder
	for _, sen := range r.Re {
		sb.WriteString(sen.String())
		sb.WriteRune('\n')
	}

	if r.Done != nil {
		sb.WriteString(r.Done.String())
	}

	return sb.String()
}

func (r *Reply) processSentence(sen *proto.Sentence) (bool, error) {
	switch sen.Word {
	case reSentence:
		if len(r.Re) >= MaxReplyRows {
			return true, &proto.LimitError{What: proto.LimitRows, Size: int64(len(r.Re)) + 1, Limit: MaxReplyRows}
		}
		r.Re = append(r.Re, sen)
	case doneSentence:
		r.Done = sen
		return true, nil
	case trapSentence, fatalSentence:
		return sen.Word == fatalSentence, &DeviceError{sen}
	case "", emptySentence:
		// API docs say that empty sentences should be ignored
	default:
		return true, &UnknownReplyError{sen}
	}
	return false, nil
}
