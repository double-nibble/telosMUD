// Package obs configures process-wide observability for TelosMUD services.
//
// Every service calls Init exactly once at startup; from then on all code logs
// through the slog default logger (so packages never need a logger passed in).
// Phase 1 wires structured JSON logging and leaves a seam for OpenTelemetry
// tracing/metrics to attach later without touching call sites.
//
// # Debug logging
//
// Components emit verbose, step-by-step tracing via slog.Debug — connection
// accepted, command dispatched, player moved, frame sent, and so on. That output
// is OFF by default. Set the DEBUG environment variable to a truthy value
// (1/true/yes/on) to turn it on:
//
//	DEBUG=1 ./bin/telos-world      # watch the world narrate every step
//	DEBUG=1 make up                # (passes through to the containers)
//
// DEBUG lowers the effective log level to Debug, overriding the configured
// log_level. With DEBUG unset, Debug records are filtered out by the handler, so
// the tracing costs effectively nothing in normal operation.
package obs

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// ShutdownFunc flushes any observability exporters on process exit.
type ShutdownFunc func(context.Context) error

// Init installs the default slog logger (JSON to stdout, tagged with the service
// name) and returns a shutdown hook. If DEBUG is truthy the level is forced to
// Debug regardless of the configured level.
func Init(service, level string) ShutdownFunc {
	lvl := parseLevel(level)
	if DebugEnabled() {
		lvl = slog.LevelDebug
	}
	// stdout JSON stays the primary sink everywhere (k8s ships it to Loki via the collector's filelog
	// receiver). When TELOS_OTEL_LOGS is set (the docker-compose observability overlay does — #459),
	// ALSO bridge slog records onto the existing OTLP connection so logs reach Loki without file
	// scraping. That is the Docker-Desktop answer: filelog cannot read the VM's container logs, but the
	// OTLP path already works (it carries metrics), so logs ride it too. It is OFF by default so k8s,
	// which already collects logs via filelog, does not double-ship.
	var handler slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	logShutdown := noopShutdown
	if LogOTLP() && otlpConfigured() {
		if lp, bridge, err := initLogs(service, lvl); err != nil {
			slog.Warn("otlp log bridge init failed; logs stay stdout-only", "err", err)
		} else {
			handler = newMultiHandler(handler, bridge) // stdout AND OTLP
			logShutdown = lp.Shutdown
		}
	}
	slog.SetDefault(slog.New(handler).With("service", service))

	if DebugEnabled() {
		slog.Debug("debug logging enabled (DEBUG env set)")
	}
	if LogOTLP() && otlpConfigured() {
		slog.Info("otel logs bridging via OTLP")
	}

	metricsShutdown := initMetrics(service)
	return func(ctx context.Context) error {
		return errors.Join(metricsShutdown(ctx), logShutdown(ctx))
	}
}

func noopShutdown(context.Context) error { return nil }

// otlpConfigured reports whether an OTLP endpoint is set via the standard env, so an exporter has
// somewhere to send. Shared by the metric and log setup.
func otlpConfigured() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}

// initLogs builds an OTLP LoggerProvider (endpoint from the standard OTEL_* env) and an slog bridge
// handler over it, filtered to the same level as stdout. Returns the provider (for Shutdown) and the
// handler. Callers multiplex it alongside the stdout handler so logs go to both places.
func initLogs(service string, lvl slog.Level) (*sdklog.LoggerProvider, slog.Handler, error) {
	ctx := context.Background()
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(service)))
	if err != nil {
		res = resource.Default()
	}
	exp, err := otlploggrpc.New(ctx) // endpoint + insecure flag come from the standard OTEL_* env
	if err != nil {
		return nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
	)
	bridge := otelslog.NewHandler(service, otelslog.WithLoggerProvider(lp))
	return lp, &levelHandler{level: lvl, next: bridge}, nil
}

// initMetrics installs the global OpenTelemetry MeterProvider (Phase 16.1). It exports over OTLP/gRPC when
// OTEL_EXPORTER_OTLP_ENDPOINT (or the metrics-specific variant) is set; otherwise the provider has no reader,
// so instrument records are negligible no-ops (metrics off). Returns the provider's Shutdown so a periodic
// reader flushes on exit.
func initMetrics(service string) ShutdownFunc {
	ctx := context.Background()
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(service)))
	if err != nil {
		res = resource.Default()
	}
	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "" {
		exp, err := otlpmetricgrpc.New(ctx) // endpoint + insecure flag come from the standard OTEL_* env
		if err != nil {
			slog.Warn("otlp metric exporter init failed; metrics disabled", "err", err)
		} else {
			opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
			slog.Info("otel metrics exporting via OTLP")
		}
	}

	mp := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(mp)
	return mp.Shutdown
}

// DebugEnabled reports whether the DEBUG env flag is truthy. Cheap slog.Debug
// calls can rely on the level filter instead; use this only to guard debug-only
// work that is itself expensive (e.g. serializing a large value just to log it).
func DebugEnabled() bool {
	return truthyEnv("DEBUG")
}

// LogRawInput reports whether the TELOS_LOG_RAW_INPUT env flag is truthy. It gates
// logging of verbatim player input lines (tells, chat, mistyped link codes) — a
// separate, explicit opt-in from DEBUG, deliberately: turning on debug logging must
// never silently start recording player chat into a durable log store (#454). Off by
// default; callers on hot paths cache the result rather than re-reading env per line.
func LogRawInput() bool {
	return truthyEnv("TELOS_LOG_RAW_INPUT")
}

// LogOTLP reports whether the TELOS_OTEL_LOGS env flag is truthy — the opt-in that ALSO bridges slog
// records over OTLP to the collector (#459). Off by default: k8s collects container logs via the
// collector's filelog receiver, so shipping them over OTLP there would double them. The docker-compose
// observability overlay sets it because filelog cannot read Docker Desktop's in-VM container logs.
func LogOTLP() bool {
	return truthyEnv("TELOS_OTEL_LOGS")
}

// truthyEnv reports whether the named env var is set to a truthy value (1/true/yes/on).
func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// multiHandler fans one slog record out to several handlers (here: stdout JSON + the OTLP bridge), so a
// log line is both human-readable on stdout and shipped to Loki. slog has no built-in fan-out.
type multiHandler struct{ hs []slog.Handler }

func newMultiHandler(hs ...slog.Handler) *multiHandler { return &multiHandler{hs: hs} }

func (m *multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.hs {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.hs {
		if h.Enabled(ctx, r.Level) {
			// Clone per handler: a Record must not be shared across handlers that may add attrs.
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *multiHandler) WithAttrs(as []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.hs))
	for i, h := range m.hs {
		next[i] = h.WithAttrs(as)
	}
	return &multiHandler{hs: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.hs))
	for i, h := range m.hs {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{hs: next}
}

// levelHandler wraps a handler with a minimum level, so the OTLP bridge honors the same level filter as
// stdout (the otelslog bridge otherwise emits at every level).
type levelHandler struct {
	level slog.Level
	next  slog.Handler
}

func (h *levelHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }
func (h *levelHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.next.Handle(ctx, r)
}

func (h *levelHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &levelHandler{level: h.level, next: h.next.WithAttrs(as)}
}

func (h *levelHandler) WithGroup(name string) slog.Handler {
	return &levelHandler{level: h.level, next: h.next.WithGroup(name)}
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
