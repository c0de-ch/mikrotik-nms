package routeros

import (
	"testing"
)

func TestSummarizeTraceroute(t *testing.T) {
	tests := []struct {
		name    string
		replies []map[string]string
		want    []TracerouteHop
	}{
		{
			// Real RouterOS serialization (verified live): "last" carries a
			// duration suffix while avg/best/worst are unitless milliseconds.
			name: "multi-section grouping picks the last pass",
			replies: []map[string]string{
				// First pass: incomplete (only one hop answered yet).
				{".section": "0", "address": "192.168.1.1", "sent": "1", "loss": "0", "status": "", "last": "1ms", "avg": "1", "best": "1", "worst": "1"},
				{".section": "0", "address": "", "sent": "1", "loss": "100", "status": "timeout"},
				// Final pass: complete table.
				{".section": "1", "address": "192.168.1.1", "sent": "2", "loss": "0", "status": "", "last": "1.2ms", "avg": "1.1", "best": "1", "worst": "1.2"},
				{".section": "1", "address": "1.1.1.1", "sent": "2", "loss": "0", "status": "", "last": "20.4ms", "avg": "20.4", "best": "4.2", "worst": "4.7"},
			},
			want: []TracerouteHop{
				{Hop: 1, Address: "192.168.1.1", LossPct: 0, Sent: 2, LastMs: f(1.2), AvgMs: f(1.1), BestMs: f(1), WorstMs: f(1.2)},
				{Hop: 2, Address: "1.1.1.1", LossPct: 0, Sent: 2, LastMs: f(20.4), AvgMs: f(20.4), BestMs: f(4.2), WorstMs: f(4.7)},
			},
		},
		{
			name: "timeout hops yield null ms and status",
			replies: []map[string]string{
				{".section": "0", "address": "192.168.1.1", "sent": "1", "loss": "0", "last": "2ms", "avg": "2", "best": "2", "worst": "2"},
				{".section": "0", "address": "", "sent": "1", "loss": "100", "status": "timeout", "last": "timeout"},
			},
			want: []TracerouteHop{
				{Hop: 1, Address: "192.168.1.1", LossPct: 0, Sent: 1, LastMs: f(2), AvgMs: f(2), BestMs: f(2), WorstMs: f(2)},
				{Hop: 2, Address: "", LossPct: 100, Sent: 1, Status: "timeout"},
			},
		},
		{
			// ms-suffixed avg/best/worst kept as a tolerance case: older code
			// assumed duration notation everywhere, and both forms must parse.
			name: "single-section fallback without .section attribute",
			replies: []map[string]string{
				{"address": "10.0.0.1", "sent": "1", "loss": "0", "last": "3ms", "avg": "3ms", "best": "3ms", "worst": "3ms"},
				{"address": "10.0.0.2", "sent": "1", "loss": "0", "last": "5ms", "avg": "5ms", "best": "5ms", "worst": "5ms"},
			},
			want: []TracerouteHop{
				{Hop: 1, Address: "10.0.0.1", LossPct: 0, Sent: 1, LastMs: f(3), AvgMs: f(3), BestMs: f(3), WorstMs: f(3)},
				{Hop: 2, Address: "10.0.0.2", LossPct: 0, Sent: 1, LastMs: f(5), AvgMs: f(5), BestMs: f(5), WorstMs: f(5)},
			},
		},
		{
			name: "loss percent suffix tolerance",
			replies: []map[string]string{
				{".section": "2", "address": "10.0.0.1", "sent": "3", "loss": "33%", "last": "4ms", "avg": "4", "best": "4", "worst": "4"},
				{".section": "2", "address": "10.0.0.2", "sent": "3", "loss": "100%", "status": "timeout"},
			},
			want: []TracerouteHop{
				{Hop: 1, Address: "10.0.0.1", LossPct: 33, Sent: 3, LastMs: f(4), AvgMs: f(4), BestMs: f(4), WorstMs: f(4)},
				{Hop: 2, Address: "10.0.0.2", LossPct: 100, Sent: 3, Status: "timeout"},
			},
		},
		{
			name:    "no replies at all",
			replies: nil,
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeTraceroute(tc.replies)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d hops, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				g, w := got[i], tc.want[i]
				if g.Hop != w.Hop || g.Address != w.Address || g.Sent != w.Sent || g.Status != w.Status {
					t.Errorf("hop %d = %+v, want %+v", i, g, w)
				}
				if g.LossPct != w.LossPct {
					t.Errorf("hop %d LossPct = %v, want %v", i, g.LossPct, w.LossPct)
				}
				if !floatPtrEq(g.LastMs, w.LastMs, 1e-9) {
					t.Errorf("hop %d LastMs = %v, want %v", i, fmtPtr(g.LastMs), fmtPtr(w.LastMs))
				}
				if !floatPtrEq(g.AvgMs, w.AvgMs, 1e-9) {
					t.Errorf("hop %d AvgMs = %v, want %v", i, fmtPtr(g.AvgMs), fmtPtr(w.AvgMs))
				}
				if !floatPtrEq(g.BestMs, w.BestMs, 1e-9) {
					t.Errorf("hop %d BestMs = %v, want %v", i, fmtPtr(g.BestMs), fmtPtr(w.BestMs))
				}
				if !floatPtrEq(g.WorstMs, w.WorstMs, 1e-9) {
					t.Errorf("hop %d WorstMs = %v, want %v", i, fmtPtr(g.WorstMs), fmtPtr(w.WorstMs))
				}
			}
		})
	}
}
