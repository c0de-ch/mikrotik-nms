package routeros_test

import (
	"sync"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

type recorder struct {
	mu       sync.Mutex
	commands []routeros.CommandEvent
	sessions []routeros.SessionEvent
}

func (r *recorder) Command(e routeros.CommandEvent) {
	r.mu.Lock()
	r.commands = append(r.commands, e)
	r.mu.Unlock()
}

func (r *recorder) Session(e routeros.SessionEvent) {
	r.mu.Lock()
	r.sessions = append(r.sessions, e)
	r.mu.Unlock()
}

// Every command on a pooled session reaches the observer with its device,
// reply rows and wall time, and every pooled dial is a session event; a
// one-shot session counts against a device only once it is tagged.
func TestObserverSeesCommandsAndSessions(t *testing.T) {
	rec := &recorder{}
	routeros.SetObserver(rec)
	t.Cleanup(func() { routeros.SetObserver(nil) })

	fake := routerostest.NewDevice(t)
	fake.SetReply("/interface/print", map[string]string{"name": "ether1"}, map[string]string{"name": "ether2"})
	fake.SetTrap("/interface/wifi/print", "no such command prefix")
	pool := routeros.NewPool(false)
	fake.Dial(t, pool, "dev-1")

	before := time.Now()
	if _, err := routeros.RunCommand(pool.Get("dev-1"), "/interface/print"); err != nil {
		t.Fatal(err)
	}
	if _, err := routeros.RunCommand(pool.Get("dev-1"), "/interface/wifi/print"); err == nil {
		t.Fatal("the trap did not surface")
	}

	once, err := routeros.DialOnce(fake.Host, fake.Port, "u", "p", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer once.Close()
	if _, err := routeros.RunCommandWithTimeout(once, time.Second, "/system/identity/print"); err != nil {
		t.Fatal(err)
	}
	routeros.TagSession(once, "dev-1")
	if _, err := routeros.RunCommandWithTimeout(once, time.Second, "/system/identity/print"); err != nil {
		t.Fatal(err)
	}
	routeros.UntagSession(once)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sessions) != 1 || rec.sessions[0].DeviceID != "dev-1" || rec.sessions[0].Err != nil {
		t.Errorf("sessions = %+v, want one successful dial of dev-1", rec.sessions)
	}
	if len(rec.commands) != 4 {
		t.Fatalf("commands = %+v, want 4", rec.commands)
	}
	c := rec.commands
	if c[0].DeviceID != "dev-1" || c[0].Command != "/interface/print" || c[0].Rows != 2 || c[0].Err != nil || c[0].Timeout {
		t.Errorf("print = %+v", c[0])
	}
	if c[0].Start.Before(before) || c[0].Wall <= 0 {
		t.Errorf("print timing = start %v wall %v", c[0].Start, c[0].Wall)
	}
	if c[1].Err == nil || c[1].Rows != 0 || c[1].DeviceID != "dev-1" {
		t.Errorf("trap = %+v", c[1])
	}
	if c[2].DeviceID != "" || c[3].DeviceID != "dev-1" {
		t.Errorf("one-shot devices = %q, %q; want \"\" then dev-1", c[2].DeviceID, c[3].DeviceID)
	}
}

// A command that hits its deadline is reported as a timeout against the
// device whose session it ended.
func TestObserverReportsTimeouts(t *testing.T) {
	rec := &recorder{}
	routeros.SetObserver(rec)
	t.Cleanup(func() { routeros.SetObserver(nil) })

	fake := routerostest.NewDevice(t)
	release := make(chan struct{})
	fake.OnCommand("/log/print", func() { <-release })
	t.Cleanup(func() { close(release) })
	pool := routeros.NewPool(false)
	fake.Dial(t, pool, "slow")

	if _, err := routeros.RunCommandWithTimeout(pool.Get("slow"), 30*time.Millisecond, "/log/print"); err == nil {
		t.Fatal("no timeout")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.commands) != 1 || !rec.commands[0].Timeout || rec.commands[0].DeviceID != "slow" || rec.commands[0].Err == nil {
		t.Errorf("commands = %+v, want one timeout on slow", rec.commands)
	}
}
