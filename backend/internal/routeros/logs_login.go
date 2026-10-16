package routeros

import (
	"regexp"
	"strings"
)

// LoginEvent is a parsed account line from the device log: who logged in or
// out (or failed to), from where and over which service.
//
// RouterOS 7 writes them as
//
//	user admin logged in from 192.0.2.10 via api              (system,info,account)
//	user admin logged out from 192.0.2.10 via api-ssl         (system,info,account)
//	user admin logged in via console                          (no address)
//	login failure for user admin from 192.0.2.10 via ssh      (system,error,critical)
type LoginEvent struct {
	Time    string
	Topics  string
	Message string

	Action  string // "login", "logout" or "failure"
	User    string
	Address string // "" for console / serial sessions
	Via     string // api, api-ssl, ssh, winbox, web, telnet, console, …
}

// IsAPI reports whether the session used the RouterOS API — the only
// service the NMS itself uses, so another API client is a second poller.
func (e *LoginEvent) IsAPI() bool {
	return e.Via == "api" || e.Via == "api-ssl"
}

// Fingerprint identifies the line within one device's buffer.
func (e *LoginEvent) Fingerprint() string {
	return e.Time + "\x1f" + e.Message
}

var (
	loginLineRe    = regexp.MustCompile(`^user\s+(\S+)\s+logged\s+(in|out)(?:\s+from\s+(\S+))?\s+via\s+(\S+)$`)
	loginFailureRe = regexp.MustCompile(`^login failure for user\s+(\S+)(?:\s+from\s+(\S+))?\s+via\s+(\S+)$`)
)

// ParseLoginMessage parses the message portion of an account log line.
// Returns nil if the message is not a login / logout / failure line.
func ParseLoginMessage(msg string) *LoginEvent {
	msg = strings.TrimSpace(msg)
	if m := loginLineRe.FindStringSubmatch(msg); m != nil {
		ev := &LoginEvent{Action: "login", User: m[1], Address: m[3], Via: strings.ToLower(m[4])}
		if m[2] == "out" {
			ev.Action = "logout"
		}
		return ev
	}
	if m := loginFailureRe.FindStringSubmatch(msg); m != nil {
		return &LoginEvent{Action: "failure", User: m[1], Address: m[2], Via: strings.ToLower(m[3])}
	}
	return nil
}

// LoginEventsFrom picks the account lines out of a log read. Successful
// logins carry the "account" topic; failures are logged under
// system,error,critical, so the message itself decides.
func LoginEventsFrom(entries []LogEntry) []LoginEvent {
	out := make([]LoginEvent, 0, 4)
	for i := range entries {
		e := &entries[i]
		if !hasTopic(e.Topics, "account") && !strings.HasPrefix(e.Message, "login failure") {
			continue
		}
		parsed := ParseLoginMessage(e.Message)
		if parsed == nil {
			continue
		}
		parsed.Time, parsed.Topics, parsed.Message = e.Time, e.Topics, e.Message
		out = append(out, *parsed)
	}
	return out
}

// ActiveUser is one /user/active row: a session open on the device right now.
type ActiveUser struct {
	Name    string
	Address string
	Via     string
	When    string
	Group   string
}

// GetActiveUsers lists the sessions currently open on the device. One cheap
// proplisted print; the login watch uses it to confirm that an API login it
// read in the log is still there.
func GetActiveUsers(run Runner) ([]ActiveUser, error) {
	reply, err := run.Run("/user/active/print", "=.proplist=name,address,via,when,group")
	if err != nil {
		return nil, err
	}
	out := make([]ActiveUser, 0, len(reply.Re))
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		out = append(out, ActiveUser{
			Name: m["name"], Address: m["address"], Via: strings.ToLower(m["via"]),
			When: m["when"], Group: m["group"],
		})
	}
	return out, nil
}
