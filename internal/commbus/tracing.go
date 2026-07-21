package commbus

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope for the comms bus's producer/consumer spans (#467).
const tracerName = "github.com/double-nibble/telosmud/internal/commbus"

func tracer() trace.Tracer { return otel.Tracer(tracerName) }

// msgCarrier is a W3C TextMapCarrier over a Message's Trace map, so the global propagator can inject a
// producer's span context into (and extract it out of) the message envelope. It rides Data on NATS and the
// in-proc Message on the mem transports, so a traceparent survives BOTH paths through one mechanism.
type msgCarrier map[string]string

func (c msgCarrier) Get(k string) string { return c[k] }
func (c msgCarrier) Set(k, v string)     { c[k] = v }
func (c msgCarrier) Keys() []string {
	ks := make([]string, 0, len(c))
	for k := range c {
		ks = append(ks, k)
	}
	return ks
}

// subjectKind reduces a concrete subject to a BOUNDED, fixed-set kind for a span attribute. Tell/config
// subjects are per-player (telos.comms.tell.<playerId>) and thus unbounded, player-driven cardinality — the
// exact #470 boundary — so the raw subject must NEVER become an attribute. A small closed set of kinds is
// the bounded dimension worth having ("was this a tell, a channel, a scope event").
func subjectKind(subj string) string {
	switch {
	case strings.HasPrefix(subj, TellPrefix):
		return "tell"
	case strings.HasPrefix(subj, ChanPrefix):
		return "chan"
	case strings.HasPrefix(subj, ConfigPrefix):
		return "config"
	case strings.HasPrefix(subj, RosterPrefix):
		return "roster"
	case strings.HasPrefix(subj, "telos.scope."):
		return "scope"
	default:
		return "other"
	}
}

// startProducer starts a short-lived PRODUCER span for a publish and injects its span context into
// msg.Trace, so a consumer can LINK to it. Returns the updated message + the span to End once the publish
// returns. When no TracerProvider is installed the span is non-recording and Inject writes a not-sampled
// context, so this is a cheap no-op — a message published with tracing off carries an empty/omitted Trace.
func startProducer(ctx context.Context, subj string, msg Message) (Message, trace.Span) {
	ctx, span := tracer().Start(ctx, "commbus.publish "+subjectKind(subj),
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("telos.subject.kind", subjectKind(subj)),
			// messaging.message_id is the idempotency key <AuthorID>:<Seq> — #467 asks for it so the dedup
			// story is readable on the trace. It is player-influenced and unbounded: FINE on a span (Tempo),
			// but it must NEVER become a Prometheus label / span-metric dimension (#470). If #469's exemplar
			// or a spanmetrics connector ever derives labels from these spans, use telos.subject.kind (the
			// bounded dimension), never messaging.message_id.
			attribute.String("messaging.message_id", msg.IdempotencyKey),
		),
	)
	// Only allocate + inject when there is a real (recording, sampled) span to propagate. With tracing off
	// the span context is invalid, so skipping the alloc keeps a publish on the hot comms path free of a
	// wasted map allocation per message (the resulting nil Trace also omits the field on the wire).
	if span.SpanContext().IsValid() {
		if msg.Trace == nil {
			msg.Trace = map[string]string{}
		}
		otel.GetTextMapPropagator().Inject(ctx, msgCarrier(msg.Trace))
		if len(msg.Trace) == 0 {
			msg.Trace = nil // propagator wrote nothing (e.g. not sampled) — omit the field entirely
		}
	}
	return msg, span
}

// maxTraceCarrier bounds the incoming Trace map a consumer will feed to the propagator: a bound on the
// number of keys and on each value's length. A traceparent+tracestate+baggage carrier is a handful of short
// keys; anything past this bound is a malformed or hostile envelope from a compromised producer, and passing
// it to the SDK would (a) let the W3C parsers emit an error-handler log LINE PER MESSAGE — a
// producer-influenced unbounded log, against the #454/#456/#481 log-hardening — and (b) let attacker-chosen
// oversized values ride on. Over the bound we skip extraction entirely (the delivery span simply gets no
// link), which is strictly safer than a fabricated one.
const (
	maxTraceKeys     = 8
	maxTraceValueLen = 512
)

// ProducerLink returns a span link to the producer that published msg, for a consumer OUTSIDE commbus (the
// zone actor, #467 zone-mailbox half) that wants to link its own span to the message's origin. It reuses the
// same security boundary as startConsumer: a bounded extract, and it returns ONLY the producer's span
// context (via the link) — attacker-controllable baggage in msg.Trace never crosses. Returns a zero (dropped
// by the SDK) link when there is no valid, in-bounds producer context.
func ProducerLink(msg Message) trace.Link {
	if !carrierWithinBounds(msg.Trace) {
		return trace.Link{}
	}
	producerCtx := otel.GetTextMapPropagator().Extract(context.Background(), msgCarrier(msg.Trace))
	return trace.LinkFromContext(producerCtx)
}

func carrierWithinBounds(t map[string]string) bool {
	if len(t) > maxTraceKeys {
		return false
	}
	for _, v := range t {
		if len(v) > maxTraceValueLen {
			return false
		}
	}
	return true
}

// startConsumer extracts the producer's span context from msg.Trace and starts a CONSUMER span LINKED to it
// — never a child. JetStream is at-least-once: a delivery may be the Nth redelivery and may arrive long
// after the producer span ended, so a link ("caused by") is truthful where parent-child ("contained in")
// would be a lie, and a redelivery days later parented to a long-dead trace is worse than no trace. The
// idempotency key + delivery attempt make a redelivery VISIBLY a redelivery: a second consumer span linked
// to the SAME producer with attempt=2, not a second child of one parent. Returns the span to End after the
// handler runs (its ctx carries the consumer span for any handler that threads it — the #467 zone-mailbox
// half).
func startConsumer(subj string, msg Message, backlog bool, attempt int) (context.Context, trace.Span) {
	// Extract the producer context ONLY from a bounded carrier — a malformed/oversized Trace from a
	// compromised producer is dropped rather than fed to the SDK (see maxTraceCarrier).
	producerCtx := context.Background()
	if carrierWithinBounds(msg.Trace) {
		producerCtx = otel.GetTextMapPropagator().Extract(context.Background(), msgCarrier(msg.Trace))
	}
	// SECURITY BOUNDARY: the consumer span roots at context.Background(), NOT at producerCtx. The composite
	// propagator extracts BAGGAGE as well as the traceparent, and baggage is attacker-controllable (a
	// compromised RoleWorld producer can set any key in msg.Trace). Rooting the consumer span — and the ctx
	// this returns to handlers (the #467 zone-mailbox half) — at producerCtx would carry that baggage across
	// the bus and into downstream span attributes / logs: an unbounded, attacker-chosen cardinality + log
	// channel. Only the producer's SPAN CONTEXT (a bounded id reference) crosses, via the link below; baggage
	// deliberately does not. Do NOT "propagate baggage" by rooting the consumer at producerCtx.
	ctx, span := tracer().Start(context.Background(), "commbus.deliver "+subjectKind(subj),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithLinks(trace.LinkFromContext(producerCtx)),
		trace.WithAttributes(
			attribute.String("messaging.system", "nats"),
			attribute.String("telos.subject.kind", subjectKind(subj)),
			attribute.String("messaging.message_id", msg.IdempotencyKey),
			attribute.Bool("telos.bus.backlog", backlog),
			attribute.Int("telos.bus.delivery_attempt", attempt),
		),
	)
	return ctx, span
}
