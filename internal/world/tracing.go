package world

import (
	"math"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope for every span this package starts (shown as otel.scope.name on
// the exported span). One constant so the handoff (#465) and session-attach (#466) traces share a scope.
const tracerName = "github.com/double-nibble/telosmud/internal/world"

// tracer returns the world package's OTel tracer from the GLOBAL provider. Like the metrics instruments, it
// resolves lazily through OTel's global delegation, so it is a no-op (non-recording spans) until obs.Init
// installs a real TracerProvider — and stays a no-op in every test that does not (#464).
func tracer() trace.Tracer { return otel.Tracer(tracerName) }

// clampInt64 converts a uint64 counter (an ownership epoch, a lease generation) to the int64 that
// attribute.Int64 requires, clamping at math.MaxInt64. These counters never approach that in practice; the
// explicit upper-bound check is what makes the narrowing conversion provably safe — which satisfies BOTH
// static checkers this repo runs (gosec G115 and CodeQL's "incorrect integer conversion", a high-severity
// rule) with a real guard rather than a suppression comment that only silences one of them.
func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
