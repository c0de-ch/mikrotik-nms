package poller

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func TestStatusAfterFailedPoll(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	threshold := 2 * time.Minute

	recent := now.Add(-30 * time.Second)
	stale := now.Add(-5 * time.Minute)
	atBoundary := now.Add(-2 * time.Minute)

	tests := []struct {
		name     string
		lastSeen *time.Time
		want     string
	}{
		{"never seen", nil, "offline"},
		{"seen recently reports unknown within grace", &recent, "unknown"},
		{"unreachable past threshold goes offline", &stale, "offline"},
		{"exactly at threshold goes offline", &atBoundary, "offline"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusAfterFailedPoll(tt.lastSeen, threshold, now); got != tt.want {
				t.Errorf("statusAfterFailedPoll() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInterSwitchPorts(t *testing.T) {
	links := []queries.Link{
		{DeviceAID: "sw1", InterfaceA: "ether1,bridge", DeviceBID: "sw2", InterfaceB: "sfp-sfpplus1"},
		{DeviceAID: "sw2", InterfaceA: " ether5 , ", DeviceBID: "ap1", InterfaceB: ""},
	}
	got := interSwitchPorts(links)

	for _, key := range []string{"sw1:ether1", "sw1:bridge", "sw2:sfp-sfpplus1", "sw2:ether5"} {
		if !got[key] {
			t.Errorf("interSwitchPorts() missing %q", key)
		}
	}
	for _, key := range []string{"sw1:ether1,bridge", "ap1:", "sw2:"} {
		if got[key] {
			t.Errorf("interSwitchPorts() unexpectedly contains %q", key)
		}
	}
	if len(got) != 4 {
		t.Errorf("interSwitchPorts() has %d keys, want 4: %v", len(got), got)
	}
}
