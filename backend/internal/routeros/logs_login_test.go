package routeros

import "testing"

func TestParseLoginMessage(t *testing.T) {
	cases := []struct {
		in   string
		want *LoginEvent
	}{
		{"user admin logged in from 192.0.2.10 via api", &LoginEvent{Action: "login", User: "admin", Address: "192.0.2.10", Via: "api"}},
		{"user nms logged in from 2001:db8::1 via api-ssl", &LoginEvent{Action: "login", User: "nms", Address: "2001:db8::1", Via: "api-ssl"}},
		{"user admin logged out from 192.0.2.10 via ssh", &LoginEvent{Action: "logout", User: "admin", Address: "192.0.2.10", Via: "ssh"}},
		{"user admin logged in via console", &LoginEvent{Action: "login", User: "admin", Via: "console"}},
		{"user admin logged in from 192.0.2.11 via winbox", &LoginEvent{Action: "login", User: "admin", Address: "192.0.2.11", Via: "winbox"}},
		{"login failure for user admin from 192.0.2.12 via api", &LoginEvent{Action: "failure", User: "admin", Address: "192.0.2.12", Via: "api"}},
		{"login failure for user root via console", &LoginEvent{Action: "failure", User: "root", Via: "console"}},
		{"ether1 link up (speed 1G, full duplex)", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := ParseLoginMessage(c.in)
		if c.want == nil {
			if got != nil {
				t.Errorf("%q: expected no event, got %+v", c.in, *got)
			}
			continue
		}
		if got == nil {
			t.Errorf("%q: expected %+v, got nil", c.in, *c.want)
			continue
		}
		if *got != *c.want {
			t.Errorf("%q:\n got %+v\nwant %+v", c.in, *got, *c.want)
		}
	}
	api := ParseLoginMessage("user a logged in from 192.0.2.1 via api-ssl")
	ssh := ParseLoginMessage("user a logged in from 192.0.2.1 via ssh")
	if !api.IsAPI() || ssh.IsAPI() {
		t.Fatal("IsAPI")
	}
}

func TestLoginEventsFromAndActiveUsers(t *testing.T) {
	entries := []LogEntry{
		{ID: "*1", Time: "10:00:00", Topics: "system,info,account", Message: "user admin logged in from 192.0.2.10 via api"},
		{ID: "*2", Time: "10:00:01", Topics: "system,info", Message: "user admin logged in from 192.0.2.10 via api"}, // wrong topic
		{ID: "*3", Time: "10:00:02", Topics: "system,error,critical", Message: "login failure for user admin from 192.0.2.12 via ssh"},
		{ID: "*4", Time: "10:00:03", Topics: "system,info,account", Message: "something else entirely"},
	}
	got := LoginEventsFrom(entries)
	if len(got) != 2 || got[0].Action != "login" || got[0].Time != "10:00:00" || got[1].Action != "failure" || got[1].Topics != "system,error,critical" {
		t.Fatalf("events: %+v", got)
	}

	f := &fakeRunner{byCmd: map[string]fakeReply{
		"/user/active/print": rep(
			map[string]string{"name": "nms", "address": "192.0.2.1", "via": "API", "when": "jan/01 10:00:00", "group": "read"},
			map[string]string{"name": "admin", "address": "192.0.2.10", "via": "winbox", "when": "jan/01 10:01:00", "group": "full"},
		),
	}}
	users, err := GetActiveUsers(f)
	if err != nil || len(users) != 2 || users[0].Via != "api" || users[1].Group != "full" {
		t.Fatalf("active users: %+v, %v", users, err)
	}
	if f.calls[0] != "/user/active/print =.proplist=name,address,via,when,group" {
		t.Fatalf("sent %q", f.calls[0])
	}
}
