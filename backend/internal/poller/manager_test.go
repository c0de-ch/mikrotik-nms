package poller

import (
	"testing"
	"time"
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
