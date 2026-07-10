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
	RecordBusLag(ctx, 3.2) // one LIVE delivery
	for range 3 {
		BusCatchup(ctx, "scope.world", 3_600_000) // three BACKLOG events, an hour old each
	}

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
		"telos.bus.catchup_events_total",
		"telos.bus.catchup_age_ms",
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

	// #276: the catch-up instruments are SEPARATE from the live-latency histogram. A resuming consumer's
	// multi-hour backlog must never be sampled into deliver_lag_ms, or its p99 becomes unreadable exactly
	// when you are trying to diagnose the recovery.
	//
	// One RecordBusLag call and three BusCatchup calls above. If the two ever shared an instrument, the
	// deliver_lag histogram would show 4 samples with an hour-long tail.
	if n := histogramCount(t, rm, "telos.bus.deliver_lag_ms"); n != 1 {
		t.Fatalf("deliver_lag_ms recorded %d samples, want 1 — a backlog event leaked into the live-latency histogram (#276)", n)
	}
	if n := histogramCount(t, rm, "telos.bus.catchup_age_ms"); n != 3 {
		t.Fatalf("catchup_age_ms recorded %d samples, want 3", n)
	}
	if n := findSum(t, rm, "telos.bus.catchup_events_total"); n != 3 {
		t.Fatalf("catchup_events_total = %d, want 3 (the catch-up DEPTH)", n)
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

// histogramCount returns the total sample count of a float64 histogram, summed across attribute sets.
func histogramCount(t *testing.T, rm metricdata.ResourceMetrics, name string) uint64 {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is not a float64 histogram", name)
			}
			var n uint64
			for _, dp := range h.DataPoints {
				n += dp.Count
			}
			return n
		}
	}
	t.Fatalf("histogram %q not found", name)
	return 0
}
