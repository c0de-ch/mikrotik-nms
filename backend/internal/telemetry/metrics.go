package telemetry

import (
	"context"
	"database/sql"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// registerMetrics wires observable instruments that read the data the NMS already
// collects (devices, connectivity, speed tests, wifi, loop events) straight from
// the DB on each export cycle. Observable gauges keep the export pull-based — no
// extra goroutine — and reflect the latest cached values written by the pollers.
func registerMetrics(meter metric.Meter, db *sql.DB) error {
	devCount, _ := meter.Int64ObservableGauge("mikrotik.devices",
		metric.WithDescription("Managed device count by status (online/offline/unknown)"))
	cpu, _ := meter.Int64ObservableGauge("mikrotik.device.cpu_load",
		metric.WithUnit("%"), metric.WithDescription("Per-device CPU load"))
	memUsed, _ := meter.Int64ObservableGauge("mikrotik.device.memory.used",
		metric.WithUnit("By"), metric.WithDescription("Per-device memory used"))
	memTotal, _ := meter.Int64ObservableGauge("mikrotik.device.memory.total",
		metric.WithUnit("By"), metric.WithDescription("Per-device memory total"))
	loss, _ := meter.Float64ObservableGauge("mikrotik.connectivity.loss",
		metric.WithUnit("%"), metric.WithDescription("Latest ICMP packet loss per ping target"))
	rtt, _ := meter.Float64ObservableGauge("mikrotik.connectivity.rtt_avg",
		metric.WithUnit("ms"), metric.WithDescription("Latest average RTT per ping target"))
	jitter, _ := meter.Float64ObservableGauge("mikrotik.connectivity.jitter",
		metric.WithUnit("ms"), metric.WithDescription("Latest jitter per ping target"))
	speed, _ := meter.Float64ObservableGauge("mikrotik.speedtest.mbps",
		metric.WithUnit("Mbit/s"), metric.WithDescription("Latest router-side download speed per test"))
	wifi, _ := meter.Int64ObservableGauge("mikrotik.wifi.clients",
		metric.WithDescription("Currently associated wireless clients per AP/SSID"))
	loopEvents, _ := meter.Int64ObservableGauge("mikrotik.loop_events.recent",
		metric.WithDescription("Loop/flap/STP events in the last 24h by severity"))

	// Flow collector (SetFlowMetricsSource): cumulative per-exporter counters
	// and collector-wide drops by reason. Observed only while a source is set.
	flowDatagrams, _ := meter.Int64ObservableGauge("mikrotik.flow.datagrams",
		metric.WithDescription("Flow datagrams received per exporter (cumulative)"))
	flowFlows, _ := meter.Int64ObservableGauge("mikrotik.flow.flows",
		metric.WithDescription("Flow records accepted per exporter (cumulative)"))
	flowBytes, _ := meter.Int64ObservableGauge("mikrotik.flow.bytes",
		metric.WithUnit("By"), metric.WithDescription("Estimated bytes of accepted flow records per exporter (cumulative)"))
	flowDecodeErrors, _ := meter.Int64ObservableGauge("mikrotik.flow.decode_errors",
		metric.WithDescription("Flow decode errors per exporter (cumulative)"))
	flowTemplateMisses, _ := meter.Int64ObservableGauge("mikrotik.flow.template_misses",
		metric.WithDescription("Flow data sets received before their template, per exporter (cumulative)"))
	flowSeqLost, _ := meter.Int64ObservableGauge("mikrotik.flow.seq_lost",
		metric.WithDescription("Flow export sequence gaps per exporter (cumulative, informational)"))
	flowLastSeen, _ := meter.Float64ObservableGauge("mikrotik.flow.exporter.last_seen_age",
		metric.WithUnit("s"), metric.WithDescription("Seconds since the last datagram per flow exporter"))
	flowDropped, _ := meter.Int64ObservableGauge("mikrotik.flow.collector.dropped",
		metric.WithDescription("Flow collector drops by reason (cumulative since start)"))

	_, err := meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		// Devices: status counts + per-device cpu/mem
		if devices, err := queries.ListDevices(db); err == nil {
			counts := map[string]int64{"online": 0, "offline": 0, "unknown": 0}
			for _, d := range devices {
				st := d.Status
				if st == "" {
					st = "unknown"
				}
				counts[st]++
				da := metric.WithAttributes(
					attribute.String("device", d.Identity),
					attribute.String("address", d.Address),
				)
				if d.CPULoad != nil {
					o.ObserveInt64(cpu, int64(*d.CPULoad), da)
				}
				if d.MemoryUsed != nil {
					o.ObserveInt64(memUsed, *d.MemoryUsed, da)
				}
				if d.MemoryTotal != nil {
					o.ObserveInt64(memTotal, *d.MemoryTotal, da)
				}
			}
			for st, c := range counts {
				o.ObserveInt64(devCount, c, metric.WithAttributes(attribute.String("status", st)))
			}
		}

		// Connectivity: latest ping sample per target
		if latest, err := queries.GetLatestPingSamples(db); err == nil {
			names := map[string]string{}
			if targets, err := queries.ListPingTargets(db); err == nil {
				for _, t := range targets {
					names[t.ID] = t.Label
				}
			}
			for tid, s := range latest {
				name := names[tid]
				if name == "" {
					name = s.Address
				}
				a := metric.WithAttributes(
					attribute.String("target", name),
					attribute.String("address", s.Address),
				)
				o.ObserveFloat64(loss, s.LossPct, a)
				if s.RTTAvgMs != nil {
					o.ObserveFloat64(rtt, *s.RTTAvgMs, a)
				}
				if s.JitterMs != nil {
					o.ObserveFloat64(jitter, *s.JitterMs, a)
				}
			}
		}

		// Speed tests: latest sample per test
		if latest, err := queries.GetLatestSpeedSamples(db); err == nil {
			for tid, s := range latest {
				if s.Mbps != nil {
					o.ObserveFloat64(speed, *s.Mbps, metric.WithAttributes(
						attribute.String("test", tid),
						attribute.String("device", s.DeviceID),
					))
				}
			}
		}

		// WiFi: associated clients grouped by AP + SSID
		if clients, err := queries.GetWifiClientsCurrentAP(db); err == nil {
			type apKey struct{ ap, ssid string }
			counts := map[apKey]int64{}
			for _, c := range clients {
				counts[apKey{c.APName, c.SSID}]++
			}
			for k, n := range counts {
				o.ObserveInt64(wifi, n, metric.WithAttributes(
					attribute.String("ap", k.ap),
					attribute.String("ssid", k.ssid),
				))
			}
		}

		// Network health: loop/flap/STP events in the last 24h
		if warn, crit, err := queries.CountRecentLoopEvents(db, time.Now().Add(-24*time.Hour)); err == nil {
			o.ObserveInt64(loopEvents, int64(warn), metric.WithAttributes(attribute.String("severity", "warn")))
			o.ObserveInt64(loopEvents, int64(crit), metric.WithAttributes(attribute.String("severity", "critical")))
		}

		// Flow collector: only when main registered a source.
		if fm, ok := currentFlowMetrics(); ok {
			for _, e := range fm.Exporters {
				a := metric.WithAttributes(
					attribute.String("exporter", e.Name),
					attribute.String("address", e.Address),
				)
				o.ObserveInt64(flowDatagrams, e.Datagrams, a)
				o.ObserveInt64(flowFlows, e.Flows, a)
				o.ObserveInt64(flowBytes, e.Bytes, a)
				o.ObserveInt64(flowDecodeErrors, e.DecodeErrors, a)
				o.ObserveInt64(flowTemplateMisses, e.TemplateMisses, a)
				o.ObserveInt64(flowSeqLost, e.SeqLost, a)
				if e.LastSeenAgeSeconds >= 0 {
					o.ObserveFloat64(flowLastSeen, e.LastSeenAgeSeconds, a)
				}
			}
			for reason, n := range fm.Dropped {
				o.ObserveInt64(flowDropped, n, metric.WithAttributes(attribute.String("reason", reason)))
			}
		}
		return nil
	}, devCount, cpu, memUsed, memTotal, loss, rtt, jitter, speed, wifi, loopEvents,
		flowDatagrams, flowFlows, flowBytes, flowDecodeErrors, flowTemplateMisses, flowSeqLost, flowLastSeen, flowDropped)

	return err
}
