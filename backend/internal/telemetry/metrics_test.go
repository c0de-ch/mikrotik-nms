package telemetry

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMain(m *testing.M) {
	queries.AllowPlaintextSecrets(true) // test devices carry no encrypted secret
	os.Exit(m.Run())
}

// cpuPoints collects the mikrotik.device.cpu_load data points by device.
func cpuPoints(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.DataPoint[int64]{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "mikrotik.device.cpu_load" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("cpu_load is %T, want an int64 gauge", m.Data)
			}
			for _, dp := range g.DataPoints {
				dev, _ := dp.Attributes.Value("device")
				out[dev.AsString()] = dp
			}
		}
	}
	return out
}

// The CPU gauge exports a device's sample only while it is younger than
// three minutes, with the time it was taken: an old value re-exported every
// cycle draws a flat line that looks measured ("constant 100 %").
func TestCPUGaugeExportsOnlyFreshSamples(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	stale := 100
	for i, id := range []string{"fresh", "old", "never"} {
		if err := queries.CreateDevice(db, &queries.Device{ID: id, Identity: id, Address: "192.0.2." + string(rune('1'+i)),
			Username: "u", APIPort: 8728, Status: "online"}); err != nil {
			t.Fatal(err)
		}
		// What the device row still holds from an hour ago must not leak out.
		if err := queries.UpdateDeviceLoad(db, id, stale, nil, ""); err != nil {
			t.Fatal(err)
		}
	}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := registerMetrics(mp.Meter("test"), db); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetCPUSource(nil) })

	// No sampler (an instance that does not poll): nothing is exported.
	SetCPUSource(nil)
	if got := cpuPoints(t, reader); len(got) != 0 {
		t.Fatalf("without a sample source the gauge exported %v", got)
	}

	now := time.Now().UTC()
	takenAt := now.Add(-40 * time.Second)
	SetCPUSource(func() map[string]CPUSample {
		return map[string]CPUSample{
			"fresh": {Load: 37, At: takenAt},
			"old":   {Load: 99, At: now.Add(-4 * time.Minute)},
		}
	})
	got := cpuPoints(t, reader)
	if len(got) != 1 {
		t.Fatalf("exported devices = %v, want only the one sampled in the last 3 minutes", got)
	}
	dp, ok := got["fresh"]
	if !ok || dp.Value != 37 {
		t.Fatalf("fresh device = %+v (found %v), want 37", dp, ok)
	}
	if v, ok := dp.Attributes.Value("sample_time"); !ok || v.AsString() != takenAt.Format(time.RFC3339) {
		t.Errorf("sample_time = %q (present %v), want %s", v.AsString(), ok, takenAt.Format(time.RFC3339))
	}
}

func TestFreshCPUSample(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for age, want := range map[time.Duration]bool{
		0: true, 2*time.Minute + 59*time.Second: true, 3 * time.Minute: false, time.Hour: false,
		-30 * time.Second: true, // a little clock skew is not staleness
		-10 * time.Minute: false,
	} {
		if got := freshCPUSample(CPUSample{Load: 1, At: now.Add(-age)}, now); got != want {
			t.Errorf("sample aged %v: fresh = %v, want %v", age, got, want)
		}
	}
	if freshCPUSample(CPUSample{}, now) {
		t.Error("a zero sample time is never fresh")
	}
}
