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
	"strconv"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
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
	switch {
	case LogOTLP() && otlpConfigured():
		if lp, bridge, err := initLogs(service, lvl); err != nil {
			slog.Warn("otlp log bridge init failed; logs stay stdout-only", "err", err)
		} else {
			handler = newMultiHandler(handler, bridge) // stdout AND OTLP
			logShutdown = lp.Shutdown
		}
	case LogOTLP():
		// The flag is set but no OTLP endpoint is configured, so the bridge can't be built — surface it
		// rather than silently ship no logs (this is exactly how the account-service endpoint gap hid).
		slog.Warn("TELOS_OTEL_LOGS is set but no OTLP endpoint is configured; log bridge disabled (logs stay stdout-only)")
	}
	slog.SetDefault(slog.New(handler).With("service", service))

	if DebugEnabled() {
		slog.Debug("debug logging enabled (DEBUG env set)")
	}
	if LogOTLP() && otlpConfigured() {
		slog.Info("otel logs bridging via OTLP")
	}

	metricsShutdown := initMetrics(service)
	tracingShutdown := initTracing(service)
	return func(ctx context.Context) error {
		return errors.Join(metricsShutdown(ctx), tracingShutdown(ctx), logShutdown(ctx))
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

// alwaysSampleKey marks a root span that must be recorded at 100% regardless of the head-sampling ratio —
// the handoff / AdoptZone carve-out (#465). It is passed as a span-START attribute (trace.WithAttributes at
// Start), which OTel surfaces to the sampler's ShouldSample BEFORE the sampling decision, so a span that
// carries it is kept even under an aggressive ratio.
const alwaysSampleKey = attribute.Key("telos.trace.always_sample")

// AlwaysSample returns the span-start attribute that forces a span to be sampled at 100%, bypassing the head
// ratio sampler. Use it ONLY for rare, bounded, high-value traces — the cross-shard handoff and the AdoptZone
// lease (#465) — never on a hot path: it defeats the volume protection the ratio sampler exists to provide.
//
//	ctx, span := tracer.Start(ctx, "handoff", trace.WithNewRoot(),
//	    trace.WithLinks(trace.Link{SpanContext: cause}), trace.WithAttributes(obs.AlwaysSample()))
func AlwaysSample() attribute.KeyValue { return alwaysSampleKey.Bool(true) }

// initTracing installs the global W3C propagator (always) and — gated on the SAME OTLP env as metrics — a
// TracerProvider exporting spans over OTLP/gRPC. Returns its Shutdown so buffered spans flush on exit; a
// no-op when no endpoint is configured.
//
// # Zero cost when unconfigured
//
// With no OTLP endpoint the global no-op TracerProvider is left in place, so otel.Tracer(...).Start returns a
// non-recording span: no span record is allocated and nothing is exported. An unconfigured process pays only
// the one-time global propagator assignment, exactly as an unconfigured metrics process pays nothing.
//
// # Sampler policy
//
// Head sampling, decided at the trace ROOT and inherited by children (sdktrace.ParentBased): a child created
// from an incoming sampled traceparent is always kept, so a sampled trace is whole rather than a scatter of
// fragments. The root decision is TraceIDRatioBased — deterministic on the trace id, so every service makes
// the same keep/drop choice for a given trace with no coordination. The ratio defaults to 1.0 and is
// overridden by the standard OTEL_TRACES_SAMPLER_ARG.
//
// The one carve-out: a span started with obs.AlwaysSample() is recorded at 100% no matter the ratio (see
// carveOutSampler). That exists for the cross-shard handoff + AdoptZone lease (#465) — the single
// highest-value, and rarest, trace in the architecture; sampling it away to respect a hot-path budget it
// never contributes to would be exactly backwards.
//
// # No per-command tracing (#471, decision B)
//
// There is deliberately no per-command trace root. Play is ONE bidi gRPC stream for the whole session
// (internal/world/server.go), so gRPC metadata carries a traceparent once per session, not once per command;
// rooting a span at each keystroke would need a field on the hottest message in the protocol to enable traces
// the ratio sampler mostly discards. The load-bearing traces — handoff (#465), session attach (#466), bus
// delivery (#467) — are all session- or infrastructure-scoped and need no per-command root. A player's
// `north` that provokes a handoff is still traced, as a handoff, linked from the session — just not rooted at
// the keystroke. Relatedly, no otelgrpc STREAM interceptor is installed on Play: it would produce one
// multi-hour span per player, emitted only at logout. Any interceptor added later must exclude Play (#464).
func initTracing(service string) ShutdownFunc {
	// Global W3C propagation, set unconditionally: an incoming traceparent must be understood even by a hop
	// that does not itself export, so trace context survives an unconfigured link in the chain. Cheap — one
	// global assignment — and with no TracerProvider installed a started span is still non-recording.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	if !otlpTracesConfigured() {
		return noopShutdown
	}

	ctx := context.Background()
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(service)))
	if err != nil {
		res = resource.Default()
	}
	exp, err := otlptracegrpc.New(ctx) // endpoint + insecure flag from the standard OTEL_* env, like metrics
	if err != nil {
		slog.Warn("otlp span exporter init failed; tracing disabled", "err", err)
		return noopShutdown
	}
	return installTracerProvider(res, exp)
}

// installTracerProvider assembles the process TracerProvider from a resource + span exporter, installs it
// globally, and returns its Shutdown. Split out from initTracing so a test can drive the EXACT production
// assembly (BatchSpanProcessor + telosSampler + the returned Shutdown) through an in-memory exporter, rather
// than a hand-rebuilt provider that could drift from what actually runs.
func installTracerProvider(res *resource.Resource, exp sdktrace.SpanExporter) ShutdownFunc {
	sampler := telosSampler()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exp), // BatchSpanProcessor flushes on tp.Shutdown — the flush this returns
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)
	slog.Info("otel traces exporting via OTLP", "sampler", sampler.Description())
	return tp.Shutdown
}

// otlpTracesConfigured reports whether an OTLP endpoint is set for traces, gating tracing on the SAME env as
// metrics (the generic endpoint or the signal-specific one), so tracing is opt-in exactly as metrics are.
func otlpTracesConfigured() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// telosSampler is the process sampler: the handoff / AdoptZone 100% carve-out wrapping a ParentBased ratio
// head sampler.
func telosSampler() sdktrace.Sampler {
	return carveOutSampler{base: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(traceSampleRatio()))}
}

// traceSampleRatio reads the standard OTEL_TRACES_SAMPLER_ARG as the root head-sampling probability, default
// 1.0, clamped to [0,1]. A malformed value degrades to 1.0 with a warning rather than silently sampling
// nothing.
func traceSampleRatio() float64 {
	s := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))
	if s == "" {
		return 1.0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		slog.Warn("OTEL_TRACES_SAMPLER_ARG is not a number; defaulting to 1.0", "value", s, "err", err)
		return 1.0
	}
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

// carveOutSampler forces a span carrying obs.AlwaysSample() to RecordAndSample regardless of the base
// sampler's decision, and delegates every other span to base. It is how the rare, high-value handoff /
// AdoptZone traces (#465) are guaranteed 100% even under an aggressive head ratio.
type carveOutSampler struct{ base sdktrace.Sampler }

func (s carveOutSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	for _, a := range p.Attributes {
		if a.Key == alwaysSampleKey && a.Value.AsBool() {
			// Preserve any inbound tracestate, mirroring what the SDK's own samplers return.
			return sdktrace.SamplingResult{
				Decision:   sdktrace.RecordAndSample,
				Tracestate: trace.SpanContextFromContext(p.ParentContext).TraceState(),
			}
		}
	}
	return s.base.ShouldSample(p)
}

func (s carveOutSampler) Description() string {
	return "TelosCarveOut{always_sample->100%,base=" + s.base.Description() + "}"
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
