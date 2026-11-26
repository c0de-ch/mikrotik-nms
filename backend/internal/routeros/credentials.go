package routeros

import (
	"errors"
	"sync"

	ros "github.com/go-routeros/routeros/v3"
)

// Credentials are a RouterOS login an admin typed for one job (firmware
// upgrade, channel change, RouterBOARD upgrade). They are never stored,
// logged or pooled: the job dials one-shot sessions with them (DialOnceWith)
// and calls Zero when it ends.
//
// The password is kept as bytes so Zero can overwrite it. That is best
// effort: the request body it was decoded from, and the string each dial has
// to hand go-routeros, are copies Go gives no way to wipe; they become
// garbage once the job is done.
type Credentials struct {
	mu       sync.Mutex
	username string
	password []byte
}

// NewCredentials copies the typed login into a Credentials.
func NewCredentials(username, password string) *Credentials {
	return &Credentials{username: username, password: []byte(password)}
}

// Username is the login name.
func (c *Credentials) Username() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.username
}

// Zero overwrites the password; later dials with c fail.
func (c *Credentials) Zero() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.password {
		c.password[i] = 0
	}
	c.password = nil
}

// ErrCredentialsCleared is returned by DialOnceWith after Zero.
var ErrCredentialsCleared = errors.New("job credentials already cleared")

// DialOnceWith is DialOnce with job credentials: the same dial (policy,
// timeouts, pinned TLS), one dedicated session the caller must Close.
func DialOnceWith(t Target, creds *Credentials) (*ros.Client, error) {
	creds.mu.Lock()
	if creds.password == nil {
		creds.mu.Unlock()
		return nil, ErrCredentialsCleared
	}
	user, pass := creds.username, string(creds.password)
	creds.mu.Unlock()
	return DialOnce(t, user, pass)
}

// IsLoginRefused reports a dial the device answered with a trap: the TCP
// session opened and the device turned the login down (wrong user name or
// password, or the user may not use the API from this address).
func IsLoginRefused(err error) bool {
	return err != nil && isTrap(err)
}
