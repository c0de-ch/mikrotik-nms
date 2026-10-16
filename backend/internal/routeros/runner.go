package routeros

import (
	"errors"

	ros "github.com/go-routeros/routeros/v3"
)

// Runner is the one-method seam between the device readers and the wire:
// production code wraps a pooled *ros.Client so every call goes through
// RunCommand's timeout and per-client mutex; tests script replies.
type Runner interface {
	RunArgs(sentences []string) (*ros.Reply, error)
}

// clientRunner routes a pooled client's calls through RunCommand. A bare
// *ros.Client also satisfies Runner, but calling it directly would bypass
// CommandTimeout and the mutex that serialises a pooled session.
type clientRunner struct{ c *ros.Client }

func (r clientRunner) RunArgs(sentences []string) (*ros.Reply, error) {
	if len(sentences) == 0 {
		return nil, errors.New("routeros: empty command")
	}
	return RunCommand(r.c, sentences[0], sentences[1:]...)
}

func runnerFor(c *ros.Client) Runner { return clientRunner{c} }
