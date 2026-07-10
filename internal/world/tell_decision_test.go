package world

import (
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// tell_decision_test.go — #266. routeTellDeliver classifies every failure the world can produce. All of them
// are TRANSIENT (they clear on their own), so each must map to RetryTransient and redeliver on the backoff
// schedule. Mapping any of them to AckDelivered would silently DROP the tell; mapping one to DropPoison would
// discard it without a single retry. The emit-blip branch is covered end-to-end by the chaos suite; the two
// branches below have no other coverage.

// TestRouteTellDeliverNilZoneIsTransient: the target's current zone is unresolvable (mid cross-shard handoff,
// or a bare session with no currentZone). The tell must stay durable and redeliver, never be acked away.
func TestRouteTellDeliverNilZoneIsTransient(t *testing.T) {
	got := routeTellDeliver(func() *Zone { return nil }, "Bob", commbus.Message{Body: "hi"}, false)
	if got != commbus.RetryTransient {
		t.Fatalf("an unresolvable zone must be RetryTransient (the tell stays durable), got %v", got)
	}
}

// TestRouteTellDeliverAckTimeoutIsTransient: the zone never replies (shutting down / overloaded). The tell
// must redeliver rather than be lost. We post to a zone whose loop is NOT running, so nothing ever answers.
func TestRouteTellDeliverAckTimeoutIsTransient(t *testing.T) {
	old := tellAckTimeout
	tellAckTimeout = 25 * time.Millisecond
	t.Cleanup(func() { tellAckTimeout = old })

	z := newZone("tz") // never Run: its inbox is never drained, so the ack never comes
	start := time.Now()
	got := routeTellDeliver(func() *Zone { return z }, "Bob", commbus.Message{Body: "hi"}, false)
	if got != commbus.RetryTransient {
		t.Fatalf("a zone that never acks must be RetryTransient (the tell stays durable), got %v", got)
	}
	if elapsed := time.Since(start); elapsed < tellAckTimeout {
		t.Fatalf("routeTellDeliver returned before the ack timeout elapsed (%v < %v)", elapsed, tellAckTimeout)
	}
}
