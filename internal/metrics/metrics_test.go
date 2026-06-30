package metrics

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestInstrumentsRecord wires the package's globally-delegated instruments onto a ManualReader provider and
// asserts each helper produces its metric. One test sets the global provider once (OTel global delegation
// re-binds the init-created instruments onto it).
func TestInstrumentsRecord(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr)))

	ctx := context.Background()
	RecordTickLag(ctx, "midgaard", 12.5)
	SetOccupancy(ctx, "midgaard", 7)
	FrameDropped(ctx)
	ConnOpened(ctx)
	ConnOpened(ctx)
	ConnClosed(ctx)
	RecordBusLag(ctx, 3.2)

	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got[m.Name] = true
		}
	}
	for _, want := range []string{
		"telos.zone.tick_lag_ms",
		"telos.zone.occupancy",
		"telos.gate.frames_dropped_total",
		"telos.gate.connections",
		"telos.bus.deliver_lag_ms",
	} {
		if !got[want] {
			t.Errorf("metric %q was not recorded; got %v", want, got)
		}
	}

	// The live-connection up/down counter nets to 1 (two opens, one close).
	conns := findSum(t, rm, "telos.gate.connections")
	if conns != 1 {
		t.Fatalf("telos.gate.connections = %d, want 1 (2 opened - 1 closed)", conns)
	}
}

func findSum(t *testing.T, rm metricdata.ResourceMetrics, name string) int64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) == 0 {
				t.Fatalf("%s: not an int64 sum with data points", name)
			}
			return sum.DataPoints[0].Value
		}
	}
	t.Fatalf("%s not found", name)
	return 0
}
