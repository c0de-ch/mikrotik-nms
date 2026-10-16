package routeros

import (
	"errors"
	"strconv"
	"strings"

	ros "github.com/go-routeros/routeros/v3"
)

// Runner runs one RouterOS API command. It is the seam between the collection
// helpers and the wire: production code hands them a pooled client through
// ClientRunner, tests script the replies. Helpers that take a Runner never
// touch the connection themselves, so every device call still goes through
// RunCommand's serialization, timeout and dead-socket eviction.
type Runner interface {
	Run(command string, args ...string) (*ros.Reply, error)
}

// ClientRunner adapts a pooled *ros.Client to Runner.
type ClientRunner struct {
	Client *ros.Client
}

// Run executes the command through RunCommand.
func (c ClientRunner) Run(command string, args ...string) (*ros.Reply, error) {
	return RunCommand(c.Client, command, args...)
}

// deviceMessage returns the message of a !trap / !fatal reply, lower-cased,
// or "" when err is not a device-side error (transport failures, timeouts).
func deviceMessage(err error) string {
	var de *ros.DeviceError
	if !errors.As(err, &de) || de.Sentence == nil {
		return ""
	}
	return strings.ToLower(de.Sentence.Map["message"])
}

// IsNoSuchCommand reports a trap that means the menu or command does not
// exist on this device: the package is not installed (wifi, wireless,
// caps-man, …) or the RouterOS version predates it. Distinct from a transport
// error, which says nothing about the device's features.
func IsNoSuchCommand(err error) bool {
	return strings.Contains(deviceMessage(err), "no such command")
}

// IsPermissionDenied reports the "not enough permissions (9)" trap: the API
// user lacks the policy the command needs. Callers must not read it as "the
// feature is absent" — the menu is there, we just cannot see it.
func IsPermissionDenied(err error) bool {
	return strings.Contains(deviceMessage(err), "not enough permissions")
}

// CountOnly runs "<command> =count-only=" and returns the row count RouterOS
// reports in the !done sentence. It is the cheapest way to ask whether a menu
// exists and how big it is: the device formats no rows at all.
func CountOnly(run Runner, command string) (int, error) {
	reply, err := run.Run(command, "=count-only=")
	if err != nil {
		return 0, err
	}
	if reply == nil || reply.Done == nil {
		return 0, errors.New("count-only: no !done sentence")
	}
	n, err := strconv.Atoi(strings.TrimSpace(reply.Done.Map["ret"]))
	if err != nil {
		return 0, errors.New("count-only: missing ret in !done")
	}
	return n, nil
}
