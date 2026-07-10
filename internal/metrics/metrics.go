// Package metrics holds TelosMUD's OpenTelemetry instruments (Phase 16.1). Call sites record through the
// thin helpers here; obs.Init installs the global MeterProvider (OTLP when configured, else a no-op), so a
// record is negligible when no exporter is wired. Instruments are created from the GLOBAL meter — OTel's
// global delegation re-wires them onto the real provider when obs.Init (or a test) calls SetMeterProvider,
// so import order does not matter.
package metrics

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// scope is the instrumentation scope name (the module path), shown as otel_scope_name on exported series.
const scope = "github.com/double-nibble/telosmud"

var meter = otel.Meter(scope)

var (
	tickLag          metric.Float64Histogram
	occupancy        metric.Int64Gauge
	framesDropped    metric.Int64Counter
	streamStalled    metric.Int64Counter
	gateConns        metric.Int64UpDownCounter
	busLag           metric.Float64Histogram
	busCatchupEvents metric.Int64Counter
	busCatchupAge    metric.Float64Histogram
	drainRedirected  metric.Int64Counter
	drainReclaimed   metric.Int64Counter
	durableParked    metric.Int64Counter
	durablePoisoned  metric.Int64Counter
)

func init() {
	// Instrument-creation errors are ignored: a nil instrument's record methods are safe no-ops, so a failed
	// instrument degrades to "this metric is off", never a crash.
	tickLag, _ = meter.Float64Histogram("telos.zone.tick_lag_ms",
		metric.WithDescription("Zone heartbeat overrun: how long a pulse's callbacks ran past the 250ms budget"),
		metric.WithUnit("ms"))
	occupancy, _ = meter.Int64Gauge("telos.zone.occupancy",
		metric.WithDescription("Live players in a zone"))
	framesDropped, _ = meter.Int64Counter("telos.gate.frames_dropped_total",
		metric.WithDescription("Server frames dropped because a player's outbound buffer was full (slow client)"))
	streamStalled, _ = meter.Int64Counter("telos.world.stream_stalled_total",
		metric.WithDescription("Play streams torn down by the world because an outbound frame was blocked in Send "+
			"past the stall bound (#274) — the gate was answering keepalives but had stopped reading the stream. "+
			"Labeled by the peer `gate` address, because one wedged gate stalls every player it serves (they share "+
			"an HTTP/2 connection window), so the burst needs somewhere to attribute itself. Non-zero means a gate "+
			"is wedged at the application layer; keepalive structurally cannot see that case."))
	gateConns, _ = meter.Int64UpDownCounter("telos.gate.connections",
		metric.WithDescription("Live gate connections"))
	busLag, _ = meter.Float64Histogram("telos.bus.deliver_lag_ms",
		metric.WithDescription("Scoped-event-bus publish->deliver latency"),
		metric.WithUnit("ms"))
	busCatchupEvents, _ = meter.Int64Counter("telos.bus.catchup_events_total",
		metric.WithDescription("Durable scoped-event BACKLOG events drained by a resuming consumer (#276). Labeled by "+
			"subject. Distinct from live deliveries: this counts how much a consumer had missed while it was down, "+
			"which is a recovery/MTTR signal, not a delivery-latency one."))
	busCatchupAge, _ = meter.Float64Histogram("telos.bus.catchup_age_ms",
		metric.WithDescription("Age (now - publish time) of each durable scoped-event BACKLOG event at delivery (#276). "+
			"Its MAX at a consumer's resume is how far behind that consumer was. Deliberately NOT folded into "+
			"telos.bus.deliver_lag_ms, whose live-latency distribution a multi-hour catch-up would dwarf."),
		metric.WithUnit("ms"))
	drainRedirected, _ = meter.Int64Counter("telos.shard.drain_redirected_total",
		metric.WithDescription("Players redirected to a peer shard during a graceful drain (socket kept open, zero drop)"))
	drainReclaimed, _ = meter.Int64Counter("telos.shard.drain_reclaimed_total",
		metric.WithDescription("Players still resident at the drain deadline, dropped to reconnect from durable state; "+
			"labeled fault=infra (a connected in-world player the drain could not hand off in time) vs "+
			"fault=client (link-dead, or never finished connecting)"))
	durableParked, _ = meter.Int64Counter("telos.commbus.durable_parked_total",
		metric.WithDescription("Durable messages PARKED after exhausting the redelivery budget — permanent LOSS "+
			"(the never-lost invariant only covers transients shorter than the backoff window). Labeled by stream. "+
			"Any non-zero value is an incident: an outage outlived the whole retry schedule."))
	durablePoisoned, _ = meter.Int64Counter("telos.commbus.durable_poisoned_total",
		metric.WithDescription("Durable messages DROPPED as undeliverable (malformed / permanently unroutable). "+
			"Labeled by stream. Distinct from parked: poison spends no retry budget and is never a transient."))
}

func zoneAttr(zone string) metric.RecordOption {
	return metric.WithAttributes(attribute.String("zone", zone))
}

// RecordTickLag records a zone heartbeat's overrun (ms past the pulse budget) for the named zone.
func RecordTickLag(ctx context.Context, zone string, ms float64) {
	if tickLag != nil {
		tickLag.Record(ctx, ms, zoneAttr(zone))
	}
}

// SetOccupancy reports the current live-player count for a zone.
func SetOccupancy(ctx context.Context, zone string, n int64) {
	if occupancy != nil {
		occupancy.Record(ctx, n, metric.WithAttributes(attribute.String("zone", zone)))
	}
}

// FrameDropped counts one dropped outbound frame for a slow client (the zone never blocks; it drops). It is
// deliberately label-free — a per-player label would explode cardinality, and the headline signal is the
// shard-wide drop rate.
func FrameDropped(ctx context.Context) {
	if framesDropped != nil {
		framesDropped.Add(ctx, 1)
	}
}

// StreamStalled counts a Play stream the world reclaimed because its peer stopped reading (#274). Non-zero
// means a gate is wedged at the application layer — a case gRPC keepalive structurally cannot see, because
// the peer's HTTP/2 stack keeps acking PINGs independently of application flow control.
func StreamStalled(ctx context.Context, gate string) {
	if streamStalled != nil {
		streamStalled.Add(ctx, 1, metric.WithAttributes(attribute.String("gate", gate)))
	}
}

// ConnOpened increments the live gate connection count.
func ConnOpened(ctx context.Context) { add(ctx, gateConns, 1) }

// ConnClosed decrements the live gate connection count.
func ConnClosed(ctx context.Context) { add(ctx, gateConns, -1) }

func add(ctx context.Context, c metric.Int64UpDownCounter, n int64) {
	if c != nil {
		c.Add(ctx, n)
	}
}

// RecordBusLag records a scoped-event publish->deliver latency (ms).
func RecordBusLag(ctx context.Context, ms float64) {
	if busLag != nil {
		busLag.Record(ctx, ms)
	}
}

// BusCatchup records one durable scoped-event BACKLOG delivery: the event count and its age (#276).
//
// It exists because deliver_lag_ms deliberately skips backlog. A consumer resuming after an outage drains
// events published hours ago; sampling those into the live-latency histogram would dwarf its distribution and
// make the p99 useless exactly when you are trying to read it. But the catch-up DEPTH is itself the signal you
// want during recovery — how far behind is this consumer, and is it converging — so it gets its own pair.
//
// ageMillis is clamped at 0 by the caller: cross-host clock skew can make now-PubMillis negative, and a
// negative sample would drag the histogram's SUM down and poison any avg=sum/count panel.
func BusCatchup(ctx context.Context, subject string, ageMillis float64) {
	attrs := metric.WithAttributes(attribute.String("subject", subject))
	if busCatchupEvents != nil {
		busCatchupEvents.Add(ctx, 1, attrs)
	}
	if busCatchupAge != nil {
		busCatchupAge.Record(ctx, ageMillis, attrs)
	}
}

// DrainRedirected counts players redirected to a peer during a graceful drain (zero-drop; socket kept open).
func DrainRedirected(ctx context.Context, n int) {
	if drainRedirected != nil && n > 0 {
		drainRedirected.Add(ctx, int64(n))
	}
}

// DrainReclaimed counts drain-deadline stragglers dropped to reconnect from durable state, labeled by fault
// ("infra" | "client"). Kept to those two low-cardinality label values, like the other shard-wide counters.
func DrainReclaimed(ctx context.Context, n int, fault string) {
	if drainReclaimed != nil && n > 0 {
		drainReclaimed.Add(ctx, int64(n), metric.WithAttributes(attribute.String("fault", fault)))
	}
}

// DurableParked counts a durable message that exhausted its redelivery budget and was parked — PERMANENT
// LOSS (#266). It is the alertable signal that the never-lost guarantee's covered window was exceeded, so it
// must never be folded together with the (routine, harmless) poison drop. `stream` is low-cardinality
// (COMMS_TELL | WORLD_EVENTS) — never a per-player subject.
func DurableParked(ctx context.Context, stream string) {
	if durableParked != nil {
		durableParked.Add(ctx, 1, metric.WithAttributes(attribute.String("stream", stream)))
	}
}

// DurablePoisoned counts a durable message dropped as undeliverable (malformed / permanently unroutable).
// Routine and bounded — it spends no retry budget — but still worth a counter so a spike in malformed
// content is visible rather than only appearing as a log line.
func DurablePoisoned(ctx context.Context, stream string) {
	if durablePoisoned != nil {
		durablePoisoned.Add(ctx, 1, metric.WithAttributes(attribute.String("stream", stream)))
	}
}
