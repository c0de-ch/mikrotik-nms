package routeros

import (
	"errors"
	"testing"

	"github.com/mikrotik-nms/backend/internal/crypto"
)

// A dial whose password is the stand-in for an undecryptable stored secret
// is refused before any connection is opened: no login is ever tried with it.
func TestDialRefusesUnreadableCredentials(t *testing.T) {
	SetDialPolicy(nil)
	t.Cleanup(func() { SetDialPolicy(nil) })

	// 192.0.2.1 (TEST-NET-1) is unroutable: reaching the network here would
	// hang or fail with a different error.
	pool := NewPool()
	if _, err := pool.Dial("d1", Target{Address: "192.0.2.1", Port: 8728}, "admin", crypto.Unreadable); !errors.Is(err, ErrCredentialsUnreadable) {
		t.Fatalf("Pool.Dial = %v, want ErrCredentialsUnreadable", err)
	}
	if _, err := DialOnce(Target{Address: "192.0.2.1", Port: 8728}, "admin", crypto.Unreadable); !errors.Is(err, ErrCredentialsUnreadable) {
		t.Fatalf("DialOnce = %v, want ErrCredentialsUnreadable", err)
	}
	// EnsureConnection gives up at once instead of retrying a refusal.
	if _, err := pool.EnsureConnection("d1", Target{Address: "192.0.2.1", Port: 8728}, "admin", crypto.Unreadable); !errors.Is(err, ErrCredentialsUnreadable) {
		t.Fatalf("EnsureConnection = %v, want ErrCredentialsUnreadable", err)
	}
	if pool.Get("d1") != nil {
		t.Fatal("a refused dial left a pooled client")
	}
	// Nothing was remembered either: GetLive must not redial it.
	if pool.GetLive("d1") != nil {
		t.Fatal("a refused dial left dial parameters behind")
	}
}
