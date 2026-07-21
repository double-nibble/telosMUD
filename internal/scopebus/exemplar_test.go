package scopebus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// assertBusLagExemplarPivot is the empirical "verified on busLag" check for #469 (a step of
// TestScopeBusDeliverLag, so it shares that test's busLag reader — the global-delegation trap forbids a
// second provider): recording busLag on busExemplarCtx of a SAMPLED producer trace (the #467 envelope)
// attaches a trace EXEMPLAR whose trace_id is the producer's, while an unsampled/absent trace attaches none.
func assertBusLagExemplarPivot(t *testing.T, rdr *sdkmetric.ManualReader) {
	t.Helper()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	const traceID = "0af7651916cd43dd8448eb211c80319c"
	sampled := commbus.Message{Trace: map[string]string{"traceparent": "00-" + traceID + "-b7ad6b7169203331-01"}}
	unsampled := commbus.Message{Trace: map[string]string{"traceparent": "00-" + traceID + "-b7ad6b7169203331-00"}}
	none := commbus.Message{}

	before := busLagExemplarTraceIDs(t, rdr)
	recordBusDeliverLag(busExemplarCtx(sampled), 1)   // -> attaches an exemplar
	recordBusDeliverLag(busExemplarCtx(unsampled), 1) // -> none (unsampled)
	recordBusDeliverLag(busExemplarCtx(none), 1)      // -> none (no producer trace)
	after := busLagExemplarTraceIDs(t, rdr)

	// Exactly one NEW exemplar, carrying the sampled producer's trace_id.
	fresh := after[len(before):]
	require.Equal(t, []string{traceID}, fresh,
		"busLag must gain exactly one exemplar — the SAMPLED producer's trace_id — for the #469 pivot")
}

// busLagExemplarTraceIDs collects the trace_ids of every exemplar currently on the busLag histogram.
func busLagExemplarTraceIDs(t *testing.T, rdr *sdkmetric.ManualReader) []string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, rdr.Collect(context.Background(), &rm))
	var ids []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "telos.bus.deliver_lag_ms" {
				continue
			}
			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "busLag should be a float64 histogram")
			for _, dp := range hist.DataPoints {
				for _, e := range dp.Exemplars {
					ids = append(ids, traceHex(e.TraceID))
				}
			}
		}
	}
	return ids
}

func traceHex(b []byte) string {
	const hexdigits = "0123456789abcdef"
	s := make([]byte, len(b)*2)
	for i, x := range b {
		s[i*2] = hexdigits[x>>4]
		s[i*2+1] = hexdigits[x&0x0f]
	}
	return string(s)
}
