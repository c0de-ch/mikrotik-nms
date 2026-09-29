package telemetry

import "sync/atomic"

// FlowExporterMetrics is one flow exporter's cumulative counters.
type FlowExporterMetrics struct {
	Name, Address                                                  string
	Datagrams, Flows, Bytes, DecodeErrors, TemplateMisses, SeqLost int64
	LastSeenAgeSeconds                                             float64 // -1 = never
}

// FlowMetrics is the flow collector's snapshot exported as OTel gauges.
type FlowMetrics struct {
	Dropped   map[string]int64 // reason: queue kernel rate_limited writer unknown overflow late dup self_export rejected
	Exporters []FlowExporterMetrics
}

// flowMetricsSource is the registered snapshot provider (nil = none).
var flowMetricsSource atomic.Pointer[func() FlowMetrics]

// SetFlowMetricsSource registers the function the metrics callback reads the
// flow collector's counters from on every export cycle (atomic; nil clears).
// main adapts the collector's Status to it, so the flow package never
// imports telemetry.
func SetFlowMetricsSource(f func() FlowMetrics) {
	if f == nil {
		flowMetricsSource.Store(nil)
		return
	}
	flowMetricsSource.Store(&f)
}

// currentFlowMetrics returns the registered snapshot, ok=false when no source
// is set.
func currentFlowMetrics() (FlowMetrics, bool) {
	f := flowMetricsSource.Load()
	if f == nil || *f == nil {
		return FlowMetrics{}, false
	}
	return (*f)(), true
}
