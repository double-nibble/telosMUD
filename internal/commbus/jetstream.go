package commbus

import (
	"context"
	"errors"
	"sync"
)

// jetstream.go is the DURABLE comms transport for Phase-8 slice 8.5 (docs/PHASE8-PLAN.md, OQ-1 =
// DURABLE-ALWAYS): every tell is a JetStream message, and "online delivery" is just a fast durable
// consumer being live. There is NO separate NATS-core online-tell path — that eliminates the
// online->offline logout race (a tell to a player logging out is never lost) and the dual code path
// (P8-D5). This mirrors the transient Bus split (NATS impl + Mem stand-in + Disabled no-op) so the
// whole durable-tell feature is unit-testable with NO live broker.
//
// # The two dedup layers it carries (P8-A5, redelivery storm)
//
// JetStream is at-least-once; a redelivery (consumer ack lost, a reconnect) must not double-render a
// tell. The model has TWO suppression layers, both modeled by this abstraction + the world consumer:
//
//  1. PUBLISH-SIDE dedup via the IdempotencyKey ("<authorID>:<seq>") set as the Nats-Msg-Id header —
//     a re-publish of an already-seen key inside the stream's dedup window is absorbed by the broker.
//     The world MUST always set the key (the MemJetStream cannot dedup an empty key).
//  2. CONSUMER-SIDE delivered-cursor — a per-player, per-sender last-delivered Seq persisted in the
//     character `state` JSONB (OQ-4). A redelivery AFTER the publish-dedup window (minutes later, a
//     consumer restart) is still suppressed at RENDER time because its Seq is <= the stored cursor.
//     This is the belt-and-suspenders layer that makes "render once" hold across a process restart —
//     it lives in the world (tell.go), not here, but this interface's at-least-once contract is what
//     makes it NECESSARY.
//
// # Ordering
//
// A single durable stream/subject is one append-ordered log; the per-player consumer drains it in
// order and acks in order, so a sender's tells arrive in send order (P8-A3, per-sender order). We do
// NOT promise a global cross-sender order (no shared clock; none is needed).
//
// # Bounded redelivery (poison-park, P8-A5)
//
// A handler that repeatedly fails to deliver (a NAK) is redelivered up to maxDeliver times, then PARKED
// (never redelivered again) rather than storming a just-logged-in player forever. The real JetStream
// consumer uses MaxDeliver; the MemJetStream models it with a per-message delivery counter. NOTE: an
// explicit NAK redelivers IMMEDIATELY — no BackOff is configured (jetstream_nats.go), and AckWait governs
// only a HUNG/lost-ack handler, not a NAK. So a tell that NAKs for a TRANSIENT reason (target mid-handoff,
// a gate-emit blip) burns all maxDeliver attempts in milliseconds and parks — a live never-lost concern
// tracked in #266 (the redelivery needs a real backoff to outlive a transient window).

// ErrJetStreamClosed is returned by a closed JetStream's PublishDurable/Consume.
var ErrJetStreamClosed = errors.New("commbus: jetstream is closed")

// DefaultMaxDeliver bounds redelivery of a single durable message (P8-A5): a handler that NAKs this
// many times has its message PARKED, never redelivered again, so a poison message cannot storm a
// freshly-joined player. A package var (not const) so a test can shrink it.
var DefaultMaxDeliver = 5

// JetStream is the durable publish/consume contract for tells (and, later, durable mail notifies).
// Three transports implement it — a NATS-JetStream impl for production, a MemJetStream stand-in for
// hermetic tests, and a Disabled no-op when JetStream is unavailable — exactly mirroring the
// transient Bus's NATS/Mem/Disabled split.
//
// Unlike the transient Bus, JetStream carries NO publish ACL/role: a durable tell is published by a
// WORLD shard (the source that resolved the target + set the author), and the per-player subject is
// the only routing — the world never lets a gate touch this handle (cmd/telos-gate wires only the
// transient OpenGate bus, never a JetStream handle).
type JetStream interface {
	// PublishDurable durably stores msg on subj, deduping by msg.IdempotencyKey (the publish-side
	// dedup window). It is at-least-once: the message survives until a consumer acks it, so a target
	// logging out between the publish and any delivery still receives it on next login (the durable-
	// always guarantee). Off any zone goroutine (the source world's tell path). A Disabled handle is
	// a safe no-op (returns nil) so tells degrade cleanly when JetStream is down.
	PublishDurable(ctx context.Context, subj string, msg Message) error

	// Consume runs a durable consumer for subj keyed by consumerID (stable per player so a restart
	// resumes, not restarts). handler is called once per message — the BACKLOG first (in append
	// order), then live messages as they arrive — on a bus-owned goroutine (never a zone goroutine).
	// The backlog bool is true for a message that was already stored when the consumer STARTED (the
	// offline catch-up — the world renders it "while you were away…"), false for a live message (a
	// real-time tell). handler returns ack=true to advance past the message (delivered or idempotently
	// suppressed) or ack=false to NAK it (redelivered immediately up to maxDeliver, then parked). Stop
	// tears the consumer down. A Disabled handle returns a no-op Consumer.
	Consume(subj, consumerID string, handler func(msg Message, backlog bool) bool) (Consumer, error)

	// Close releases the JetStream handle (the underlying connection is shared with the transient bus
	// in production, so Close here is a logical close of the JetStream context). Idempotent.
	Close() error
}

// Consumer is a live durable-consumer handle. Stop ends delivery and releases it; idempotent.
type Consumer interface {
	Stop() error
}

// --- Disabled no-op (JetStream unavailable; the never-fatal degradation) -----------------------

// DisabledJetStream returns a no-op JetStream: PublishDurable drops, Consume delivers nothing. Used
// when NATS/JetStream is unreachable so tells degrade to unavailable rather than crashing boot —
// exactly as the transient bus degrades via Disabled().
func DisabledJetStream() JetStream { return disabledJS{} }

type disabledJS struct{}

func (disabledJS) PublishDurable(context.Context, string, Message) error { return nil }
func (disabledJS) Consume(string, string, func(Message, bool) bool) (Consumer, error) {
	return disabledConsumer{}, nil
}
func (disabledJS) Close() error { return nil }

type disabledConsumer struct{}

func (disabledConsumer) Stop() error { return nil }

// --- MemJetStream adapter: make the 8.1 stand-in a JetStream -----------------------------------
//
// The 8.1 MemJetStream (memjs.go) already models the append log + publish-side idempotency dedup +
// per-consumer delivered cursor. Here we wrap it as a live JetStream: PublishDurable appends and
// wakes consumers; Consume spawns a goroutine that drains the backlog and then blocks for new
// appends, calling handler and modeling bounded redelivery (a NAK re-runs handler up to maxDeliver,
// then parks the message). This is the in-process model of one durable stream, one per-player
// consumer, no broker.

// memConsumer is one live consumer goroutine over a MemJetStream subject.
type memConsumer struct {
	js       *MemJetStream
	subj     string
	id       string
	handler  func(Message, bool) bool
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	// backlogRemaining is the count of already-stored messages at consumer START — the offline
	// catch-up. The first backlogRemaining deliveries are flagged backlog=true ("while you were
	// away…"); everything after is live. Touched only by run() (single goroutine), no lock needed.
	backlogRemaining int
}

// Consume implements JetStream for the stand-in: a goroutine drains the subject's backlog past this
// consumer's cursor (DeliverPending), then waits on the wake signal for new appends. Each delivered
// message runs handler with bounded redelivery; a permanently-NAKing message is parked (logged via
// the handler returning false maxDeliver times) and the cursor advances past it so it never storms.
func (js *MemJetStream) Consume(subj, consumerID string, handler func(Message, bool) bool) (Consumer, error) {
	js.mu.Lock()
	if js.closed {
		js.mu.Unlock()
		return nil, ErrJetStreamClosed
	}
	js.mu.Unlock()
	c := &memConsumer{
		js:               js,
		subj:             subj,
		id:               consumerID,
		handler:          handler,
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
		backlogRemaining: js.Pending(subj, consumerID), // the offline catch-up depth at start
	}
	go c.run()
	return c, nil
}

func (c *memConsumer) run() {
	defer close(c.done)
	for {
		// Capture the wake channel BEFORE draining: an Append closes-and-replaces it under the lock, so
		// if a publish lands AFTER our drain returns empty but BEFORE we wait, it closes THIS captured
		// channel and the select fires immediately (we re-drain). Capturing it after the drain would be a
		// LOST WAKEUP — the publish would close the old channel and we'd block on the fresh one forever.
		wake := c.js.wakeChan(c.subj)
		pending := c.js.deliverPendingForConsume(c.subj, c.id)
		for _, msg := range pending {
			backlog := c.backlogRemaining > 0
			if backlog {
				c.backlogRemaining--
			}
			c.deliverBounded(msg, backlog)
			select {
			case <-c.stop:
				return
			default:
			}
		}
		if len(pending) > 0 {
			// We delivered something; immediately re-loop to drain any concurrent appends without
			// blocking (and re-capture a fresh wake channel for the next wait).
			select {
			case <-c.stop:
				return
			default:
			}
			continue
		}
		// Nothing pending: wait for the captured wake (a publish) or stop.
		select {
		case <-c.stop:
			return
		case <-wake:
		}
	}
}

// deliverBounded runs handler for one message with bounded redelivery (P8-A5): a NAK re-runs it with
// no real backoff (the stand-in is synchronous) up to DefaultMaxDeliver attempts, then parks it
// (gives up — the cursor already advanced past it in DeliverPending, so it is not redelivered). An
// ACK simply returns. The point under test is that a permanently-failing message does not loop
// forever.
func (c *memConsumer) deliverBounded(msg Message, backlog bool) {
	for attempt := 0; attempt < DefaultMaxDeliver; attempt++ {
		if c.handler(msg, backlog) {
			return // acked
		}
		select {
		case <-c.stop:
			return
		default:
		}
	}
	// maxDeliver exhausted: park (drop). A real consumer would move it to a parked state; here the
	// cursor is already past it, so it is simply never retried.
}

func (c *memConsumer) Stop() error {
	c.stopOnce.Do(func() {
		close(c.stop)
		c.js.wake(c.subj) // unblock a run() parked on the wake channel so it observes stop
	})
	<-c.done
	return nil
}

// Compile-time assertions.
var (
	_ JetStream = (*MemJetStream)(nil)
	_ JetStream = disabledJS{}
	_ Consumer  = (*memConsumer)(nil)
	_ Consumer  = disabledConsumer{}
)
