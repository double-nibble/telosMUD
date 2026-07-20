package luasandbox

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestLogRateLimiter pins the token bucket: a full burst is allowed, then lines drop until wall-clock
// time refills tokens at MaxLogLinesPerSec. Uses an injected clock for determinism.
func TestLogRateLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLogRateLimiter(func() time.Time { return now })

	// Clock frozen: exactly the burst (MaxLogsPerCall) lines pass, the rest drop.
	allowed := 0
	for i := 0; i < MaxLogsPerCall*3; i++ {
		if l.Allow() {
			allowed++
		}
	}
	if allowed != MaxLogsPerCall {
		t.Fatalf("frozen clock: allowed %d, want the burst %d", allowed, MaxLogsPerCall)
	}
	if want := int64(MaxLogsPerCall*3 - MaxLogsPerCall); l.Dropped() != want {
		t.Fatalf("dropped %d, want %d", l.Dropped(), want)
	}

	// Advance one second: ~MaxLogLinesPerSec tokens refill.
	now = now.Add(time.Second)
	refilled := 0
	for i := 0; i < MaxLogsPerCall; i++ {
		if l.Allow() {
			refilled++
		}
	}
	if refilled < MaxLogLinesPerSec-1 || refilled > MaxLogLinesPerSec+1 {
		t.Fatalf("after 1s refill allowed %d, want ~%d", refilled, MaxLogLinesPerSec)
	}
}

// TestNilLogRateLimiterAllows: a nil limiter allows everything (a runtime built without one is
// unbounded — defensive, never the case in production).
func TestNilLogRateLimiter(t *testing.T) {
	var l *LogRateLimiter
	if !l.Allow() {
		t.Error("nil limiter must allow")
	}
	if l.Dropped() != 0 {
		t.Error("nil limiter drops nothing")
	}
}

// logcap_test.go — #456: the shared builder-log bounds (length cap + per-call rate abort + label),
// tested at the luasandbox layer that the director shares.

func TestCapLogMsg(t *testing.T) {
	// Short message: unchanged.
	if got := CapLogMsg("hello"); got != "hello" {
		t.Errorf("short message altered: %q", got)
	}
	// At the boundary: unchanged.
	exact := strings.Repeat("a", MaxLogMsgBytes)
	if got := CapLogMsg(exact); got != exact {
		t.Errorf("message at the cap was altered")
	}
	// Over the cap: truncated, marker appended, content clipped near the cap.
	long := strings.Repeat("a", MaxLogMsgBytes*4)
	got := CapLogMsg(long)
	if !strings.HasSuffix(got, logTruncationMarker) {
		t.Errorf("truncated message must end with the marker; got tail %q", got[len(got)-20:])
	}
	if body := strings.TrimSuffix(got, logTruncationMarker); len(body) > MaxLogMsgBytes {
		t.Errorf("truncated body %d bytes exceeds cap %d", len(body), MaxLogMsgBytes)
	}
	// Multibyte: truncation never splits a rune.
	multi := strings.Repeat("界", MaxLogMsgBytes) // 3 bytes each, well over the cap
	if got := CapLogMsg(multi); !utf8.ValidString(got) {
		t.Errorf("CapLogMsg split a multibyte rune (invalid UTF-8)")
	}
}

// TestRuntimeLogFloodTripsBreaker: a director-style script that prints in a tight loop past the
// per-call cap aborts, and repeated it trips the shared-runtime breaker (quarantined, not unbounded).
func TestRuntimeLogFloodTripsBreaker(t *testing.T) {
	var buf bytes.Buffer
	rt := NewRuntime(slog.New(slog.NewTextHandler(&buf, nil)), Opts{})
	defer rt.Close()
	if err := rt.Compile("s", `function flood() for i = 1, 250 do print("x") end end`); err != nil {
		t.Fatal(err)
	}
	if err := rt.LoadGlobals("s"); err != nil {
		t.Fatal(err)
	}

	// One call must abort at the per-call cap, not complete.
	if _, err := rt.CallGlobal("s", "flood", 0, nil); err == nil {
		t.Fatal("a 100k-line print loop must abort at the per-call cap")
	}

	tripped := false
	for i := 0; i < 50; i++ {
		if _, err := rt.CallGlobal("s", "flood", 0, nil); err != nil {
			_ = err
		}
		if rt.breaker.disabled("s") {
			tripped = true
			break
		}
	}
	if !tripped {
		t.Fatal("a script flooding the log every call must trip the breaker")
	}
}

// TestRuntimePrintLabelledAndCapped: the runtime's print is labelled source=builder_lua and its
// message is length-capped.
func TestRuntimePrintLabelledAndCapped(t *testing.T) {
	var buf bytes.Buffer
	rt := NewRuntime(slog.New(slog.NewTextHandler(&buf, nil)), Opts{})
	defer rt.Close()
	if err := rt.Compile("s", `function go() print(string.rep("Z", 5000)) end`); err != nil {
		t.Fatal(err)
	}
	if err := rt.LoadGlobals("s"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.CallGlobal("s", "go", 0, nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "source=builder_lua") {
		t.Errorf("print must be labelled source=builder_lua, got:\n%s", truncateForMsg(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("the over-cap print must be truncated")
	}
	if n := strings.Count(out, "Z"); n > MaxLogMsgBytes+8 {
		t.Errorf("print logged %d 'Z's; not length-capped", n)
	}
}

// truncateForMsg keeps a failure dump readable.
func truncateForMsg(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
