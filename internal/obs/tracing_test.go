package obs

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// clearOTLPEnv makes the process look unconfigured for the duration of a test.
func clearOTLPEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
		"OTEL_TRACES_SAMPLER_ARG",
	} {
		t.Setenv(k, "")
	}
}

// resetGlobalTracerProvider restores the global provider to a no-op after a test that installed a real one,
// so provider state does not leak between tests in this package.
func resetGlobalTracerProvider(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
}

// TestTracingZeroCostWhenUnconfigured is the acceptance guard from #464: with no OTLP endpoint, initTracing
// installs NO TracerProvider, so a started span is non-recording and unsampled — no span record, nothing to
// export. This is what "an unconfigured process pays nothing" means, verified rather than assumed.
func TestTracingZeroCostWhenUnconfigured(t *testing.T) {
	clearOTLPEnv(t)
	resetGlobalTracerProvider(t)
	// Ensure we start from a clean no-op provider (a prior test may have set a real one).
	otel.SetTracerProvider(noop.NewTracerProvider())

	shutdown := initTracing("test-svc")
	if shutdown == nil {
		t.Fatal("initTracing returned a nil ShutdownFunc")
	}

	// Probe the provider state BEFORE shutting anything down. A shut-DOWN real provider also yields
	// non-recording spans, so probing after shutdown could not tell "no provider installed" from "provider
	// installed then shut down" — the whole claim under test. So assert the global provider is STILL the
	// no-op type, and that a span from it is non-recording, while tracing is live.
	if _, isNoop := otel.GetTracerProvider().(noop.TracerProvider); !isNoop {
		t.Errorf("unconfigured initTracing installed a real TracerProvider (%T); it must leave the no-op in place",
			otel.GetTracerProvider())
	}
	_, span := otel.Tracer("t").Start(context.Background(), "op")
	if span.IsRecording() {
		t.Error("span is recording with no exporter configured — tracing is not zero-cost when unconfigured")
	}
	if span.SpanContext().IsSampled() {
		t.Error("span is sampled with no exporter configured")
	}
	span.End()

	// The returned no-op shutdown must still be safe to call.
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("unconfigured shutdown returned error: %v", err)
	}
}

// TestPropagatorIsW3C asserts initTracing sets a global composite propagator that carries W3C traceparent
// (and baggage), so trace context can cross a process boundary — the whole point of #464.
func TestPropagatorIsW3C(t *testing.T) {
	clearOTLPEnv(t)
	resetGlobalTracerProvider(t)
	_ = initTracing("test-svc")

	fields := otel.GetTextMapPropagator().Fields()
	var hasTraceparent, hasBaggage bool
	for _, f := range fields {
		switch f {
		case "traceparent":
			hasTraceparent = true
		case "baggage":
			hasBaggage = true
		}
	}
	if !hasTraceparent {
		t.Errorf("global propagator does not carry W3C traceparent; fields=%v", fields)
	}
	if !hasBaggage {
		t.Errorf("global propagator does not carry baggage; fields=%v", fields)
	}
}

// TestTracingInstalledWhenConfigured asserts that with an OTLP endpoint set, initTracing installs a real
// TracerProvider so spans record. otlptracegrpc.New connects lazily, so this needs no live collector.
func TestTracingInstalledWhenConfigured(t *testing.T) {
	clearOTLPEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "localhost:4317")
	resetGlobalTracerProvider(t)

	shutdown := initTracing("test-svc")
	// Shut down with a SHORT deadline: the configured endpoint is a dead collector, and the batch
	// processor's flush would otherwise block on the export retry until its 10s timeout. We only assert the
	// provider is installed (spans record); a failed export to a dead endpoint is irrelevant to that.
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_ = shutdown(sctx)
	})

	_, span := otel.Tracer("t").Start(context.Background(), "op")
	defer span.End()
	if !span.IsRecording() {
		t.Error("span is not recording though an OTLP traces endpoint is configured")
	}
	if !span.SpanContext().IsSampled() {
		t.Error("span is not sampled at the default ratio (1.0) though tracing is configured")
	}
}

// retainingExporter records exported spans and, unlike tracetest.InMemoryExporter, does NOT clear them on
// Shutdown — so a post-Shutdown count actually reflects what the batch processor flushed DURING shutdown.
type retainingExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
}

func (e *retainingExporter) ExportSpans(_ context.Context, ss []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, ss...)
	return nil
}
func (e *retainingExporter) Shutdown(context.Context) error { return nil } // deliberately does NOT reset
func (e *retainingExporter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.spans)
}

// TestShutdownFlushesSpans verifies the flush-on-shutdown wiring #464 requires, exercising the EXACT
// production assembly: installTracerProvider is what initTracing calls once an exporter exists, so this
// covers the real code path (BatchSpanProcessor + telosSampler + the returned Shutdown), not a hand-rebuilt
// provider. The batch processor buffers, so nothing is exported at span.End(); only the returned Shutdown
// drains it. A retaining exporter (which does not clear on its own Shutdown) makes the post-Shutdown count
// meaningful.
func TestShutdownFlushesSpans(t *testing.T) {
	resetGlobalTracerProvider(t)
	exp := &retainingExporter{}
	shutdown := installTracerProvider(resource.Default(), exp)

	_, span := otel.GetTracerProvider().Tracer("t").Start(context.Background(), "op")
	span.End()
	if got := exp.count(); got != 0 {
		t.Fatalf("batch processor exported %d spans before any flush, want 0 (it should buffer)", got)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := exp.count(); got != 1 {
		t.Fatalf("after the returned ShutdownFunc ran, the exporter has %d spans, want 1 — spans did not "+
			"flush on shutdown", got)
	}
}

// TestCarveOutSamplerForces100 is the load-bearing sampler test (#464/#465): a span carrying AlwaysSample()
// is recorded even when the BASE sampler would drop everything, and a span WITHOUT it follows the base.
func TestCarveOutSamplerForces100(t *testing.T) {
	// Base = never sample. The carve-out must still keep an AlwaysSample() span.
	s := carveOutSampler{base: sdktrace.NeverSample()}

	withFlag := s.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		Name:          "handoff",
		Attributes:    []attribute.KeyValue{AlwaysSample()},
	})
	if withFlag.Decision != sdktrace.RecordAndSample {
		t.Errorf("AlwaysSample() span decision = %v, want RecordAndSample even under NeverSample base", withFlag.Decision)
	}

	without := s.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		Name:          "ordinary",
	})
	if without.Decision != sdktrace.Drop {
		t.Errorf("ordinary span under NeverSample base decision = %v, want Drop", without.Decision)
	}

	// A false-valued flag must NOT force sampling (only a true bool is the carve-out).
	falseFlag := s.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		Name:          "op",
		Attributes:    []attribute.KeyValue{alwaysSampleKey.Bool(false)},
	})
	if falseFlag.Decision != sdktrace.Drop {
		t.Errorf("always_sample=false span decision = %v, want Drop (only true is the carve-out)", falseFlag.Decision)
	}
}

// TestCarveOutEndToEndUnderZeroRatio proves the carve-out through a REAL TracerProvider whose base ratio is
// 0.0 (drop everything): an AlwaysSample() span is still recorded and exported, an ordinary one is not.
func TestCarveOutEndToEndUnderZeroRatio(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp), // synchronous export so GetSpans is immediate
		sdktrace.WithSampler(carveOutSampler{base: sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.0))}),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tr := tp.Tracer("t")

	_, ordinary := tr.Start(context.Background(), "ordinary", trace.WithNewRoot())
	ordinary.End()
	_, forced := tr.Start(context.Background(), "handoff", trace.WithNewRoot(), trace.WithAttributes(AlwaysSample()))
	forced.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans under a 0.0 base ratio, want exactly 1 (the AlwaysSample carve-out)", len(spans))
	}
	if spans[0].Name != "handoff" {
		t.Fatalf("the exported span is %q, want the AlwaysSample()d \"handoff\" span", spans[0].Name)
	}
}

// TestOtlpTracesConfigured pins the env gating: tracing turns on for EITHER the generic OTLP endpoint or the
// traces-specific one (the same "same env as metrics" contract the issue requires), and off for neither.
func TestOtlpTracesConfigured(t *testing.T) {
	cases := []struct {
		name          string
		generic, spec string
		want          bool
	}{
		{"neither", "", "", false},
		{"generic only", "localhost:4317", "", true},
		{"traces-specific only", "", "localhost:4317", true},
		{"both", "localhost:4317", "localhost:4318", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", c.generic)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", c.spec)
			if got := otlpTracesConfigured(); got != c.want {
				t.Errorf("otlpTracesConfigured() = %v, want %v (generic=%q traces=%q)", got, c.want, c.generic, c.spec)
			}
		})
	}
}

// TestNoOtelgrpcStreamInterceptorOnPlay is the structural guard for the #464 constraint: Play is ONE
// multi-hour bidi stream, so an otelgrpc STREAM interceptor would produce one span per player held until
// logout. No such interceptor may exist. This scans the source tree and fails if an otelgrpc stream
// interceptor is wired anywhere — a future slice adding one trips this rather than silently regressing the
// decision. (Unary interceptors on the account/handoff RPCs are explicitly allowed by #464 and are NOT
// matched here.)
func TestNoOtelgrpcStreamInterceptorOnPlay(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored/generated/VCS trees.
			switch d.Name() {
			case ".git", "vendor", "node_modules", "gen":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path) //nolint:gosec // path comes from WalkDir over the repo root, not user input
		if rerr != nil {
			return rerr
		}
		src := string(b)
		if strings.Contains(src, "otelgrpc") &&
			(strings.Contains(src, "StreamServerInterceptor") || strings.Contains(src, "StreamClientInterceptor")) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk source tree: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("an otelgrpc STREAM interceptor was added in %v — Play is one multi-hour stream, so a stream "+
			"interceptor produces one span per player held until logout (#464). Use per-span instrumentation "+
			"or a UNARY interceptor on the account/handoff RPCs instead.", offenders)
	}
}

// repoRoot walks up from the test's working directory to the module root (the dir holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the test working directory")
		}
		dir = parent
	}
}

// TestTraceSampleRatio pins the OTEL_TRACES_SAMPLER_ARG parsing + clamping.
func TestTraceSampleRatio(t *testing.T) {
	cases := []struct {
		val  string
		want float64
	}{
		{"", 1.0},
		{"1", 1.0},
		{"0", 0.0},
		{"0.25", 0.25},
		{"  0.5 ", 0.5},
		{"-1", 0.0},      // clamped
		{"2", 1.0},       // clamped
		{"garbage", 1.0}, // malformed -> default 1.0, not silently 0
	}
	for _, c := range cases {
		t.Setenv("OTEL_TRACES_SAMPLER_ARG", c.val)
		if got := traceSampleRatio(); got != c.want {
			t.Errorf("traceSampleRatio() with OTEL_TRACES_SAMPLER_ARG=%q = %v, want %v", c.val, got, c.want)
		}
	}
}

// TestTraceHandlerAddsIDsWhenSpanPresent pins #468: the stdout handler adds trace_id/span_id when a log is
// emitted with a ctx carrying a valid span, and adds nothing otherwise — the Tempo→Loki correlation, and
// zero cost when there is no span.
func TestTraceHandlerAddsIDsWhenSpanPresent(t *testing.T) {
	var buf bytes.Buffer
	h := traceHandler{next: slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})}
	log := slog.New(h)

	// A real recording span installed in the ctx.
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("t").Start(context.Background(), "op")
	wantTrace := span.SpanContext().TraceID().String()
	wantSpan := span.SpanContext().SpanID().String()

	log.InfoContext(ctx, "with span")
	span.End()
	out := buf.String()
	if !strings.Contains(out, `"trace_id":"`+wantTrace+`"`) {
		t.Errorf("log with a span-carrying ctx is missing the correct trace_id; got %q", out)
	}
	if !strings.Contains(out, `"span_id":"`+wantSpan+`"`) {
		t.Errorf("log with a span-carrying ctx is missing the correct span_id; got %q", out)
	}

	// A log with NO span (ctx-less form / Background) gets neither id.
	buf.Reset()
	log.Info("no span")
	if got := buf.String(); strings.Contains(got, "trace_id") || strings.Contains(got, "span_id") {
		t.Errorf("a log with no span must carry no trace_id/span_id; got %q", got)
	}
}
