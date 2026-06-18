// Package telemetry wires the NMS to an OpenTelemetry (OTLP) backend so the data
// the app already collects is exported as metrics (→ dashboards), traces (→ Tempo)
// and logs (→ Loki). It targets a single OTLP endpoint — typically an OpenTelemetry
// Collector gateway that fans the signals out to the individual backends.
//
// Configuration comes from env defaults (MIKROTIK_NMS_OTEL_*) overlaid with the
// app_settings written by the Settings → Observability card. Export is initialised
// once at startup; changing the settings takes effect on the next backend restart
// (consistent with the env-driven polling intervals).
package telemetry

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellogglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Config is the resolved OTLP export configuration.
type Config struct {
	Enabled     bool
	Endpoint    string            // host:port, no scheme (e.g. "obs.lan:4317")
	Protocol    string            // "grpc" (default, :4317) or "http" (:4318)
	Insecure    bool              // plaintext OTLP (true for a no-TLS collector)
	Headers     map[string]string // optional per-export headers (auth/tenant)
	ServiceName string
	SampleRatio float64       // trace head-sampling ratio (1.0 = all)
	ExportInterval time.Duration // metric push interval
}

func (c Config) isHTTP() bool { return strings.EqualFold(c.Protocol, "http") }

// Provider holds the SDK providers so they can be flushed and shut down cleanly.
type Provider struct {
	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider
	lp *sdklog.LoggerProvider
}

// ParseHeaders parses a "k=v,k2=v2" string into a header map (empty → nil).
func ParseHeaders(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			continue
		}
		if k := strings.TrimSpace(kv[0]); k != "" {
			out[k] = strings.TrimSpace(kv[1])
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ConfigFromSettings overlays the persisted app_settings (Settings → Observability)
// on top of the env-derived defaults. A missing/blank setting keeps the env default.
func ConfigFromSettings(db *sql.DB, env Config) Config {
	get := func(key, def string) string {
		var v string
		if err := db.QueryRow("SELECT value FROM app_settings WHERE key = ?", key).Scan(&v); err == nil {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
		return def
	}
	getBool := func(key string, def bool) bool {
		var v string
		if err := db.QueryRow("SELECT value FROM app_settings WHERE key = ?", key).Scan(&v); err == nil {
			if v = strings.TrimSpace(v); v != "" {
				return v == "true" || v == "1"
			}
		}
		return def
	}
	out := env
	out.Enabled = getBool("otel_enabled", env.Enabled)
	out.Endpoint = get("otel_endpoint", env.Endpoint)
	out.Protocol = get("otel_protocol", env.Protocol)
	out.Insecure = getBool("otel_insecure", env.Insecure)
	out.ServiceName = get("otel_service_name", env.ServiceName)
	if h := get("otel_headers", ""); h != "" {
		out.Headers = ParseHeaders(h)
	}
	return out
}

func buildResource(cfg Config) *resource.Resource {
	host, _ := os.Hostname()
	name := cfg.ServiceName
	if name == "" {
		name = "mikrotik-nms"
	}
	// Schemaless so we don't pin a semconv version. Loki promotes host.name and
	// source to labels (per the lab-observability collector config).
	return resource.NewSchemaless(
		attribute.String("service.name", name),
		attribute.String("service.version", "1"),
		attribute.String("host.name", host),
		attribute.String("source", "mikrotik-nms"),
	)
}

func newTraceExporter(ctx context.Context, cfg Config) (*otlptrace.Exporter, error) {
	if cfg.isHTTP() {
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		return otlptracehttp.New(ctx, opts...)
	}
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
	}
	return otlptracegrpc.New(ctx, opts...)
}

func newMetricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	if cfg.isHTTP() {
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
		}
		return otlpmetrichttp.New(ctx, opts...)
	}
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
	}
	return otlpmetricgrpc.New(ctx, opts...)
}

func newLogExporter(ctx context.Context, cfg Config) (sdklog.Exporter, error) {
	if cfg.isHTTP() {
		opts := []otlploghttp.Option{otlploghttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlploghttp.WithInsecure())
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlploghttp.WithHeaders(cfg.Headers))
		}
		return otlploghttp.New(ctx, opts...)
	}
	opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploggrpc.WithHeaders(cfg.Headers))
	}
	return otlploggrpc.New(ctx, opts...)
}

// Init builds the OTLP exporters, installs global trace/metric/log providers, and
// registers the domain metric instruments backed by db. Call NewLogWriter afterwards
// to bridge the stdlib logger to Loki. Returns a Provider for graceful shutdown.
func Init(ctx context.Context, cfg Config, db *sql.DB) (*Provider, error) {
	// Keep OTel's own internal errors out of the stdlib logger so they never feed
	// back through the Loki log bridge.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		fmt.Fprintln(os.Stderr, "otel:", err)
	}))

	res := buildResource(cfg)
	ratio := cfg.SampleRatio
	if ratio <= 0 {
		ratio = 1.0
	}
	interval := cfg.ExportInterval
	if interval <= 0 {
		interval = time.Minute
	}

	// Traces → Tempo
	traceExp, err := newTraceExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	// Metrics → Prometheus/dashboards
	metricExp, err := newMetricExporter(ctx, cfg)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, fmt.Errorf("metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(interval))),
	)
	otel.SetMeterProvider(mp)
	if err := registerMetrics(mp.Meter("mikrotik-nms"), db); err != nil {
		fmt.Fprintln(os.Stderr, "otel: register metrics:", err)
	}

	// Logs → Loki
	logExp, err := newLogExporter(ctx, cfg)
	if err != nil {
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
		return nil, fmt.Errorf("log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)
	otellogglobal.SetLoggerProvider(lp)

	return &Provider{tp: tp, mp: mp, lp: lp}, nil
}

// Shutdown flushes and stops all providers, bounded by ctx.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	var firstErr error
	if p.tp != nil {
		if err := p.tp.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if p.mp != nil {
		if err := p.mp.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if p.lp != nil {
		if err := p.lp.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// TestConnection verifies an OTLP endpoint by exporting a single throwaway span and
// flushing it within a short timeout. Used by the Settings "Test connection" button
// so the admin can confirm reachability before saving. Does NOT touch global state.
func TestConnection(ctx context.Context, cfg Config) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	exp, err := newTraceExporter(ctx, cfg)
	if err != nil {
		return "", err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(buildResource(cfg)))
	_, span := tp.Tracer("mikrotik-nms").Start(ctx, "otel.connection-test")
	span.End()
	if err := tp.ForceFlush(ctx); err != nil {
		_ = tp.Shutdown(context.Background())
		return "", err
	}
	_ = tp.Shutdown(context.Background())

	proto := "gRPC"
	if cfg.isHTTP() {
		proto = "HTTP"
	}
	return fmt.Sprintf("Connected — test span exported to %s (OTLP/%s)", cfg.Endpoint, proto), nil
}
