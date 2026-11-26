package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// seedDeviceMinutes writes device_minutes rows for a device at the given
// offsets (minutes before f.end): covered for polledS seconds, or a gap with
// the given reason when polledS is 0.
func (f *trafficFixture) seedDeviceMinutes(t *testing.T, dev string, polledS int64, gap string, minutesAgo ...int) {
	t.Helper()
	var rows []queries.DeviceMinute
	for _, m := range minutesAgo {
		rows = append(rows, queries.DeviceMinute{DeviceID: dev, Bucket: f.end.Add(-time.Duration(m) * time.Minute),
			PolledS: polledS, GapReason: gap})
	}
	if err := queries.UpsertDeviceMinutes(f.db, rows); err != nil {
		t.Fatal(err)
	}
}

func minutesRange(from, to int) []int {
	var out []int
	for m := from; m >= to; m-- {
		out = append(out, m)
	}
	return out
}

// A port minute is a gap unless its own device has a covered device-minute
// for it. A device that was paused while the rest of the fleet was read used
// to show 0 bps for those minutes — some other port had a row, so the
// collector "was running".
func TestPortHistoryIsAGapUnlessTheDeviceWasCovered(t *testing.T) {
	f := newTrafficFixture(t)
	f.seedMinuteRows(t, "r", "eth1-wan", 7_500_000, minutesRange(10, 1)...)
	f.seedMinuteRows(t, "s", "ether7", 7_500_000, 10, 9, 8, 7, 3, 2) // idle one minute ago
	f.seedDeviceMinutes(t, "r", 60, "", minutesRange(10, 1)...)
	f.seedDeviceMinutes(t, "s", 60, "", 10, 9, 8, 7, 3, 2, 1)
	f.seedDeviceMinutes(t, "s", 0, queries.GapPaused, 6, 5, 4)

	var h portHistoryResponse
	if code := f.get(t, "/traffic/ports/history", map[string]string{"device": "s", "iface": "ether7", "range": "1h"}, &h); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(h.Points) != 10 {
		t.Fatalf("points = %+v", h.Points)
	}
	for i, p := range h.Points {
		minutesAgo := 10 - i
		switch {
		case minutesAgo >= 4 && minutesAgo <= 6:
			if !p.Gap {
				t.Errorf("%d min ago = %+v, want a gap: the device was paused", minutesAgo, p)
			}
		case minutesAgo == 1:
			if p.Gap || p.RxBps != 0 {
				t.Errorf("1 min ago = %+v, want 0: the device was covered and the port idle", p)
			}
		default:
			if p.Gap || p.RxBps != mbps {
				t.Errorf("%d min ago = %+v, want 1 Mbit/s", minutesAgo, p)
			}
		}
	}
	// 45 MB over the 7 covered minutes; the percentile runs over those too.
	if h.Stats.RxBytes != 45_000_000 || h.Stats.RxAvg != 857_143 || h.Stats.RxP95 != mbps {
		t.Errorf("stats = %+v, want the paused minutes left out of the average", h.Stats)
	}

	// The device that was read throughout has no gap.
	h = portHistoryResponse{}
	f.get(t, "/traffic/ports/history", map[string]string{"device": "r", "iface": "eth1-wan", "range": "1h"}, &h)
	for i, p := range h.Points {
		if p.Gap || p.RxBps != mbps {
			t.Errorf("r point %d = %+v, want 1 Mbit/s", i, p)
		}
	}

	// Rankings and sparklines divide by each device's own covered seconds.
	var top topPortsResponse
	f.get(t, "/traffic/ports/top", map[string]string{"range": "1h"}, &top)
	if len(top.Rows) != 2 {
		t.Fatalf("top rows = %+v", top.Rows)
	}
	for _, row := range top.Rows {
		switch row.DeviceID {
		case "r":
			if row.RxAvg != mbps {
				t.Errorf("top r avg = %d, want 1 Mbit/s", row.RxAvg)
			}
		case "s":
			if row.RxAvg != 857_143 {
				t.Errorf("top s avg = %d, want 857143 (45 MB over 420 covered seconds)", row.RxAvg)
			}
			// 2-minute steps from 10 min ago: [10,9] [8,7] [6,5] [4,3] [2,1].
			sp := row.Sparkline
			if len(sp) != 5 || !sp[2].Gap || sp[3].Gap || sp[3].RxBps != mbps || sp[4].RxBps != mbps/2 {
				t.Errorf("top s sparkline = %+v, want a gap for the fully paused step and rates over covered seconds", sp)
			}
		}
	}

	// The estimated Sankey reads the paused device's ports over its covered
	// seconds as well.
	rates := f.windowRatesFor(t, "1h")
	if rx, _, ok := rates("s", "ether7"); !ok || rx != 857_143 {
		t.Errorf("window rate of s/ether7 = %d (measured %v), want 857143", rx, ok)
	}
	if rx, _, ok := rates("r", "eth1-wan"); !ok || rx != mbps {
		t.Errorf("window rate of r/eth1-wan = %d (measured %v), want 1 Mbit/s", rx, ok)
	}
}

// windowRatesFor resolves a range the way the Sankey handler does and
// returns its rate function.
func (f *trafficFixture) windowRatesFor(t *testing.T, key string) func(dev, iface string) (int64, int64, bool) {
	t.Helper()
	rng, _ := parseTrafficRange(key)
	topo, err := f.s.trafficTopology()
	if err != nil {
		t.Fatal(err)
	}
	rates, _, err := f.s.rangeRates(topo, rng, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rates
}

// History from before device_minutes existed keeps rendering as it did: for
// buckets older than the device's first coverage row the fleet-wide
// inference still decides (any row anywhere = the collector was running).
func TestPortHistoryBeforeTheFirstCoverageRowKeepsTheOldInference(t *testing.T) {
	f := newTrafficFixture(t)
	// The collector was down 15 and 14 minutes ago (no row fleet-wide).
	f.seedMinuteRows(t, "r", "eth1-wan", 7_500_000, 20, 19, 18, 17, 16, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 2, 1)
	f.seedMinuteRows(t, "c", "q1", 7_500_000, 20, 3)
	// Coverage rows exist for r from 5 minutes ago; it was unreachable 3
	// minutes ago, while c still had traffic.
	f.seedDeviceMinutes(t, "r", 60, "", 5, 4, 2, 1)
	f.seedDeviceMinutes(t, "r", 0, queries.GapUnreachable, 3)

	var h portHistoryResponse
	f.get(t, "/traffic/ports/history", map[string]string{"device": "r", "iface": "eth1-wan", "range": "1h"}, &h)
	if len(h.Points) != 20 || h.CoverageFrom == nil || !h.CoverageFrom.Equal(f.end.Add(-20*time.Minute)) {
		t.Fatalf("points = %d, coverage_from %v; want 20 points from the old data on", len(h.Points), h.CoverageFrom)
	}
	for i, p := range h.Points {
		minutesAgo := 20 - i
		wantGap := minutesAgo == 15 || minutesAgo == 14 || minutesAgo == 3
		if p.Gap != wantGap || (!wantGap && p.RxBps != mbps) {
			t.Errorf("%d min ago = %+v, want gap=%v", minutesAgo, p, wantGap)
		}
	}

	// A device without any coverage row is judged the old way throughout:
	// idle where the fleet had rows, a gap where it had none.
	h = portHistoryResponse{}
	f.get(t, "/traffic/ports/history", map[string]string{"device": "c", "iface": "q1", "range": "1h"}, &h)
	if len(h.Points) != 20 {
		t.Fatalf("c points = %+v", h.Points)
	}
	for i, p := range h.Points {
		minutesAgo := 20 - i
		switch minutesAgo {
		case 15, 14:
			if !p.Gap {
				t.Errorf("c %d min ago = %+v, want a gap", minutesAgo, p)
			}
		case 20, 3:
			if p.Gap || p.RxBps != mbps {
				t.Errorf("c %d min ago = %+v, want 1 Mbit/s", minutesAgo, p)
			}
		default:
			if p.Gap || p.RxBps != 0 {
				t.Errorf("c %d min ago = %+v, want an idle zero", minutesAgo, p)
			}
		}
	}
}

// Hourly data follows the same rule through device_hours.covered_s: an hour
// counts for the seconds the device was covered, and an hour without a row
// is a gap once the device has hourly telemetry at all.
func TestHourlyHistoryUsesTheDevicesCoveredSeconds(t *testing.T) {
	f := newTrafficFixture(t)
	hour0 := f.end.Truncate(time.Hour)
	h3, h2, h1 := hour0.Add(-3*time.Hour), hour0.Add(-2*time.Hour), hour0.Add(-time.Hour)

	var ports []queries.PortStatRow
	var mins []queries.DeviceMinute
	port := func(dev, iface string, ts time.Time) {
		ports = append(ports, queries.PortStatRow{DeviceID: dev, InterfaceName: iface, Bucket: ts,
			RxBpsAvg: mbps, RxBpsMax: mbps, RxBytes: 7_500_000, Samples: 4})
	}
	for m := 0; m < 60; m++ { // fully covered
		port("r", "eth1-wan", h3.Add(time.Duration(m)*time.Minute))
		mins = append(mins, queries.DeviceMinute{DeviceID: "r", Bucket: h3.Add(time.Duration(m) * time.Minute), PolledS: 60})
	}
	for m := 0; m < 60; m++ { // covered for the first half only
		ts := h2.Add(time.Duration(m) * time.Minute)
		if m < 30 {
			port("r", "eth1-wan", ts)
			mins = append(mins, queries.DeviceMinute{DeviceID: "r", Bucket: ts, PolledS: 60})
		} else {
			mins = append(mins, queries.DeviceMinute{DeviceID: "r", Bucket: ts, GapReason: queries.GapUnreachable})
		}
	}
	// The last hour: nothing recorded for r, while another device had traffic.
	port("c", "q1", h1.Add(10*time.Minute))
	if err := queries.InsertPortStats1m(f.db, ports); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertDeviceMinutes(f.db, mins); err != nil {
		t.Fatal(err)
	}
	if err := queries.RollupPortStats1h(f.db, h3, hour0); err != nil {
		t.Fatal(err)
	}
	if err := queries.RollupDeviceHours(f.db, h3, hour0); err != nil {
		t.Fatal(err)
	}
	f.fake.rolled = hour0

	var h portHistoryResponse
	f.get(t, "/traffic/ports/history", map[string]string{"device": "r", "iface": "eth1-wan", "range": "7d"}, &h)
	if len(h.Points) != 3 {
		t.Fatalf("7d points = %+v", h.Points)
	}
	if p := h.Points[0]; p.Gap || p.RxBps != mbps {
		t.Errorf("fully covered hour = %+v, want 1 Mbit/s", p)
	}
	if p := h.Points[1]; p.Gap || p.RxBps != mbps {
		t.Errorf("half covered hour = %+v, want 1 Mbit/s over its 1800 covered seconds, not half that", p)
	}
	if p := h.Points[2]; !p.Gap {
		t.Errorf("hour without device coverage = %+v, want a gap", p)
	}
	if h.Stats.RxAvg != mbps {
		t.Errorf("7d average = %d, want 1 Mbit/s over the covered seconds", h.Stats.RxAvg)
	}
}
