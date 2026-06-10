package poller

import "sync"

// runGuard tracks in-flight dedicated-connection runs keyed "<kind>:<id>"
// ("speedtest:<testID>" / "traceroute:<targetID>"). It is package-level and
// SHARED between the scheduled pollers and the API's run-now handlers so a
// scheduled run and a run-now can never download/trace concurrently for the
// same test/target.
var runGuard sync.Map

// TryBeginRun claims the run slot for key, returning true when acquired.
// Callers MUST EndRun(key) when the run finishes (defer it).
func TryBeginRun(key string) bool {
	_, held := runGuard.LoadOrStore(key, true)
	return !held
}

// EndRun releases the run slot claimed by TryBeginRun.
func EndRun(key string) {
	runGuard.Delete(key)
}

// apiRunSlots globally bounds API-triggered dedicated-connection runs (speed
// tests + traceroutes): each run-now dials its own DialOnce connection, so a
// burst of clicks across many tests/targets could otherwise stack unbounded
// goroutines and downloads.
var apiRunSlots = make(chan struct{}, 3)

// TryAcquireAPIRunSlot reserves one of the global API run slots without
// blocking; false means the cap is already fully in use.
func TryAcquireAPIRunSlot() bool {
	select {
	case apiRunSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseAPIRunSlot returns a slot taken by TryAcquireAPIRunSlot.
func ReleaseAPIRunSlot() {
	<-apiRunSlots
}
