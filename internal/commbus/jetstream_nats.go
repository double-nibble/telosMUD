package commbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/double-nibble/telosmud/internal/metrics"
)

// jetstream_nats.go is the PRODUCTION durable-tell transport: a thin wrapper over a NATS JetStream
// context that publishes durable tells (with Nats-Msg-Id dedup) and runs a per-player durable
// consumer with bounded redelivery. It is the only file importing nats.go/jetstream, so the world
// package and the tests depend solely on the JetStream interface — the broker is needed only for a
// live process and the gated integration test (mirrors nats.go for the transient bus).
//
// Optional, never fatal: NewJetStream returns an error if JetStream is unreachable; the caller (the
// world wiring) treats it as "tells disabled" and uses DisabledJetStream() — never a boot failure.
//
// # The durable model (P8-D5, OQ-1 durable-always)
//
//   - Stream COMMS_TELL captures telos.comms.dtell.> (every per-target tell subject) with FILE
//     storage and a Duplicates window so a re-publish of the same Nats-Msg-Id within the window is
//     deduped by the broker (the publish-side layer). Old tells age out via MaxAge (a bounded backlog).
//   - A per-player DURABLE consumer (Durable name keyed by the player id) filters dtell.<player> and
//     delivers in stream order, acking explicitly. MaxDeliver bounds redelivery so a poison message
//     parks instead of storming (P8-A5). The consumer is durable so a restart RESUMES from the last
//     ack, not the start — that, plus the character-state delivered-cursor, makes "render once" hold
//     across a restart even though JetStream is at-least-once.

// jsStreamName is the durable-tell stream. Captures every per-target dtell subject.
const jsStreamName = "COMMS_TELL"

// jsScopeStreamName is the durable SCOPED-EVENT stream (Phase 10.2b): it captures every scoped-event
// subject (telos.scope.>) so a director/zone's durable consumer replays the state-changing world events
// it missed while down. ScopeSubjectPrefix is the single source of truth for that subject root — the
// scopebus package's SubjectRoot is defined as this, so the stream binding and the publisher can never
// drift apart. (Kept here, not in scopebus, because scopebus imports commbus, not the reverse.)
const (
	jsScopeStreamName  = "WORLD_EVENTS"
	ScopeSubjectPrefix = "telos.scope."
)

// DtellPrefix / DtellSubject build the durable per-target tell subject (P8-D2). DtellSubject is built
// from a RESOLVED player id (the source world resolved the target via the directory) — never free-
// form client text concatenated into the subject space (P8-A8, subject injection).
const DtellPrefix = SubjectRoot + "dtell." // telos.comms.dtell.<targetPlayerId>

// DtellSubject builds the per-target durable-tell subject from a resolved player id.
func DtellSubject(playerID string) string { return DtellPrefix + playerID }

// jsDedupWindow / jsMaxAge / jsAckWait tune the stream + consumer. The dedup window is the publish-
// side Nats-Msg-Id suppression horizon; MaxAge bounds how long an undelivered (offline) tell lives;
// AckWait is how long the broker waits for an ack before redelivering. Package vars so a gated test
// can shrink them.
var (
	jsDedupWindow = 2 * time.Minute
	jsMaxAge      = 30 * 24 * time.Hour // an offline tell survives ~30 days, then ages out
	jsAckWait     = 30 * time.Second
)

// maxAttemptClamp bounds the broker's uint64 NumDelivered before it is narrowed to an int (#266). The real
// counter never approaches this — the consumer parks at DefaultMaxDeliver — so the clamp exists purely to
// make the narrowing provably overflow-free.
const maxAttemptClamp uint64 = 1 << 20

// NATSJetStream is the JetStream-backed durable transport. It owns the JetStream context (over a NATS
// connection it does NOT own — the connection is shared with the transient bus in production, so Close
// here only stops the consumers, it does not close the connection).
type NATSJetStream struct {
	js     jetstream.JetStream
	stream jetstream.Stream
	name   string // the stream name (COMMS_TELL | WORLD_EVENTS): the low-cardinality metric label
	log    *slog.Logger

	// nc is the underlying NATS connection. TODAY dialJetStream creates a DEDICATED connection per handle
	// (newJetStreamFromConn is split out so a future wiring MAY share the transient bus's — hence "from
	// conn"); either way Close does not close it (logical close only). Retained so the parkmon can subscribe
	// to the broker's MAX_DELIVERIES advisory, a CORE NATS subject the jetstream context does not expose (#311).
	nc *nats.Conn
	// parkSub is the live MAX_DELIVERIES advisory subscription (#311); Unsubscribed on Close. nil when the
	// subscribe failed (never fatal — the per-park ERROR log remains the operator backstop).
	parkSub *nats.Subscription
}

// durableConsumerConfig builds the per-consumer JetStream config. Extracted from Consume so the production
// values — above all MaxAckPending, which is load-bearing rather than tuning (#266) — are pinned by a
// HERMETIC test rather than only by the broker-gated tier. maxAckPending comes from the resolved
// ConsumeConfig (#312): the default is 1 (the serializing posture both current consumers require); a caller
// may raise it only for a reorder-tolerant consumer (see ConsumeConfig.MaxAckPending).
func durableConsumerConfig(subj, consumerID string, maxAckPending int) jetstream.ConsumerConfig {
	// Fail closed on an invalid value regardless of caller: NATS reads MaxAckPending<=0 as UNLIMITED, which
	// would silently defeat the serialization every current consumer depends on. resolveConsumeConfig already
	// clamps, but this is the function whose whole purpose is to be the single, isolable definition of the
	// consumer config — so a future second caller passing an unresolved 0 can never produce the unlimited-ack
	// posture here either.
	if maxAckPending <= 0 {
		maxAckPending = 1
	}
	return jetstream.ConsumerConfig{
		Durable:       consumerID,
		Name:          consumerID,
		FilterSubject: subj,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MaxDeliver:    DefaultMaxDeliver,
		AckWait:       jsAckWait,
		BackOff:       consumerBackoff(),
		MaxAckPending: maxAckPending,
	}
}

// NewJetStream dials/uses url (its own connection for now — a later wiring may share the transient
// bus's connection) and ensures the COMMS_TELL stream exists, returning a ready JetStream or an error
// (the caller degrades to DisabledJetStream). An empty url is a configuration "no JetStream" and is a
// caller decision (open() handles it), not an error here.
func NewJetStream(url string) (*NATSJetStream, error) {
	return dialJetStream(url, jsStreamName, DtellPrefix+">")
}

// NewScopeJetStream is the durable transport for the SCOPED EVENT BUS (Phase 10.2b): same machinery as
// the tell stream, bound to the WORLD_EVENTS stream over telos.scope.>. A director/zone wires this as
// the scopebus durable tier so a state-changing world event survives a restart. Empty url is a caller
// decision (degrade to DisabledJetStream), not an error here.
func NewScopeJetStream(url string) (*NATSJetStream, error) {
	return dialJetStream(url, jsScopeStreamName, ScopeSubjectPrefix+">")
}

// dialJetStream opens a connection and ensures the named stream over subjectFilter.
func dialJetStream(url, streamName, subjectFilter string) (*NATSJetStream, error) {
	nc, err := nats.Connect(url,
		nats.Timeout(connectTimeout),
		nats.Name("telos-commbus-jetstream"),
		nats.RetryOnFailedConnect(false),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("commbus: jetstream connect %q: %w", url, err)
	}
	return newJetStreamFromConn(nc, streamName, subjectFilter)
}

// newJetStreamFromConn builds the JetStream context + ensures the named stream (over subjectFilter, e.g.
// "telos.comms.dtell.>" or "telos.scope.>") on an existing connection. Split out so a future wiring can
// share the transient bus's *nats.Conn rather than dialing twice, and parameterized by stream so the
// tell stream and the scoped-event stream share one code path.
func newJetStreamFromConn(nc *nats.Conn, streamName, subjectFilter string) (*NATSJetStream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("commbus: jetstream new: %w", err)
	}
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       streamName,
		Subjects:   []string{subjectFilter},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		Duplicates: jsDedupWindow,
		MaxAge:     jsMaxAge,
	})
	if err != nil {
		return nil, fmt.Errorf("commbus: ensure stream %s: %w", streamName, err)
	}
	b := &NATSJetStream{
		js:     js,
		stream: stream,
		name:   streamName,
		log:    slog.With("component", "commbus", "transport", "jetstream", "stream", streamName),
		nc:     nc,
	}
	b.subscribeParkAdvisory()
	return b, nil
}

// jsMaxDeliveriesAdvisoryPrefix is the broker's per-consumer "message parked (max deliveries exhausted)"
// advisory subject prefix. The full subject is <prefix><stream>.<consumer>; the parkmon filters to this
// stream's consumers with a trailing ">" (#311).
const jsMaxDeliveriesAdvisoryPrefix = "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES."

// parkAdvisorySubject is the MAX_DELIVERIES advisory subject filtered to streamName's consumers.
func parkAdvisorySubject(streamName string) string {
	return jsMaxDeliveriesAdvisoryPrefix + streamName + ".>"
}

// parkAdvisoryQueue is the SHARED queue-group name the parkmon subscribes under, so a park is counted
// EXACTLY ONCE cluster-wide. Every process running a consumer on this stream (every world shard for
// COMMS_TELL; every director for WORLD_EVENTS) subscribes to the same stream-wide advisory subject — a
// plain subscription would fan the advisory to ALL of them and multiply the count by the process count.
// A queue group makes the broker deliver each advisory to exactly one member.
func parkAdvisoryQueue(streamName string) string {
	return "telos-parkmon-" + streamName
}

// maxDeliveriesAdvisory is the subset of the broker's io.nats.jetstream.advisory.v1.max_deliveries payload
// the parkmon reads (for the log; the counter is labeled by stream regardless).
type maxDeliveriesAdvisory struct {
	Stream     string `json:"stream"`
	Consumer   string `json:"consumer"`
	StreamSeq  uint64 `json:"stream_seq"`
	Deliveries int    `json:"deliveries"`
}

// parkObserver is the park test-seam signature (see parkAdvisoryObserver).
type parkObserver func(streamName, consumer string, streamSeq uint64)

// parkAdvisoryObserver, when set, is invoked for every MAX_DELIVERIES advisory the parkmon processes — a
// TEST SEAM so the gated broker test can assert the park path fired (the OTel counter itself is not readily
// observable in a test). Unset (nil) in production. An atomic.Pointer, not a bare var, because it is READ on
// the NATS delivery goroutine (handleParkAdvisory) while a gated test WRITES it from setup/cleanup — a plain
// package var would be an unsynchronized read/write under the race detector.
var parkAdvisoryObserver atomic.Pointer[parkObserver]

// subscribeParkAdvisory wires the park counter off the broker's MAX_DELIVERIES advisory (#311). The
// handler-side detection (in Consume) only fires when the MaxDeliver-th delivery lands in THIS process and
// the handler runs — it misses a park via AckWait-expiry (a hung/lost-ack consumer) or across a restart. The
// advisory fires for EVERY park regardless of which redelivery path exhausted the budget, so it is the
// authoritative signal for WHICH parks happen and thus the right source for durable_parked_total (a park is
// permanent loss — the counter is the alert route).
//
// It is an ALERT counter, not an exact one: the advisory is an ephemeral core-NATS message (no broker
// buffering for a disconnected subscription), so a park published while NO group member is connected is not
// replayed. The QUEUE GROUP — not the auto-resubscribe — is what bounds that window: the broker balances
// each advisory to one CONNECTED member, so a single process reconnecting is invisible (another member takes
// it); the miss window shrinks to "every member down simultaneously" (a full-fleet outage at the instant of
// a park), which is negligible at fleet scale. Never fatal: a subscribe failure is logged and leaves the
// per-park ERROR log (in Consume) as the operator backstop; the counter degrades, nothing crashes.
func (b *NATSJetStream) subscribeParkAdvisory() {
	if b.nc == nil {
		return
	}
	subj := parkAdvisorySubject(b.name)
	sub, err := b.nc.QueueSubscribe(subj, parkAdvisoryQueue(b.name), func(m *nats.Msg) {
		b.handleParkAdvisory(m.Data)
	})
	if err != nil {
		b.log.Warn("commbus: could not subscribe to the MAX_DELIVERIES park advisory; park COUNTING is degraded "+
			"(the per-park ERROR log still fires for in-process parks)", "subject", subj, "err", err)
		return
	}
	b.parkSub = sub
	b.log.Debug("commbus: park advisory monitor subscribed", "subject", subj, "queue", parkAdvisoryQueue(b.name))
}

// handleParkAdvisory counts one parked message off the broker advisory (#311) and logs it as the incident it
// is (permanent loss). Runs on a NATS client goroutine. A malformed payload still counts the park (the
// advisory subject alone proves a park happened) but logs without the seq/consumer detail.
func (b *NATSJetStream) handleParkAdvisory(data []byte) {
	var adv maxDeliveriesAdvisory
	if err := json.Unmarshal(data, &adv); err != nil {
		// b.log already carries the stream field (slog.With at construction), so it is not repeated here.
		b.log.Error("durable message PARKED (max deliveries) — it is LOST; advisory payload unparseable",
			"err", err)
		metrics.DurableParked(context.Background(), b.name)
		notifyParkObserver(b.name, "", 0)
		return
	}
	b.log.Error("durable message PARKED (max deliveries) — it is LOST (authoritative broker advisory)",
		"consumer", adv.Consumer, "stream_seq", adv.StreamSeq, "deliveries", adv.Deliveries)
	metrics.DurableParked(context.Background(), b.name)
	notifyParkObserver(b.name, adv.Consumer, adv.StreamSeq)
}

// notifyParkObserver invokes the test seam if one is set (see parkAdvisoryObserver).
func notifyParkObserver(streamName, consumer string, streamSeq uint64) {
	if obs := parkAdvisoryObserver.Load(); obs != nil {
		(*obs)(streamName, consumer, streamSeq)
	}
}

// PublishDurable publishes msg on subj with the Nats-Msg-Id header set to msg.IdempotencyKey, so the
// broker dedups a re-publish within the Duplicates window (the publish-side layer). An empty key
// would defeat dedup — the world tell path ALWAYS sets it. At-least-once: the PubAck confirms the
// message is durably stored before this returns, so a target logging out after this point still gets
// it on next login.
func (b *NATSJetStream) PublishDurable(ctx context.Context, subj string, msg Message) error {
	msg.Subject = subj
	data, err := msg.marshal()
	if err != nil {
		return fmt.Errorf("commbus: marshal durable message: %w", err)
	}
	m := &nats.Msg{Subject: subj, Data: data, Header: nats.Header{}}
	if msg.IdempotencyKey != "" {
		m.Header.Set(jetstream.MsgIDHeader, msg.IdempotencyKey)
	}
	if _, err := b.js.PublishMsg(ctx, m); err != nil {
		return fmt.Errorf("commbus: durable publish %s: %w", subj, err)
	}
	b.log.Debug("durable tell published", "subject", subj, "author", msg.AuthorID, "seq", msg.Seq)
	return nil
}

// Consume runs a per-player durable consumer filtered to subj (the player's dtell.<id> subject).
// consumerID is the durable name (stable per player so a restart resumes from the last ack). handler
// returns an AckDecision: AckDelivered acks; DropPoison acks + logs (an undeliverable message never
// spends the retry budget); RetryTransient NAKs it WITH the NakBackoff delay (#266 — a bare Nak()
// redelivers immediately, which burned the whole budget inside a transient window and parked the tell).
//
// MaxAckPending=1 is load-bearing, not tuning: a delayed NAK must not let a successor be delivered and
// advance the world's per-sender delivered-cursor past the pending message, which would suppress it as a
// duplicate on redelivery and lose it (see jetstream.go's ordering note). BackOff paces the OTHER
// redelivery path (an AckWait expiry from a hung/lost-ack handler) on the same schedule; NATS applies it
// there but NOT to an explicit NAK, which is why NakWithDelay is required below. len(BackOff) <=
// MaxDeliver is a NATS constraint; past the schedule's end the last entry repeats.
//
// The consumer delivers the backlog (DeliverAll) then live, in stream order.
func (b *NATSJetStream) Consume(subj, consumerID string, handler func(Message, bool) AckDecision, opts ...ConsumeOption) (Consumer, error) {
	cfg := resolveConsumeConfig(opts...)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	cons, err := b.stream.CreateOrUpdateConsumer(ctx, durableConsumerConfig(subj, consumerID, cfg.MaxAckPending))
	if err != nil {
		return nil, fmt.Errorf("commbus: ensure consumer %s: %w", consumerID, err)
	}
	// backlogEdge is the stream sequence of the last message already stored when this consumer started
	// (the offline catch-up boundary): a delivered message at/below it is BACKLOG ("while you were
	// away…"), above it is LIVE. We read it once from the consumer's pending count at start. A zero edge
	// (no backlog) makes every delivery live.
	var backlogEdge uint64
	if info, ierr := cons.Info(ctx); ierr == nil && info != nil {
		backlogEdge = info.Delivered.Stream + info.NumPending // last stored seq = delivered + still-pending
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		msg, err := unmarshalMessage(m.Data())
		if err != nil {
			b.log.Warn("dropping malformed durable message (poison)", "subject", m.Subject(), "err", err)
			metrics.DurablePoisoned(context.Background(), b.name)
			_ = m.Ack() // poison: ack it so it doesn't redeliver forever, and it spends no retry budget
			return
		}
		msg.Subject = m.Subject()
		backlog := false
		attempt := 1
		if meta, merr := m.Metadata(); merr == nil {
			backlog = meta.Sequence.Stream <= backlogEdge
			// Clamp before narrowing. NumDelivered is a small positive counter (the broker parks at
			// MaxDeliver), and the schedule holds at its last entry past the end, so a larger count carries
			// no information — the clamp only makes the narrowing provably safe.
			nd := meta.NumDelivered
			if nd > maxAttemptClamp {
				nd = maxAttemptClamp
			}
			attempt = int(nd) //nolint:gosec // clamped to maxAttemptClamp (< MaxInt32) on the line above
		}
		switch handler(msg, backlog) {
		case AckDelivered:
			_ = m.Ack()
		case DropPoison:
			b.log.Warn("dropping undeliverable durable message (poison)",
				"subject", m.Subject(), "author", msg.AuthorID, "seq", msg.Seq)
			metrics.DurablePoisoned(context.Background(), b.name)
			_ = m.Ack()
		default: // RetryTransient
			// The final attempt failing means the broker will PARK this message — permanent loss, since
			// the durable consumer is never deleted and so never replays it. That is an incident, not a
			// routine drop: surface it loudly with the author/seq context only this in-process path has.
			// The COUNTER (durable_parked_total), however, is owned by the broker MAX_DELIVERIES advisory
			// (subscribeParkAdvisory, #311), NOT incremented here — this path is best-effort (it misses an
			// AckWait-expiry or across-restart park), and double-counting the advisory-visible in-process
			// park would corrupt the count. So: rich log here, authoritative count there.
			if attempt >= DefaultMaxDeliver {
				b.log.Error("durable message PARKED after exhausting the redelivery budget — it is LOST",
					"subject", m.Subject(), "author", msg.AuthorID, "seq", msg.Seq,
					"attempts", attempt, "window", totalNakWindow())
			}
			_ = m.NakWithDelay(nakBackoff(attempt))
		}
	})
	if err != nil {
		return nil, fmt.Errorf("commbus: consume %s: %w", consumerID, err)
	}
	b.log.Debug("durable tell consumer started", "subject", subj, "consumer", consumerID)
	return &natsConsumer{cc: cc}, nil
}

// Close is a logical close: it does not close the shared NATS connection (the transient bus owns it). It
// DOES tear down the park advisory subscription (#311) so a stopped handle stops counting (a fresh handle
// subscribes its own parkmon at construction). Idempotent.
func (b *NATSJetStream) Close() error {
	if b.parkSub != nil {
		_ = b.parkSub.Unsubscribe()
		b.parkSub = nil
	}
	return nil
}

// natsConsumer adapts a jetstream.ConsumeContext to the Consumer interface.
type natsConsumer struct{ cc jetstream.ConsumeContext }

func (c *natsConsumer) Stop() error {
	if c.cc != nil {
		c.cc.Stop()
	}
	return nil
}

// OpenJetStream is the optional/never-fatal wiring helper (mirrors OpenWorld/OpenGate): it dials url
// and, on failure or an empty url, logs via logf and returns DisabledJetStream() so boot never fails
// on an unreachable broker. The world wiring uses this for the durable-tell handle.
func OpenJetStream(url string, logf func(err error)) JetStream {
	if url == "" {
		if logf != nil {
			logf(nil)
		}
		return DisabledJetStream()
	}
	js, err := NewJetStream(url)
	if err != nil {
		if logf != nil {
			logf(err)
		}
		return DisabledJetStream()
	}
	return js
}

// OpenScopeJetStream is OpenJetStream for the scoped-event stream (the scopebus durable tier): never
// fatal — an empty/unreachable url degrades to DisabledJetStream so a director runs without durable
// orchestration rather than failing boot.
func OpenScopeJetStream(url string, logf func(err error)) JetStream {
	if url == "" {
		if logf != nil {
			logf(nil)
		}
		return DisabledJetStream()
	}
	js, err := NewScopeJetStream(url)
	if err != nil {
		if logf != nil {
			logf(err)
		}
		return DisabledJetStream()
	}
	return js
}

// Compile-time assertions.
var (
	_ JetStream = (*NATSJetStream)(nil)
	_ Consumer  = (*natsConsumer)(nil)
)
