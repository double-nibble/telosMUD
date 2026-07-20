package luasandbox

import (
	"fmt"
	"time"
	"unicode/utf8"
)

// logcap.go — bounds on builder-authored Lua logging (#456).
//
// Content Lua can write arbitrary strings into the process log via print / mud.log / director.log.
// Two builder-facing sinks default to Info, so they land in the log store under the default
// production posture — and once container stdout is shipped into Loki (Observability 2) that is a
// direct, always-on write primitive into the observability store for SEMI-TRUSTED content. Left
// unbounded it is a disk-fill / ingest-flood DoS against the whole box (the game shares the node),
// and it lets a broken/hostile script bury real signal during an incident.
//
// The defences are (a) a per-message LENGTH cap so one call cannot emit a megabyte, and (b) a
// per-CALL line-count cap so a tick-loop cannot emit millions of lines. Over the line cap the
// invocation ABORTS (like the instruction/allocation budgets), which feeds the circuit breaker so a
// script that floods every call is quarantined — bounded, not merely throttled. The per-call reset
// is at the SAME chokepoint that resets the instruction/spawn budgets, so nested calls share the
// budget (a script cannot re-nest to reset its log tally), mirroring the existing discipline.

const (
	// MaxLogMsgBytes caps a single builder log message. ~1KB is generous for a human-readable
	// diagnostic; anything larger is a report the log is the wrong channel for.
	MaxLogMsgBytes = 1024

	// MaxLogsPerCall caps builder log lines emitted within one FRAME (a hook/event/tick call).
	// Generous — legitimate content rarely logs more than a handful per call — so crossing it is a
	// clear flood signal. The (cap+1)th call aborts the invocation and feeds the breaker.
	MaxLogsPerCall = 200

	// MaxLogLinesPerSec bounds SUSTAINED builder-log volume per runtime (per zone / per director) in
	// wall-clock time. The per-call cap bounds a single frame; it does NOT bound the call RATE — a
	// self-rescheduling mud.after timer (≈4 fires/sec) or many co-firing timers can emit a per-call
	// burst every tick forever, which is a disk-fill / ingest-flood DoS against the whole node (the
	// game shares it; k3s local-path PVCs don't enforce size). This token-bucket DROPS builder log
	// lines past the rate so sustained volume is hard-bounded (~50 lines × ~1KB = ~50KB/s/runtime)
	// regardless of how many calls or nested frames produce them (#456).
	MaxLogLinesPerSec = 50
)

// LogRateLimiter is a wall-clock token bucket bounding sustained builder-log volume per runtime. Burst
// is MaxLogsPerCall (a single legitimate call may emit up to a full per-call burst); it refills at
// MaxLogLinesPerSec. Not goroutine-safe: a runtime is driven from one serialized goroutine. The clock
// is injectable for deterministic tests (nil => time.Now). Log volume is not gameplay state, so using
// wall-clock here — as the sandbox deadline already does — does not affect replay determinism.
type LogRateLimiter struct {
	now     func() time.Time
	tokens  float64
	last    time.Time
	dropped int64
}

// NewLogRateLimiter builds a full bucket. now may be nil (=> time.Now); tests inject a fake clock.
func NewLogRateLimiter(now func() time.Time) *LogRateLimiter {
	if now == nil {
		now = time.Now
	}
	return &LogRateLimiter{now: now, tokens: MaxLogsPerCall, last: now()}
}

// Allow refills by elapsed wall time (capped at the burst) and takes one token. It returns true if a
// token was available (log the line) or false if the bucket is empty (DROP the line); a drop is
// counted. A nil limiter allows everything (a runtime built without one is unbounded — never in prod).
func (l *LogRateLimiter) Allow() bool {
	if l == nil {
		return true
	}
	t := l.now()
	if elapsed := t.Sub(l.last).Seconds(); elapsed > 0 {
		l.tokens += elapsed * MaxLogLinesPerSec
		if l.tokens > MaxLogsPerCall {
			l.tokens = MaxLogsPerCall
		}
		l.last = t
	}
	if l.tokens >= 1 {
		l.tokens--
		return true
	}
	l.dropped++
	return false
}

// Dropped is the cumulative count of builder log lines dropped by the rate limit — surfaced as a
// metric so an operator can see a script being throttled.
func (l *LogRateLimiter) Dropped() int64 {
	if l == nil {
		return 0
	}
	return l.dropped
}

// logTruncationMarker is appended to a message clipped by CapLogMsg so a reader can tell the line
// was cut rather than genuinely ending there.
const logTruncationMarker = "…[truncated]"

// CapLogMsg clamps s to at most MaxLogMsgBytes bytes (plus the truncation marker), never splitting a
// UTF-8 rune. Shared by the zone and director log sinks so the bound is identical everywhere.
func CapLogMsg(s string) string {
	if len(s) <= MaxLogMsgBytes {
		return s
	}
	cut := MaxLogMsgBytes
	// Back up to a rune boundary so a multibyte rune is never sliced mid-sequence.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + logTruncationMarker
}

// LogFloodError is the error raised when a script exceeds MaxLogsPerCall within one invocation. It is
// a plain (non-budget, non-deadline) error, so ClassifyError buckets it as AbortLogic — the heaviest
// breaker weight — because a log flood is deterministic and unambiguously the script's own doing
// (not transient host load), so it should quarantine a chronically-flooding script promptly.
func LogFloodError() error {
	return fmt.Errorf("lua log rate exceeded (max %d log lines per call)", MaxLogsPerCall)
}
