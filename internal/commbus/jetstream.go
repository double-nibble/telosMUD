package commbus

import (
	"context"
	"errors"
	"sync"
	"time"
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
// # Redelivery: transient retry vs poison drop (P8-A5, #266)
//
// A handler returns an AckDecision, not a bool, so the two failure modes are separated:
//
//   - DropPoison — the message can NEVER be delivered (malformed). It is dropped immediately, loudly.
//     It must not spend the transient budget, and it must not storm a freshly-joined player.
//   - RetryTransient — delivery failed for a momentary reason (the target is mid cross-shard handoff, a
//     gate-emit blip, an overloaded zone). This is the ONLY arm that redelivers, and it does so on the
//     NakBackoff SCHEDULE (`m.NakWithDelay`), not immediately.
//
// The immediate-NAK was #266: a bare `Nak()` redelivers at once (AckWait governs only a HUNG/lost-ack
// handler, and ConsumerConfig.BackOff is NOT applied to an explicit NAK), so a tell NAKing for a
// transient reason burned every attempt in milliseconds and parked — losing it long before the transient
// window (sub-second handoff; seconds of bus blip) could clear.
//
// # Ordering under a delayed NAK (why MaxAckPending=1)
//
// A delayed redelivery introduces a loss path of its own if the consumer pipelines: message N is NAK'd
// with a delay, the consumer delivers N+1, N+1 renders and advances the per-sender delivered-cursor past
// seq(N); N's delayed redelivery then arrives BELOW the cursor and is suppressed as a duplicate — N is
// silently lost, and per-sender order (P8-A3) is broken. MaxAckPending=1 forbids delivering N+1 while N
// is outstanding, so the cursor can never overtake a pending message. The MemJetStream models this by
// construction (it resolves one message fully before the next).
//
// # Parking is terminal
//
// After DefaultMaxDeliver attempts the message is PARKED. The durable consumer is never deleted (its name
// is the stable player id, so a restart RESUMES from the last ack), which means a parked message is never
// redelivered — parking is permanent loss. The schedule is therefore sized so that no realistic transient
// reaches it; a park means an outage longer than the whole schedule, and is logged at ERROR and counted
// (metrics.DurableParked) as an incident rather than dropped silently.
//
// # The honest bound of "never lost"
//
// The guarantee this transport provides is: a durable message is never lost to any TRANSIENT shorter than
// totalNakWindow. It is not, and cannot be, "delivered to the player's screen". The terminal hop — the
// world's transient publish to the gate — is at-most-once: it returns once NATS-core accepts the frame, not
// once the gate renders it, and only then does the handler ack and the durable copy get discarded. A gate
// that drops the frame after that point (slow-consumer overflow, mid-reconnect) loses the message. So the
// invariant holds from publish through the world→gate handoff, and no further. Widening it would require an
// end-to-end render ack, which is a different design.
//
// # MaxAckPending=1 is a transport-wide constraint
//
// Consume hardcodes MaxAckPending=1 for EVERY caller, because both of today's durable consumers need it:
// COMMS_TELL (the per-sender delivered-cursor) and WORLD_EVENTS (scopebus's per-source `applied` high-water)
// each have the same overtaking hazard. A future consumer that tolerates reordering (a seen-set rather than
// a high-water) would be serialized anyway; if one is ever written, make this a per-consumer option rather
// than relaxing it here.

// ErrJetStreamClosed is returned by a closed JetStream's PublishDurable/Consume.
var ErrJetStreamClosed = errors.New("commbus: jetstream is closed")

// AckDecision is a durable-message handler's verdict. It separates a TRANSIENT failure (retry on the
// backoff schedule) from POISON (drop now), so a momentary blip can never spend a poison-sized budget and
// a permanently-undeliverable message can never storm the consumer (#266).
type AckDecision int

const (
	// AckDelivered means the message was rendered, or idempotently suppressed by the delivered-cursor.
	// Advance past it.
	AckDelivered AckDecision = iota
	// RetryTransient means delivery failed for a momentary reason: redeliver on the NakBackoff schedule.
	// It is the ONLY arm that consumes the redelivery budget.
	RetryTransient
	// DropPoison means the message is undeliverable forever (malformed). Drop it and log; it never
	// redelivers and spends no budget.
	DropPoison
)

// DefaultMaxDeliver bounds redelivery of a single durable message (P8-A5): a message RetryTransient'd this
// many times is PARKED (permanent loss — see the terminal note above), so a stuck message cannot redeliver
// forever. Sized against NakBackoff so the covered window outlives any realistic transient. A package var
// (not const) so a test can shrink it.
var DefaultMaxDeliver = 10

// NakBackoff is the redelivery schedule for a RetryTransient NAK: the delay before attempt i+1 after
// attempt i failed. It RAMPS then HOLDS — once exhausted, the last entry repeats for the remaining
// MaxDeliver attempts, giving a long flat tail without an unbounded wait. With DefaultMaxDeliver=10 the
// nine gaps are 200ms,1s,3s,10s,30s,30s,30s,30s,30s ≈ 164s of covered transient:
//
//	a cross-shard handoff (sub-second) clears by attempt 2; a NATS/gate bus blip (seconds) by attempt 4;
//	a reconnect (tens of seconds) by attempt 6. Only a multi-minute outage reaches the park.
//
// It is ALSO installed as ConsumerConfig.BackOff so the hung/lost-ack (AckWait-expiry) redelivery path
// paces identically. A package var so a gated test can shrink it. NATS requires len(BackOff) <= MaxDeliver.
var NakBackoff = []time.Duration{
	200 * time.Millisecond,
	1 * time.Second,
	3 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

// consumerBackoff returns the BackOff slice to install on the JetStream consumer, truncated so it always
// satisfies the broker's constraint that MaxDeliver be STRICTLY GREATER than len(BackOff) (NATS err_code
// 10116). Without this, shrinking DefaultMaxDeliver below len(NakBackoff) — which a test or an operator may
// do — makes CreateOrUpdateConsumer fail outright, and tells go silently undelivered rather than merely
// retrying fewer times. Truncation is safe: nakBackoff() holds at the last entry past the schedule's end, so
// the client-side NakWithDelay pacing is unchanged. Returns nil when there is no room for any entry.
func consumerBackoff() []time.Duration {
	room := DefaultMaxDeliver - 1
	if room <= 0 {
		return nil
	}
	if room >= len(NakBackoff) {
		return NakBackoff
	}
	return NakBackoff[:room]
}

// totalNakWindow is the cumulative time a RetryTransient message is retried over before it parks: the sum
// of the DefaultMaxDeliver-1 gaps between attempts. This is the transient window the never-lost guarantee
// actually covers — anything longer parks. Reported on the park alarm so an operator sees the bound.
func totalNakWindow() time.Duration {
	var total time.Duration
	for attempt := 1; attempt < DefaultMaxDeliver; attempt++ {
		total += nakBackoff(attempt)
	}
	return total
}

// nakBackoff returns the delay before the NEXT delivery, given the 1-based attempt number that just
// failed (JetStream's Metadata().NumDelivered). Past the end of the schedule the last entry repeats.
func nakBackoff(numDelivered int) time.Duration {
	if len(NakBackoff) == 0 {
		return 0
	}
	i := numDelivered - 1
	if i < 0 {
		i = 0
	}
	if i >= len(NakBackoff) {
		i = len(NakBackoff) - 1
	}
	return NakBackoff[i]
}

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
	// real-time tell). handler returns an AckDecision: AckDelivered advances past the message,
	// RetryTransient NAKs it onto the NakBackoff schedule (parked after DefaultMaxDeliver attempts), and
	// DropPoison discards it now. Stop tears the consumer down. A Disabled handle returns a no-op Consumer.
	//
	// opts customize the consumer (#312). With no options the production posture applies (MaxAckPending==1,
	// the serializing default both current consumers require); a future reorder-tolerant consumer opts out
	// via WithMaxAckPending rather than the transport relaxing the constraint globally.
	Consume(subj, consumerID string, handler func(msg Message, backlog bool) AckDecision, opts ...ConsumeOption) (Consumer, error)

	// Close releases the JetStream handle (the underlying connection is shared with the transient bus
	// in production, so Close here is a logical close of the JetStream context). Idempotent.
	Close() error
}

// ConsumeConfig carries the per-consumer overrides a Consume call resolves from its options (#312). Its zero
// value is NOT the default — resolveConsumeConfig seeds the production posture first, then applies options —
// so a field left unset (or set <= 0) keeps that default.
type ConsumeConfig struct {
	// MaxAckPending caps how many delivered-but-unacked messages the broker keeps in flight for this
	// consumer. The default (1) SERIALIZES delivery, which is load-bearing — not tuning — for both existing
	// durable consumers:
	//   - COMMS_TELL: the world's per-sender delivered-cursor would let a successor advance past a delay-
	//     NAK'd message, suppressing it as a duplicate on redelivery (silent loss);
	//   - WORLD_EVENTS: scopebus's per-source `applied` high-water has the identical overtaking hazard.
	// The constraint lives in the transport today; making it a per-consumer field lets a FUTURE consumer
	// whose delivered-progress model tolerates reordering (a seen-SET rather than a high-water cursor) raise
	// it, WITHOUT relaxing it for the ordered consumers. <= 0 means "use the default".
	//
	// Raising this alone only grants the BROKER permission to have >1 message unacked in flight — it does
	// NOT by itself make the client faster: the consume callback still runs messages serially on one
	// goroutine, so a consumer that raises this and changes nothing else inherits the reordering hazard with
	// none of the throughput gain. A real reorder-tolerant consumer also needs client-side handler
	// concurrency and likely a revisited AckWait (jsAckWait) once many messages are pending per consumer.
	MaxAckPending int
}

// ConsumeOption customizes a durable consumer at Consume time (#312). No options == the production default
// (MaxAckPending == 1).
type ConsumeOption func(*ConsumeConfig)

// WithMaxAckPending overrides the consumer's MaxAckPending (default 1). n <= 0 keeps the default. ONLY safe
// for a consumer whose delivered-progress model tolerates reordering (see ConsumeConfig.MaxAckPending); the
// two current consumers (COMMS_TELL, WORLD_EVENTS) require the serializing default and must NOT set this.
func WithMaxAckPending(n int) ConsumeOption {
	return func(c *ConsumeConfig) { c.MaxAckPending = n }
}

// resolveConsumeConfig seeds the load-bearing production defaults, then applies opts over them. A resolved
// MaxAckPending is always >= 1 (a caller passing <= 0 keeps the serializing default) so the transport never
// builds a consumer with an invalid unlimited-ack posture by accident.
func resolveConsumeConfig(opts ...ConsumeOption) ConsumeConfig {
	cfg := ConsumeConfig{MaxAckPending: 1} // the serializing default both ordered consumers require
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if cfg.MaxAckPending <= 0 {
		cfg.MaxAckPending = 1
	}
	return cfg
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
func (disabledJS) Consume(string, string, func(Message, bool) AckDecision, ...ConsumeOption) (Consumer, error) {
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
	handler  func(Message, bool) AckDecision
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
//
// opts are accepted for interface parity (#312) but have no effect here: the in-memory model delivers one
// message at a time on a single goroutine (blocking for the handler's ack before the next), so it is already
// effectively MaxAckPending==1 and a higher value would not change its behavior — the MaxAckPending contract
// is a broker property exercised against the real NATS consumer, not this stand-in.
func (js *MemJetStream) Consume(subj, consumerID string, handler func(Message, bool) AckDecision, _ ...ConsumeOption) (Consumer, error) {
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

// deliverBounded runs handler for one message with the same redelivery SEMANTICS the real consumer has
// (#266): AckDelivered returns; DropPoison drops after exactly one call, spending no budget and no delay;
// RetryTransient re-runs it up to DefaultMaxDeliver attempts, RECORDING the NakBackoff delay it would
// have waited, then parks it (gives up — the cursor already advanced past it in DeliverPending).
//
// The stand-in RECORDS the schedule rather than sleeping it. Sleeping ~164s of real backoff would make
// every hermetic test unusable, and wall time is not the property under test: the properties are the
// DECISION arms, the attempt COUNT, the delay SCHEDULE, and the ordering. A test asserts the recorded
// delays (js.NakDelays) and the parked set (js.Parked) — which is what #62 taught us to do, after the
// old double encoded a false timing model that hid this very bug. A test that needs the transient to
// "clear" flips its own flag; it never needs the clock to advance.
//
// Because this loop resolves one message completely before run() delivers the next, the stand-in models
// the real consumer's MaxAckPending=1 by construction — the invariant that stops a successor from
// advancing the delivered-cursor past a pending, delay-NAK'd message.
func (c *memConsumer) deliverBounded(msg Message, backlog bool) {
	for attempt := 1; attempt <= DefaultMaxDeliver; attempt++ {
		switch c.handler(msg, backlog) {
		case AckDelivered:
			return
		case DropPoison:
			c.js.notePoison(c.id, msg)
			return // exactly one handler call; no delay, no budget spent
		default: // RetryTransient
			c.js.noteNak(c.id, nakBackoff(attempt))
		}
		select {
		case <-c.stop:
			return
		default:
		}
	}
	// Budget exhausted: PARK. Permanent loss in the real consumer (the durable is never deleted), so the
	// stand-in records it — a test asserts a park happened rather than the message silently vanishing.
	c.js.notePark(c.id, msg)
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
