package world

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// producerTraceOf publishes-and-captures: it starts a real producer span and returns the Trace envelope a
// delivered message would carry, plus the producer's trace/span ids — so the zone-side test can assert the
// zone.deliver_tell span LINKS to that exact producer.
func producerTraceOf(t *testing.T) (map[string]string, string) {
	t.Helper()
	tr := otel.Tracer("test-producer")
	ctx, span := tr.Start(context.Background(), "producer")
	defer span.End()
	carrier := map[string]string{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(carrier))
	return carrier, span.SpanContext().SpanID().String()
}

// TestZoneTellDeliverSpan pins the #467 zone-mailbox half: handling a drained durable tell on the ZONE
// goroutine starts a "zone.deliver_tell" span that (a) LINKS to the producer that published the tell (not a
// child), (b) records inbox queue-wait, (c) labels the zone by TEMPLATE (never the instance id, #470), and
// (d) carries only an immutable Trace envelope + timestamp on the message — never a cancellable context.
func TestZoneTellDeliverSpan(t *testing.T) {
	exp := installSpanRecorder(t)
	// The link only forms if a propagator is installed to move the traceparent (obs.Init sets this in prod).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	prodTrace, prodSpanID := producerTraceOf(t)

	sh := NewDemoShard()
	z := sh.zoneByID("midgaard")
	// A resident target so the delivery renders (not a NAK).
	s := newTestPlayerEntity(z, "Bob")
	z.join(s, "")

	// Deliver a drained tell as the off-zone consumer would: an immutable Trace envelope + an enqueue time,
	// posted onto the inbox and handled on the zone goroutine.
	m := tellDeliverMsg{
		target:   "Bob",
		msg:      commbus.Message{AuthorID: "Alice", AuthorName: "Alice", Seq: 1, Body: "hi", Trace: prodTrace},
		ack:      make(chan bool, 1),
		enqueued: time.Now().Add(-25 * time.Millisecond), // some queue wait to observe
	}
	ok := z.deliverTellTraced(m)
	require.True(t, ok, "the drained tell should be delivered to the resident")

	span, found := findSpan(exp, "zone.deliver_tell")
	require.True(t, found, "handling a drained tell must start a zone.deliver_tell span")

	// (a) LINK to the producer, not a child.
	require.False(t, span.Parent.IsValid(), "the zone tell span must not be a child of the producer")
	require.Len(t, span.Links, 1, "it must link to the producer")
	require.Equal(t, prodSpanID, span.Links[0].SpanContext.SpanID().String(),
		"the link must reference the exact producer that published the tell")

	// (b) queue-wait recorded and (c) zone by template.
	// enqueued was set 25ms in the past, so queue-wait must be recorded AND positive — a strict >0 also fails
	// if the attribute is absent (spanInt64Attr returns 0), making this a non-vacuous guard.
	require.Greater(t, spanInt64Attr(span, "telos.zone.queue_wait_ms"), int64(0),
		"inbox queue-wait must be recorded and positive (the hot-zone saturation signal)")
	require.Equal(t, "midgaard", spanAttr(span, "telos.zone"))

	// (d) No instance-shaped value anywhere on the span.
	assertNoInstanceIDAttr(t, exp.GetSpans())
}
