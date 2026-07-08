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

// busLagHistogram returns the telos.bus.deliver_lag_ms histogram's (count, sum), both 0 if never recorded.
func busLagHistogram(t *testing.T, rdr *sdkmetric.ManualReader) (uint64, float64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "telos.bus.deliver_lag_ms" {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok && len(h.DataPoints) > 0 {
				return h.DataPoints[0].Count, h.DataPoints[0].Sum
			}
		}
	}
	return 0, 0
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
	recordBusDeliverLag(time.Now().UnixMilli() + 10_000)
	c2, s2 := busLagHistogram(t, rdr)
	if c2 != c1+1 {
		t.Fatalf("skewed sample: count %d -> %d, want +1 (still recorded, clamped)", c1, c2)
	}
	if s2 < s1 {
		t.Fatalf("skewed sample dragged the histogram Sum down (%.1f -> %.1f); it must clamp at 0", s1, s2)
	}

	// (3) an unstamped/foreign message → nothing recorded.
	recordBusDeliverLag(0)
	recordBusDeliverLag(-5)
	c3, _ := busLagHistogram(t, rdr)
	if c3 != c2 {
		t.Fatalf("an unstamped message recorded a sample: count %d -> %d", c2, c3)
	}
}
