package metrics

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// testReader is the ONE ManualReader this binary ever installs, and installing it exactly once is a
// correctness requirement rather than tidiness.
//
// The instruments in this package are created at init() from the GLOBAL meter, and OTel's global
// delegation binds each one to the FIRST real provider that is set. A second SetMeterProvider does not
// re-bind them: they keep reporting into the first provider, and a fresh reader collects nothing. So a
// test that installed a provider per run passed at -count=1 and failed at -count=2 with every metric
// missing — which made `make test-race` (-count=100) red for this package.
var (
	testReader     *sdkmetric.ManualReader
	testReaderOnce sync.Once
)

// installReader returns the process-wide reader, installing the provider on first use.
func installReader() *sdkmetric.ManualReader {
	testReaderOnce.Do(func() {
		testReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(testReader)))
	})
	return testReader
}

// collect gathers the current values of every instrument.
func collect(t *testing.T, rdr *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rdr.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

// TestInstrumentsRecord wires the package's globally-delegated instruments onto a ManualReader provider and
// asserts each helper produces its metric. One test sets the global provider once (OTel global delegation
// re-binds the init-created instruments onto it).
func TestInstrumentsRecord(t *testing.T) {
	rdr := installReader()
	ctx := context.Background()

	// Cumulative instruments keep their values across runs of this test in the same binary, so every
	// numeric assertion below is a DELTA against a baseline taken here. Asserting absolute values would
	// pass at -count=1 and fail at -count=2 for a reason that has nothing to do with the code.
	base := collect(t, rdr)
	baseConns := sumOr0(base, "telos.gate.connections")
	baseLive := histCountOr0(base, "telos.bus.deliver_lag_ms")
	baseCatchAge := histCountOr0(base, "telos.bus.catchup_age_ms")
	baseCatchEvents := sumOr0(base, "telos.bus.catchup_events_total")
	RecordTickLag(ctx, "midgaard", 12.5)
	SetOccupancy(ctx, "midgaard", 7)
	FrameDropped(ctx)
	StreamStalled(ctx, "10.0.0.7:5001")
	ConnOpened(ctx)
	ConnOpened(ctx)
	ConnClosed(ctx)
	RecordBusLag(ctx, 3.2) // one LIVE delivery
	for range 3 {
		BusCatchup(ctx, "scope.world", 3_600_000) // three BACKLOG events, an hour old each
	}

	rm := collect(t, rdr)
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
		"telos.world.stream_stalled_total",
		"telos.gate.connections",
		"telos.bus.deliver_lag_ms",
		"telos.bus.catchup_events_total",
		"telos.bus.catchup_age_ms",
	} {
		if !got[want] {
			t.Errorf("metric %q was not recorded; got %v", want, got)
		}
	}

	// The live-connection up/down counter nets to +1 per run (two opens, one close).
	if conns := findSum(t, rm, "telos.gate.connections") - baseConns; conns != 1 {
		t.Fatalf("telos.gate.connections moved by %d, want 1 (2 opened - 1 closed)", conns)
	}

	// #276: the catch-up instruments are SEPARATE from the live-latency histogram. A resuming consumer's
	// multi-hour backlog must never be sampled into deliver_lag_ms, or its p99 becomes unreadable exactly
	// when you are trying to diagnose the recovery.
	//
	// One RecordBusLag call and three BusCatchup calls above. If the two ever shared an instrument, the
	// deliver_lag histogram would show 4 samples with an hour-long tail.
	if n := histogramCount(t, rm, "telos.bus.deliver_lag_ms") - baseLive; n != 1 {
		t.Fatalf("deliver_lag_ms recorded %d new samples, want 1 — a backlog event leaked into the live-latency histogram (#276)", n)
	}
	if n := histogramCount(t, rm, "telos.bus.catchup_age_ms") - baseCatchAge; n != 3 {
		t.Fatalf("catchup_age_ms recorded %d new samples, want 3", n)
	}
	if n := findSum(t, rm, "telos.bus.catchup_events_total") - baseCatchEvents; n != 3 {
		t.Fatalf("catchup_events_total moved by %d, want 3 (the catch-up DEPTH)", n)
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

// sumOr0 / histCountOr0 read a baseline WITHOUT failing when the instrument has not been recorded yet —
// on the first run of this binary nothing has, and a zero baseline is exactly right then. They are
// deliberately separate from findSum / histogramCount, which still fail loudly when a metric the test
// just recorded is missing.
func sumOr0(rm metricdata.ResourceMetrics, name string) int64 {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				return total
			}
		}
	}
	return 0
}

func histCountOr0(rm metricdata.ResourceMetrics, name string) uint64 {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				var n uint64
				for _, dp := range h.DataPoints {
					n += dp.Count
				}
				return n
			}
		}
	}
	return 0
}
