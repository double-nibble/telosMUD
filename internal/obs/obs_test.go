package obs

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestLogRawInput pins the TELOS_LOG_RAW_INPUT opt-in that gates verbatim player-input logging
// (#454). It is a SEPARATE flag from DEBUG on purpose: turning on debug logging must never
// silently start recording player tells/chat/link-codes into a durable log store. This asserts
// the two are decoupled — DEBUG alone does not enable raw input, and raw input alone does not
// enable DEBUG.
func TestLogRawInput(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"off", false},
		{"no", false},
		{"1", true},
		{"true", true},
		{"TRUE", true}, // case-insensitive
		{"yes", true},
		{"on", true},
		{" 1 ", true}, // trimmed
	}
	for _, tc := range cases {
		t.Setenv("TELOS_LOG_RAW_INPUT", tc.val)
		if got := LogRawInput(); got != tc.want {
			t.Errorf("LogRawInput() with TELOS_LOG_RAW_INPUT=%q = %v, want %v", tc.val, got, tc.want)
		}
	}
}

// TestRawInputDecoupledFromDebug asserts the two flags do not bleed into each other: enabling one
// must never enable the other. This is the crux of #454 — "turn on debug" can never mean "record
// player chat".
func TestRawInputDecoupledFromDebug(t *testing.T) {
	t.Setenv("DEBUG", "1")
	t.Setenv("TELOS_LOG_RAW_INPUT", "")
	if LogRawInput() {
		t.Error("DEBUG=1 must NOT enable raw-input logging")
	}
	if !DebugEnabled() {
		t.Error("DEBUG=1 should enable debug")
	}

	t.Setenv("DEBUG", "")
	t.Setenv("TELOS_LOG_RAW_INPUT", "1")
	if DebugEnabled() {
		t.Error("TELOS_LOG_RAW_INPUT=1 must NOT enable debug")
	}
	if !LogRawInput() {
		t.Error("TELOS_LOG_RAW_INPUT=1 should enable raw-input logging")
	}
}

// TestLogOTLP pins the TELOS_OTEL_LOGS opt-in (the OTLP log bridge, #459). Off by default; distinct
// from DEBUG and TELOS_LOG_RAW_INPUT.
func TestLogOTLP(t *testing.T) {
	t.Setenv("TELOS_OTEL_LOGS", "")
	if LogOTLP() {
		t.Error("LogOTLP must default off")
	}
	t.Setenv("TELOS_OTEL_LOGS", "1")
	if !LogOTLP() {
		t.Error("TELOS_OTEL_LOGS=1 should enable the OTLP log bridge")
	}
	// Independent of DEBUG.
	t.Setenv("TELOS_OTEL_LOGS", "")
	t.Setenv("DEBUG", "1")
	if LogOTLP() {
		t.Error("DEBUG=1 must not enable the OTLP log bridge")
	}
}

// TestMultiHandlerFansOut: a record reaches every child handler (stdout + OTLP bridge in production).
func TestMultiHandlerFansOut(t *testing.T) {
	var a, b bytes.Buffer
	h := newMultiHandler(
		slog.NewTextHandler(&a, &slog.HandlerOptions{Level: slog.LevelInfo}),
		slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelInfo}),
	)
	slog.New(h).With("svc", "x").Info("hello", "k", "v")
	for name, buf := range map[string]*bytes.Buffer{"a": &a, "b": &b} {
		if got := buf.String(); !strings.Contains(got, "hello") || !strings.Contains(got, "svc=x") || !strings.Contains(got, "k=v") {
			t.Errorf("handler %s missing the fanned-out record/attrs: %q", name, got)
		}
	}
}

// TestLevelHandlerFilters: the level wrapper (used so the OTLP bridge honors the stdout level) drops
// records below its level and passes those at/above.
func TestLevelHandlerFilters(t *testing.T) {
	var buf bytes.Buffer
	lh := &levelHandler{level: slog.LevelWarn, next: slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})}
	log := slog.New(lh)
	log.Info("below")   // dropped (Info < Warn)
	log.Warn("atlevel") // kept
	out := buf.String()
	if strings.Contains(out, "below") {
		t.Errorf("levelHandler leaked a below-threshold record: %q", out)
	}
	if !strings.Contains(out, "atlevel") {
		t.Errorf("levelHandler dropped an at-threshold record: %q", out)
	}
}
