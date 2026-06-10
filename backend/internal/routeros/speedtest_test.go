package routeros

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestSummarizeFetch(t *testing.T) {
	tests := []struct {
		name        string
		replies     []map[string]string
		elapsed     time.Duration
		wantMbps    *float64
		wantBytes   int64
		wantDurMs   int64
		wantErr     bool
		errContains string
	}{
		{
			// Reported duration >= 5s: the device value wins over elapsed.
			name: "finished happy path",
			replies: []map[string]string{
				{"status": "downloading", "downloaded": "512", "total": "12500", "duration": "1s"},
				{"status": "downloading", "downloaded": "6250", "total": "12500", "duration": "4s"},
				{"status": "finished", "downloaded": "12500", "total": "12500", "duration": "8s"},
			},
			elapsed: 9 * time.Second,
			// 12500 KiB = 12800000 bytes; 12800000*8/8/1e6 = 12.8 Mbps
			wantMbps:  f(12.8),
			wantBytes: 12800000,
			wantDurMs: 8000,
		},
		{
			name: "KiB suffix on downloaded",
			replies: []map[string]string{
				{"status": "finished", "downloaded": "12500KiB", "total": "12500KiB", "duration": "8s"},
			},
			elapsed:   9 * time.Second,
			wantMbps:  f(12.8),
			wantBytes: 12800000,
			wantDurMs: 8000,
		},
		{
			name: "ms-resolution duration",
			replies: []map[string]string{
				{"status": "finished", "downloaded": "1000", "duration": "8s120ms"},
			},
			elapsed: 9 * time.Second,
			// 1000 KiB = 1024000 bytes; 1024000*8/8.12/1e6 ≈ 1.0089 Mbps
			wantMbps:  f(1024000 * 8 / 8.12 / 1e6),
			wantBytes: 1024000,
			wantDurMs: 8120,
		},
		{
			// Reported duration is whole-second (real capture: '1s'), so a
			// sub-second download reports 0 — elapsed wall clock fills in.
			name: "zero reported duration falls back to elapsed",
			replies: []map[string]string{
				{"status": "finished", "downloaded": "1000", "duration": "0s"},
			},
			elapsed: 800 * time.Millisecond,
			// 1000 KiB = 1024000 bytes; 1024000*8/0.8/1e6 = 10.24 Mbps
			wantMbps:  f(10.24),
			wantBytes: 1024000,
			wantDurMs: 800,
		},
		{
			// 1-4s reported durations quantize badly: elapsed is preferred.
			name: "short reported duration prefers elapsed",
			replies: []map[string]string{
				{"status": "finished", "downloaded": "1000", "duration": "1s"},
			},
			elapsed: 1500 * time.Millisecond,
			// 1000 KiB = 1024000 bytes; 1024000*8/1.5/1e6 ≈ 5.4613 Mbps
			wantMbps:  f(1024000 * 8 / 1.5 / 1e6),
			wantBytes: 1024000,
			wantDurMs: 1500,
		},
		{
			name: "trap-less unfinished (status failed)",
			replies: []map[string]string{
				{"status": "downloading", "downloaded": "512", "duration": "1s"},
				{"status": "failed", "downloaded": "512", "duration": "2s"},
			},
			elapsed:     3 * time.Second,
			wantErr:     true,
			errContains: "did not finish",
		},
		{
			name: "zero-duration guard when elapsed is zero too",
			replies: []map[string]string{
				{"status": "finished", "downloaded": "12500", "duration": "0s"},
			},
			wantErr:     true,
			errContains: "zero duration",
		},
		{
			name: "zero bytes downloaded",
			replies: []map[string]string{
				{"status": "finished", "downloaded": "0", "duration": "2s"},
			},
			elapsed:     2 * time.Second,
			wantErr:     true,
			errContains: "zero bytes",
		},
		{
			name: "last finished sentence wins over trailing progress",
			replies: []map[string]string{
				{"status": "finished", "downloaded": "2000", "duration": "2s"},
				{"status": "downloading", "downloaded": "100", "duration": "1s"},
			},
			elapsed: 2 * time.Second,
			// 2000 KiB = 2048000 bytes over 2s → 8.192 Mbps
			wantMbps:  f(8.192),
			wantBytes: 2048000,
			wantDurMs: 2000,
		},
		{
			name:        "no replies at all",
			replies:     nil,
			elapsed:     time.Second,
			wantErr:     true,
			errContains: "no progress",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := summarizeFetch(tc.replies, tc.elapsed)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("summarizeFetch() err = nil, want error containing %q", tc.errContains)
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				if got.Mbps != nil {
					t.Errorf("Mbps = %v, want nil on error", *got.Mbps)
				}
				return
			}
			if err != nil {
				t.Fatalf("summarizeFetch() err = %v", err)
			}
			if !floatPtrEq(got.Mbps, tc.wantMbps, 1e-9) {
				t.Errorf("Mbps = %v, want %v", fmtPtr(got.Mbps), fmtPtr(tc.wantMbps))
			}
			if got.Bytes != tc.wantBytes {
				t.Errorf("Bytes = %d, want %d", got.Bytes, tc.wantBytes)
			}
			if got.DurationMs != tc.wantDurMs {
				t.Errorf("DurationMs = %d, want %d", got.DurationMs, tc.wantDurMs)
			}
		})
	}
}

// Keep the float tolerance helpers honest for larger magnitudes too.
func TestSummarizeFetchMbpsPrecision(t *testing.T) {
	res, err := summarizeFetch([]map[string]string{
		{"status": "finished", "downloaded": "1048576", "duration": "10s"}, // 1 GiB in 10s
	}, 11*time.Second)
	if err != nil {
		t.Fatalf("summarizeFetch: %v", err)
	}
	want := 1073741824.0 * 8 / 10 / 1e6 // ≈ 858.99 Mbps
	if res.Mbps == nil || math.Abs(*res.Mbps-want) > 1e-6 {
		t.Errorf("Mbps = %v, want %v", fmtPtr(res.Mbps), want)
	}
}
