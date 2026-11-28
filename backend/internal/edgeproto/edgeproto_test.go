package edgeproto

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/devclass"
)

// A tier runs at its cadence; not at all while the device is paused, nor
// when it is not in the device's plan — and the cadence stays what it is.
func TestDeviceSpecRunInterval(t *testing.T) {
	spec := DeviceSpec{ID: "r3", Cadence: map[devclass.Tier]time.Duration{devclass.TierARP: 15 * time.Minute}}
	if got := spec.RunInterval(devclass.TierARP); got != 15*time.Minute {
		t.Errorf("ARP runs every %v, want 15m", got)
	}
	if got := spec.RunInterval(devclass.TierDHCPLeases); got != 0 {
		t.Errorf("a tier outside the plan runs every %v, want 0", got)
	}
	spec.Paused = true
	if got := spec.RunInterval(devclass.TierARP); got != 0 {
		t.Errorf("paused: ARP runs every %v, want 0", got)
	}
	if spec.Cadence[devclass.TierARP] != 15*time.Minute {
		t.Error("the pause changed the cadence: staleness rules need it as it was")
	}
}
