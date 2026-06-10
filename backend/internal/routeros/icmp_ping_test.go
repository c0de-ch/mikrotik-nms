package routeros

import (
	"math"
	"testing"
)

func TestParseROSDuration(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		wantMs float64
		wantOK bool
	}{
		{"ms and us", "11ms559us", 11.559, true},
		{"us only", "559us", 0.559, true},
		{"s and ms", "1s234ms", 1234, true},
		{"ros6 ms only", "11ms", 11, true},
		{"seconds only", "2s", 2000, true},
		{"minutes and seconds", "1m2s", 62000, true},
		{"hours", "1h", 3600000, true},
		{"zero", "0ms", 0, true},
		{"surrounding space", " 5ms ", 5, true},
		{"empty", "", 0, false},
		{"missing key (empty map value)", "", 0, false},
		{"garbage", "fast", 0, false},
		{"number without unit", "11", 0, false},
		{"unknown unit", "11ns", 0, false},
		{"trailing garbage", "11ms!", 0, false},
		{"unit without number", "ms", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseROSDuration(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("parseROSDuration(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			}
			if ok && math.Abs(got-tc.wantMs) > 1e-9 {
				t.Errorf("parseROSDuration(%q) = %v ms, want %v ms", tc.in, got, tc.wantMs)
			}
		})
	}
}

func floatPtrEq(got *float64, want *float64, tol float64) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return math.Abs(*got-*want) <= tol
}

func f(v float64) *float64 { return &v }

func TestSummarizePing(t *testing.T) {
	tests := []struct {
		name    string
		replies []map[string]string
		count   int
		want    PingResult
	}{
		{
			name: "all success with cumulative fields",
			replies: []map[string]string{
				{"seq": "0", "time": "10ms", "sent": "1", "received": "1", "packet-loss": "0", "min-rtt": "10ms", "avg-rtt": "10ms", "max-rtt": "10ms"},
				{"seq": "1", "time": "12ms", "sent": "2", "received": "2", "packet-loss": "0", "min-rtt": "10ms", "avg-rtt": "11ms", "max-rtt": "12ms"},
				{"seq": "2", "time": "14ms", "sent": "3", "received": "3", "packet-loss": "0", "min-rtt": "10ms", "avg-rtt": "12ms", "max-rtt": "14ms"},
			},
			count: 3,
			want: PingResult{
				Sent: 3, Received: 3, LossPct: 0,
				RTTMinMs: f(10), RTTAvgMs: f(12), RTTMaxMs: f(14),
				JitterMs: f(2), // |12-10| and |14-12| → mean 2
			},
		},
		{
			name: "mixed timeouts with cumulative fields",
			replies: []map[string]string{
				{"seq": "0", "time": "10ms", "sent": "1", "received": "1", "packet-loss": "0", "min-rtt": "10ms", "avg-rtt": "10ms", "max-rtt": "10ms"},
				{"seq": "1", "status": "timeout", "sent": "2", "received": "1", "packet-loss": "50"},
				{"seq": "2", "time": "20ms", "sent": "3", "received": "2", "packet-loss": "33", "min-rtt": "10ms", "avg-rtt": "15ms", "max-rtt": "20ms"},
			},
			count: 3,
			want: PingResult{
				Sent: 3, Received: 2, LossPct: 33,
				RTTMinMs: f(10), RTTAvgMs: f(15), RTTMaxMs: f(20),
				JitterMs: f(10), // only consecutive successes 10→20
			},
		},
		{
			name: "all timeouts",
			replies: []map[string]string{
				{"seq": "0", "status": "timeout", "sent": "1", "received": "0", "packet-loss": "100"},
				{"seq": "1", "status": "timeout", "sent": "2", "received": "0", "packet-loss": "100"},
				{"seq": "2", "status": "timeout", "sent": "3", "received": "0", "packet-loss": "100"},
			},
			count: 3,
			want:  PingResult{Sent: 3, Received: 0, LossPct: 100},
		},
		{
			name: "missing cumulative fields falls back to row counting",
			replies: []map[string]string{
				{"seq": "0", "time": "5ms"},
				{"seq": "1", "status": "timeout"},
				{"seq": "2", "time": "7ms"},
			},
			count: 3,
			want: PingResult{
				Sent: 3, Received: 2, LossPct: 100.0 / 3.0,
				RTTMinMs: f(5), RTTAvgMs: f(6), RTTMaxMs: f(7),
				JitterMs: f(2),
			},
		},
		{
			name: "ros6 ms-only times without cumulative fields",
			replies: []map[string]string{
				{"seq": "0", "time": "11ms"},
				{"seq": "1", "time": "13ms"},
			},
			count: 2,
			want: PingResult{
				Sent: 2, Received: 2, LossPct: 0,
				RTTMinMs: f(11), RTTAvgMs: f(12), RTTMaxMs: f(13),
				JitterMs: f(2),
			},
		},
		{
			name: "single success has nil jitter",
			replies: []map[string]string{
				{"seq": "0", "time": "8ms123us", "sent": "1", "received": "1", "packet-loss": "0", "min-rtt": "8ms123us", "avg-rtt": "8ms123us", "max-rtt": "8ms123us"},
			},
			count: 1,
			want: PingResult{
				Sent: 1, Received: 1, LossPct: 0,
				RTTMinMs: f(8.123), RTTAvgMs: f(8.123), RTTMaxMs: f(8.123),
			},
		},
		{
			name:    "no replies at all",
			replies: nil,
			count:   5,
			want:    PingResult{Sent: 5, Received: 0, LossPct: 100},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizePing(tc.replies, tc.count)
			if got.Sent != tc.want.Sent {
				t.Errorf("Sent = %d, want %d", got.Sent, tc.want.Sent)
			}
			if got.Received != tc.want.Received {
				t.Errorf("Received = %d, want %d", got.Received, tc.want.Received)
			}
			if math.Abs(got.LossPct-tc.want.LossPct) > 1e-6 {
				t.Errorf("LossPct = %v, want %v", got.LossPct, tc.want.LossPct)
			}
			if !floatPtrEq(got.RTTMinMs, tc.want.RTTMinMs, 1e-9) {
				t.Errorf("RTTMinMs = %v, want %v", fmtPtr(got.RTTMinMs), fmtPtr(tc.want.RTTMinMs))
			}
			if !floatPtrEq(got.RTTAvgMs, tc.want.RTTAvgMs, 1e-9) {
				t.Errorf("RTTAvgMs = %v, want %v", fmtPtr(got.RTTAvgMs), fmtPtr(tc.want.RTTAvgMs))
			}
			if !floatPtrEq(got.RTTMaxMs, tc.want.RTTMaxMs, 1e-9) {
				t.Errorf("RTTMaxMs = %v, want %v", fmtPtr(got.RTTMaxMs), fmtPtr(tc.want.RTTMaxMs))
			}
			if !floatPtrEq(got.JitterMs, tc.want.JitterMs, 1e-9) {
				t.Errorf("JitterMs = %v, want %v", fmtPtr(got.JitterMs), fmtPtr(tc.want.JitterMs))
			}
		})
	}
}

func fmtPtr(p *float64) interface{} {
	if p == nil {
		return nil
	}
	return *p
}
