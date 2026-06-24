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
	"log/slog"
	"os"
	"strings"
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
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler).With("service", service))

	if DebugEnabled() {
		slog.Debug("debug logging enabled (DEBUG env set)")
	}
	// TODO(phase0): initialize an OTel TracerProvider/MeterProvider here and
	// return its Shutdown so traces/metrics flush on exit.
	return func(context.Context) error { return nil }
}

// DebugEnabled reports whether the DEBUG env flag is truthy. Cheap slog.Debug
// calls can rely on the level filter instead; use this only to guard debug-only
// work that is itself expensive (e.g. serializing a large value just to log it).
func DebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEBUG"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
