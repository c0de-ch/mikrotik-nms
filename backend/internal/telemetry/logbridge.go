package telemetry

import (
	"context"
	"io"
	"strings"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	otellogglobal "go.opentelemetry.io/otel/log/global"
)

// logWriter tees the application's stdlib logger to OTLP (→ Loki) while still
// writing to the original sink (stderr). Installing it with log.SetOutput keeps
// every existing log.Printf/Println call working unchanged.
type logWriter struct {
	logger   otellog.Logger
	fallback io.Writer
}

// NewLogWriter returns an io.Writer for log.SetOutput that mirrors each log line to
// the global OTel logger (set up by Init) and to fallback (the original stderr).
func NewLogWriter(fallback io.Writer) io.Writer {
	return &logWriter{
		logger:   otellogglobal.GetLoggerProvider().Logger("mikrotik-nms"),
		fallback: fallback,
	}
}

func (w *logWriter) Write(p []byte) (int, error) {
	// Always keep the local sink working first.
	n, err := w.fallback.Write(p)

	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		var rec otellog.Record
		rec.SetTimestamp(time.Now())
		rec.SetBody(otellog.StringValue(msg))
		sev, sevText := severityOf(msg)
		rec.SetSeverity(sev)
		rec.SetSeverityText(sevText)
		w.logger.Emit(context.Background(), rec)
	}
	return n, err
}

// severityOf classifies a log line from its leading keyword so Loki/Grafana can
// filter by level. The app logs unstructured lines, so this is best-effort.
func severityOf(msg string) (otellog.Severity, string) {
	l := strings.ToLower(msg)
	switch {
	case strings.Contains(l, "error") || strings.Contains(l, "failed"):
		return otellog.SeverityError, "ERROR"
	case strings.Contains(l, "warning") || strings.Contains(l, "warn"):
		return otellog.SeverityWarn, "WARN"
	default:
		return otellog.SeverityInfo, "INFO"
	}
}
