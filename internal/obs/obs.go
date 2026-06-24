// Package obs configures process-wide observability. Phase 0 wires structured
// logging (slog) and leaves a seam for OpenTelemetry tracing/metrics to attach
// later without touching call sites.
package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// ShutdownFunc flushes any observability exporters on process exit.
type ShutdownFunc func(context.Context) error

// Init installs the default slog logger (JSON to stdout, tagged with service)
// and returns a shutdown hook. OTel providers will be created here later and
// their Shutdown returned in place of the no-op.
func Init(service, level string) ShutdownFunc {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(level)})
	slog.SetDefault(slog.New(handler).With("service", service))
	// TODO(phase0): initialize an OTel TracerProvider/MeterProvider here and
	// return its Shutdown so traces/metrics flush on exit.
	return func(context.Context) error { return nil }
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
