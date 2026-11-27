package routeros

import (
	"errors"
	"reflect"
	"testing"
)

func TestReadConfigSendsAProplistAndKeepsOnlyItsFields(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		// The device answers with more than it was asked for (a fake does,
		// and so does a RouterOS that ignores the proplist): nothing but
		// the requested fields may come back.
		"/snmp/community/print": rep(
			map[string]string{"name": "s3cret-community", "addresses": "::/0", "security": "none",
				"authentication-password": "hunter2", "read-access": "true"},
		),
	}}
	got := ReadConfig(f, ConfigRead{Command: "/snmp/community/print", Fields: []string{"addresses", "security", "read-access", "write-access"}})
	if got.State != ConfigOK || got.Err != nil {
		t.Fatalf("state = %s, err = %v", got.State, got.Err)
	}
	want := []map[string]string{{"addresses": "::/0", "security": "none", "read-access": "true"}}
	if !reflect.DeepEqual(got.Rows, want) {
		t.Fatalf("rows = %v\nwant   %v", got.Rows, want)
	}
	if f.calls[0] != "/snmp/community/print =.proplist=addresses,security,read-access,write-access" {
		t.Fatalf("sent %q", f.calls[0])
	}
}

func TestReadConfigCountOnlyWithAQuery(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/firewall/filter/print": {done: map[string]string{"ret": "7"}},
	}}
	got := ReadConfig(f, ConfigRead{Command: "/ip/firewall/filter/print", Query: []string{"?chain=forward"}, CountOnly: true})
	if got.State != ConfigOK || got.Count != 7 || len(got.Rows) != 0 {
		t.Fatalf("got %+v", got)
	}
	if f.calls[0] != "/ip/firewall/filter/print =count-only= ?chain=forward" {
		t.Fatalf("sent %q", f.calls[0])
	}
}

func TestReadConfigTellsAbsentDeniedAndFailedApart(t *testing.T) {
	f := &fakeRunner{byCmd: map[string]fakeReply{
		"/user/group/print":         {err: trapErr("not enough permissions (9)")},
		"/tool/sniffer/print":       {err: errors.New("read tcp: i/o timeout")},
		"/system/device-mode/print": {err: trapErr("unknown parameter")},
	}}
	for _, c := range []struct {
		command string
		want    ConfigState
	}{
		{"/interface/wifi/capsman/print", ConfigAbsent}, // no such command: the menu is not there
		{"/user/group/print", ConfigDenied},             // the account may not read it — never "clean"
		{"/tool/sniffer/print", ConfigFailed},           // a transport failure says nothing
		{"/system/device-mode/print", ConfigFailed},     // any other trap is a failure, not "absent"
	} {
		got := ReadConfig(f, ConfigRead{Command: c.command, Fields: []string{"enabled"}})
		if got.State != c.want {
			t.Errorf("%s: state = %s, want %s (err %v)", c.command, got.State, c.want, got.Err)
		}
		if c.want == ConfigFailed && got.Err == nil {
			t.Errorf("%s: a failed read must carry its error", c.command)
		}
		if len(got.Rows) != 0 {
			t.Errorf("%s: rows from a read that did not succeed: %v", c.command, got.Rows)
		}
	}
}

// A read that names no fields would return everything the menu has —
// passwords included — and one that names a secret would ask for it. Neither
// is ever sent.
func TestReadConfigRefusesUnboundedAndSecretReads(t *testing.T) {
	for _, r := range []ConfigRead{
		{Command: "/user/print"},
		{Command: "/user/print", Fields: []string{"name", "password"}},
		{Command: "/ppp/secret/print", Fields: []string{"name", "secret"}},
		{Command: "/interface/wifi/security/print", Fields: []string{"name", "passphrase"}},
		{Command: "/ip/ipsec/identity/print", Fields: []string{"peer", "secret"}},
		{Command: "/interface/wireguard/print", Fields: []string{"name", "private-key"}},
		{Command: "/snmp/print", Fields: []string{"enabled", "trap-community"}},
		{Command: "print", Fields: []string{"enabled"}},
		{Command: "/snmp/set", Fields: []string{"enabled"}},
	} {
		f := &fakeRunner{byCmd: map[string]fakeReply{r.Command: rep(map[string]string{"name": "x"})}}
		got := ReadConfig(f, r)
		if got.State != ConfigFailed || got.Err == nil {
			t.Errorf("%s %v: state = %s, want it refused", r.Command, r.Fields, got.State)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s %v: the device was sent %q", r.Command, r.Fields, f.calls)
		}
	}
}

func TestIsSecretField(t *testing.T) {
	for _, name := range []string{"password", "authentication-password", "encryption-password", "secret", "passphrase",
		"private-key", "preshared-key", "pre-shared-key", "wpa-pre-shared-key", "wpa2-pre-shared-key", "trap-community",
		"community", "key", "supplicant-password", "wep-key", "radius-password", "api-key", "token", "PASSWORD"} {
		if !IsSecretField(name) {
			t.Errorf("%q is a secret field", name)
		}
	}
	for _, name := range []string{"name", "enabled", "addresses", "security", "policy", "group", "via", "topics",
		"remote", "interval", "mode", "use-ip-firewall", "allow-remote-requests", "mirror-target"} {
		if IsSecretField(name) {
			t.Errorf("%q is not a secret field", name)
		}
	}
}
