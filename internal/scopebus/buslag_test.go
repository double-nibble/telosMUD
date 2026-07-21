package scopebus

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// histoStats returns a float64 histogram's (count, sum) summed across attribute sets, both 0 if never recorded.
func histoStats(t *testing.T, rdr *sdkmetric.ManualReader, name string) (uint64, float64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			var count uint64
			var sum float64
			for _, dp := range h.DataPoints {
				count += dp.Count
				sum += dp.Sum
			}
			return count, sum
		}
	}
	return 0, 0
}

// busLagHistogram returns the telos.bus.deliver_lag_ms histogram's (count, sum).
func busLagHistogram(t *testing.T, rdr *sdkmetric.ManualReader) (uint64, float64) {
	t.Helper()
	return histoStats(t, rdr, "telos.bus.deliver_lag_ms")
}

// TestScopeBusDeliverLag consolidates the #44 bus-lag assertions into ONE test: OTel's global meter binds its
// instruments on the FIRST SetMeterProvider, so a second call in a sibling test would leave that test's reader
// empty. Sharing one provider + delta assertions avoids that.
//
//   - a transient Signal->Subscribe round-trip records exactly one publish->deliver sample;
//   - a future publish stamp (cross-host skew) still records, but CLAMPED to 0 so it never drags the Sum down;
//   - an unstamped/foreign message (PubMillis 0) records nothing.
func TestScopeBusDeliverLag(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr)))

	// (1) transient round-trip → one sample.
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	b := New(core)
	got := make(chan struct{}, 1)
	sub, err := b.Subscribe(World(), func(string, json.RawMessage, string) { got <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	c0, _ := busLagHistogram(t, rdr)
	if err := b.Signal(context.Background(), World(), "invasion.start", json.RawMessage(`{"n":1}`), "world-director"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("event not delivered")
	}
	c1, s1 := busLagHistogram(t, rdr)
	if c1 != c0+1 {
		t.Fatalf("transient round-trip: deliver-lag count %d -> %d, want +1", c0, c1)
	}

	// (2) a skewed (future) stamp → still records, clamped so the Sum does not drop.
	recordBusDeliverLag(context.Background(), time.Now().UnixMilli()+10_000)
	c2, s2 := busLagHistogram(t, rdr)
	if c2 != c1+1 {
		t.Fatalf("skewed sample: count %d -> %d, want +1 (still recorded, clamped)", c1, c2)
	}
	if s2 < s1 {
		t.Fatalf("skewed sample dragged the histogram Sum down (%.1f -> %.1f); it must clamp at 0", s1, s2)
	}

	// (3) an unstamped/foreign message → nothing recorded.
	recordBusDeliverLag(context.Background(), 0)
	recordBusDeliverLag(context.Background(), -5)
	c3, _ := busLagHistogram(t, rdr)
	if c3 != c2 {
		t.Fatalf("an unstamped message recorded a sample: count %d -> %d", c2, c3)
	}

	// (4) #276: a BACKLOG delivery goes to the catch-up instruments, never to deliver_lag_ms. A resuming
	// consumer drains events published hours ago; sampling those into the live-latency histogram would dwarf
	// its distribution exactly when you are trying to read it during a recovery.
	catch0, _ := histoStats(t, rdr, "telos.bus.catchup_age_ms")
	recordBusCatchup("scope.world", time.Now().UnixMilli()-3_600_000) // an hour behind
	catch1, catchSum := histoStats(t, rdr, "telos.bus.catchup_age_ms")
	if catch1 != catch0+1 {
		t.Fatalf("catchup_age_ms count %d -> %d, want +1", catch0, catch1)
	}
	if catchSum < 3_000_000 {
		t.Fatalf("catchup_age_ms sum = %.0f; an hour-old backlog event must record its real age", catchSum)
	}
	if c4, _ := busLagHistogram(t, rdr); c4 != c3 {
		t.Fatalf("a BACKLOG event leaked into the live-latency histogram: deliver_lag count %d -> %d (#276)", c3, c4)
	}

	// (5) the same clamps as deliver_lag: an unstamped backlog event records nothing, and a skewed future
	// stamp clamps at 0 rather than dragging the Sum down.
	recordBusCatchup("scope.world", 0)
	recordBusCatchup("scope.world", -5)
	if c5, _ := histoStats(t, rdr, "telos.bus.catchup_age_ms"); c5 != catch1 {
		t.Fatalf("an unstamped backlog event recorded a sample: %d -> %d", catch1, c5)
	}
	recordBusCatchup("scope.world", time.Now().UnixMilli()+10_000)
	c6, sum6 := histoStats(t, rdr, "telos.bus.catchup_age_ms")
	if c6 != catch1+1 {
		t.Fatalf("a skewed backlog stamp must still record (clamped): %d -> %d", catch1, c6)
	}
	if sum6 < catchSum {
		t.Fatalf("a skewed backlog stamp dragged the Sum down (%.0f -> %.0f); it must clamp at 0", catchSum, sum6)
	}

	// (6) #469 — the metric→trace EXEMPLAR pivot on busLag. Recording on a ctx carrying a SAMPLED producer
	// span context (off the #467 trace envelope, via busExemplarCtx) attaches a trace exemplar whose trace_id
	// is the producer's; an unsampled/absent producer trace attaches none (an exemplar exists only for a
	// sampled trace). Kept in THIS test because the busLag reader is bound to this test's provider (the
	// global-delegation trap the header documents) — a sibling test with its own reader would see nothing.
	assertBusLagExemplarPivot(t, rdr)
}
