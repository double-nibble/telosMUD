package commbus

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// newW3C is the W3C composite propagator obs.Init installs in production; the tests set it explicitly so
// Inject/Extract actually move a traceparent.
func newW3C() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

func installBusSpanRecorder(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	// Set the W3C propagator too — Inject/Extract are no-ops without it, so the producer→consumer link
	// would silently never form. obs.Init sets this in production; a test that skips it would be vacuous.
	otel.SetTextMapPropagator(newW3C())
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
	return exp
}

func spanByName(exp *tracetest.InMemoryExporter, name string) (tracetest.SpanStub, bool) {
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

func busStrAttr(s tracetest.SpanStub, key string) string {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
}

func busIntAttr(s tracetest.SpanStub, key string) int64 {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsInt64()
		}
	}
	return -1
}

// TestTransientTraceLink: a transient publish carries a traceparent to the subscriber, and the delivery span
// LINKS to the producer (not a child), attributed by the BOUNDED subject kind — never the per-player subject.
func TestTransientTraceLink(t *testing.T) {
	exp := installBusSpanRecorder(t)
	bus := NewMemBus()
	t.Cleanup(func() { _ = bus.Close() })

	var got Message
	done := make(chan struct{})
	sub, err := bus.Subscribe(TellSubject("player-42"), func(m Message) {
		got = m
		close(done)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, bus.Publish(context.Background(), TellSubject("player-42"),
		Message{IdempotencyKey: "author-1:7", Body: "hi"}))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tell never delivered")
	}

	// The message the subscriber received carries a W3C traceparent in the envelope.
	require.NotEmpty(t, got.Trace["traceparent"], "the delivered message must carry a traceparent")

	prod, ok := spanByName(exp, "commbus.publish tell")
	require.True(t, ok, "a producer span must exist")
	require.Equal(t, trace.SpanKindProducer, prod.SpanKind)

	cons, ok := spanByName(exp, "commbus.deliver tell")
	require.True(t, ok, "a consumer span must exist")
	require.Equal(t, trace.SpanKindConsumer, cons.SpanKind)

	// LINK, not parent-child: the consumer is a root (no parent) and LINKS to the producer's span context.
	require.False(t, cons.Parent.IsValid(), "the consumer span must NOT be a child of the producer (link, not parent)")
	require.Len(t, cons.Links, 1, "the consumer span must carry exactly one link (to the producer)")
	require.Equal(t, prod.SpanContext.TraceID(), cons.Links[0].SpanContext.TraceID(),
		"the consumer link must reference the producer's trace")
	require.Equal(t, prod.SpanContext.SpanID(), cons.Links[0].SpanContext.SpanID(),
		"the consumer link must reference the producer's span")

	// Bounded subject attribute — the KIND, never the per-player subject.
	require.Equal(t, "tell", busStrAttr(cons, "telos.subject.kind"))
	for _, s := range exp.GetSpans() {
		for _, a := range s.Attributes {
			require.NotContains(t, a.Value.AsString(), "player-42",
				"the per-player subject must never reach a span attribute (#470): span %q key %q", s.Name, a.Key)
		}
	}
	require.Equal(t, "author-1:7", busStrAttr(cons, "messaging.message_id"))
}

// TestDurableRedeliveryIsVisiblyRedelivery: a durable message NAK'd twice then acked produces THREE consumer
// spans, each LINKED to the SAME producer with a rising delivery_attempt — a redelivery is a fresh span
// linked to the one producer, not a second child of one parent (the at-least-once model, #467).
func TestDurableRedeliveryIsVisiblyRedelivery(t *testing.T) {
	exp := installBusSpanRecorder(t)
	js := NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })

	var attempts atomic.Int32
	done := make(chan struct{})
	cons, err := js.Consume(TellSubject("p9"), "consumer-1", func(_ Message, _ bool) AckDecision {
		if attempts.Add(1) < 3 {
			return RetryTransient
		}
		close(done)
		return AckDelivered
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, js.PublishDurable(context.Background(), TellSubject("p9"),
		Message{IdempotencyKey: "author-2:3"}))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("durable message never acked after redeliveries")
	}
	time.Sleep(50 * time.Millisecond) // let the final consumer span export

	prod, ok := spanByName(exp, "commbus.publish tell")
	require.True(t, ok, "a durable producer span must exist")

	// Collect the delivery spans; there must be exactly three, attempts 1/2/3, ALL linked to the one producer.
	var attemptsSeen []int64
	for _, s := range exp.GetSpans() {
		if s.Name != "commbus.deliver tell" {
			continue
		}
		require.Len(t, s.Links, 1, "each redelivery span links to the producer")
		require.Equal(t, prod.SpanContext.SpanID(), s.Links[0].SpanContext.SpanID(),
			"every redelivery must link to the SAME producer, not a new parent each time")
		attemptsSeen = append(attemptsSeen, busIntAttr(s, "telos.bus.delivery_attempt"))
	}
	require.ElementsMatch(t, []int64{1, 2, 3}, attemptsSeen,
		"three delivery spans with attempts 1,2,3 — a redelivery is visibly a redelivery")
}

// TestTracingOffLeavesNoTraceField: with no TracerProvider (tracing off), a publish injects nothing, so the
// message carries no Trace field — zero envelope overhead and no `"trace":{}` bytes on every message.
func TestTracingOffLeavesNoTraceField(t *testing.T) {
	// Explicitly no provider (noop).
	otel.SetTracerProvider(noop.NewTracerProvider())
	otel.SetTextMapPropagator(newW3C())

	bus := NewMemBus()
	t.Cleanup(func() { _ = bus.Close() })
	var got Message
	done := make(chan struct{})
	sub, _ := bus.Subscribe(TellSubject("z"), func(m Message) { got = m; close(done) })
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, bus.Publish(context.Background(), TellSubject("z"), Message{IdempotencyKey: "x:1"}))
	<-done
	require.Nil(t, got.Trace, "with tracing off the message must carry no Trace map (zero overhead)")
}

// TestConsumerContextCarriesNoProducerBaggage pins the F2 security boundary: baggage set by a (possibly
// compromised) producer and injected into the message envelope must NOT cross the bus into the consumer's
// context. Only the producer's span-context id crosses, via the link — never baggage. A refactor that rooted
// the consumer span at producerCtx (to "propagate baggage") would trip this.
func TestConsumerContextCarriesNoProducerBaggage(t *testing.T) {
	installBusSpanRecorder(t)
	// A hostile envelope: a valid traceparent PLUS attacker baggage.
	msg := Message{
		IdempotencyKey: "a:1",
		Trace: map[string]string{
			"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
			"baggage":     "attacker=pwned,player_id=victim",
		},
	}
	ctx, span := startConsumer(TellSubject("p"), msg, false, 1)
	defer span.End()

	// The link carried the producer's trace id (the id reference crosses)...
	stub := trace.SpanContextFromContext(ctx)
	_ = stub
	// ...but NO baggage member crossed into the consumer ctx.
	require.Empty(t, baggage.FromContext(ctx).Members(),
		"attacker baggage from the message envelope must NOT enter the consumer context (F2 boundary)")
}

// TestOversizedTraceCarrierIsDropped pins F4/F5: a malformed/oversized Trace map (a compromised producer
// flooding baggage) is not fed to the propagator — no link, no per-message SDK error log — rather than
// passed through.
func TestOversizedTraceCarrierIsDropped(t *testing.T) {
	exp := installBusSpanRecorder(t)
	huge := strings.Repeat("x", maxTraceValueLen+1)
	// A hostile envelope with a VALID traceparent AND an oversized value: without the bounds check the
	// propagator would extract the traceparent and form a link, so this is a non-vacuous guard — dropping the
	// whole carrier (no link) is what proves the oversized value never reached the SDK.
	msg := Message{IdempotencyKey: "a:1", Trace: map[string]string{
		"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"tracestate":  huge,
	}}
	_, span := startConsumer(TellSubject("p"), msg, false, 1)
	span.End()

	cons, ok := spanByName(exp, "commbus.deliver tell")
	require.True(t, ok)
	require.Empty(t, cons.Links, "an over-bound Trace carrier must yield NO link (dropped, not extracted) "+
		"even when it contains a valid traceparent")

	// And a carrier with too many keys is likewise dropped.
	many := map[string]string{}
	for i := 0; i < maxTraceKeys+1; i++ {
		many[string(rune('a'+i))] = "v"
	}
	require.False(t, carrierWithinBounds(many), "a carrier past the key bound must be rejected")
	require.True(t, carrierWithinBounds(map[string]string{"traceparent": "ok"}), "a normal carrier is within bounds")
}
