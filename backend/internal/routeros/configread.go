package routeros

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Configuration reads for the hygiene audit and the diagnose pass.
//
// Every one of them is a print that changes nothing on the device and names
// the fields it wants: a .proplist (or =count-only=, which returns no rows
// at all). The reply is cut down to those fields again on this side, so
// whatever a device sends beyond them — a RouterOS that ignores the proplist,
// a menu that grew a property — never reaches the caller, let alone the
// database. A field whose name says it holds a secret cannot be asked for at
// all.

// ConfigRead is one read-only configuration print.
type ConfigRead struct {
	// Command is the print, e.g. "/snmp/community/print".
	Command string
	// Fields is the .proplist and, at the same time, the only fields kept
	// from the reply. Required unless CountOnly.
	Fields []string
	// Query are the query words, e.g. "?chain=forward".
	Query []string
	// CountOnly asks for the number of rows instead of the rows.
	CountOnly bool
}

// ConfigState is how a configuration read ended.
type ConfigState string

const (
	// ConfigOK: the device answered.
	ConfigOK ConfigState = "ok"
	// ConfigAbsent: "no such command" — the menu does not exist on this
	// device (a package that is not installed, an older RouterOS). Nothing
	// can be configured there, so it is not an error.
	ConfigAbsent ConfigState = "absent"
	// ConfigDenied: "not enough permissions" — the menu is there and the
	// NMS account may not read it. Never the same as "nothing found".
	ConfigDenied ConfigState = "denied"
	// ConfigUnsupported: the device answered the read with another trap —
	// it knows the menu and will not take this print (a property or a query
	// this RouterOS version does not have). Asking again gets the same
	// answer; it says nothing about what is configured there.
	ConfigUnsupported ConfigState = "unsupported"
	// ConfigFailed: the read did not get an answer (a transport error, a
	// timeout, a reply that makes no sense). It says nothing about the
	// device's configuration, and is worth another try.
	ConfigFailed ConfigState = "failed"
)

// ConfigReply is the answer to a ConfigRead.
type ConfigReply struct {
	State ConfigState
	// Rows are the reply rows, each cut down to the read's Fields (a field
	// the device did not return is absent from the row).
	Rows []map[string]string
	// Count is the row count of a CountOnly read.
	Count int
	// Err is set when State is ConfigFailed or ConfigUnsupported.
	Err error
}

// secretFieldParts are the fragments that mark a property as holding a
// secret: passwords and passphrases, pre-shared and private keys, PPP and
// IPsec secrets, SNMP community names.
var secretFieldParts = []string{"password", "passwd", "passphrase", "secret", "shared-key", "private-key",
	"community", "token", "psk"}

// IsSecretField reports a RouterOS property whose name says its value is a
// secret. ReadConfig refuses to ask for one.
func IsSecretField(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "key" || strings.HasSuffix(n, "-key") {
		return true
	}
	for _, part := range secretFieldParts {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

// validate reports why the read must not be sent, nil when it may.
func (r ConfigRead) validate() error {
	if !strings.HasPrefix(r.Command, "/") || !strings.HasSuffix(r.Command, "/print") {
		return fmt.Errorf("routeros: %q is not a print", r.Command)
	}
	if r.CountOnly {
		return nil
	}
	if len(r.Fields) == 0 {
		return fmt.Errorf("routeros: %s without a .proplist is not sent", r.Command)
	}
	for _, f := range r.Fields {
		if IsSecretField(f) {
			return fmt.Errorf("routeros: %s: the field %q is a secret and is never read", r.Command, f)
		}
	}
	return nil
}

// Words are the argument words of the read as sent.
func (r ConfigRead) Words() []string {
	var words []string
	if r.CountOnly {
		words = append(words, "=count-only=")
	} else {
		words = append(words, "=.proplist="+strings.Join(r.Fields, ","))
	}
	return append(words, r.Query...)
}

// String is the read as one line, for logs and reports.
func (r ConfigRead) String() string {
	return r.Command + " " + strings.Join(r.Words(), " ")
}

// ReadConfig runs one configuration read. It never returns more than the
// read's own fields, and it never sends a read that names no fields or names
// a secret.
func ReadConfig(run Runner, r ConfigRead) ConfigReply {
	if err := r.validate(); err != nil {
		return ConfigReply{State: ConfigFailed, Err: err}
	}
	reply, err := run.Run(r.Command, r.Words()...)
	switch {
	case err == nil:
	case IsNoSuchCommand(err):
		return ConfigReply{State: ConfigAbsent}
	case IsPermissionDenied(err):
		return ConfigReply{State: ConfigDenied}
	case isTrap(err) && !IsSessionFatal(err): // a !trap; a !fatal ended the session and is a failure
		return ConfigReply{State: ConfigUnsupported, Err: err}
	default:
		return ConfigReply{State: ConfigFailed, Err: err}
	}
	if r.CountOnly {
		if reply == nil || reply.Done == nil {
			return ConfigReply{State: ConfigFailed, Err: errors.New("count-only: no !done sentence")}
		}
		n, err := strconv.Atoi(strings.TrimSpace(reply.Done.Map["ret"]))
		if err != nil {
			return ConfigReply{State: ConfigFailed, Err: errors.New("count-only: missing ret in !done")}
		}
		return ConfigReply{State: ConfigOK, Count: n}
	}
	out := ConfigReply{State: ConfigOK}
	if reply == nil {
		return out
	}
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		row := make(map[string]string, len(r.Fields))
		for _, f := range r.Fields {
			if v, ok := m[f]; ok {
				row[f] = v
			}
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}

// Bool reads a RouterOS boolean ("true"/"false", "yes"/"no").
func Bool(v string) bool { return rosBool(v) }
