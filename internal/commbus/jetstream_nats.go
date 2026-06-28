package commbus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

// NATSJetStream is the JetStream-backed durable transport. It owns the JetStream context (over a NATS
// connection it does NOT own — the connection is shared with the transient bus in production, so Close
// here only stops the consumers, it does not close the connection).
type NATSJetStream struct {
	js     jetstream.JetStream
	stream jetstream.Stream
	log    *slog.Logger
}

// NewJetStream dials/uses url (its own connection for now — a later wiring may share the transient
// bus's connection) and ensures the COMMS_TELL stream exists, returning a ready JetStream or an error
// (the caller degrades to DisabledJetStream). An empty url is a configuration "no JetStream" and is a
// caller decision (open() handles it), not an error here.
func NewJetStream(url string) (*NATSJetStream, error) {
	nc, err := nats.Connect(url,
		nats.Timeout(connectTimeout),
		nats.Name("telos-commbus-jetstream"),
		nats.RetryOnFailedConnect(false),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return nil, fmt.Errorf("commbus: jetstream connect %q: %w", url, err)
	}
	return newJetStreamFromConn(nc)
}

// newJetStreamFromConn builds the JetStream context + ensures the stream over an existing connection.
// Split out so a future wiring can share the transient bus's *nats.Conn rather than dialing twice.
func newJetStreamFromConn(nc *nats.Conn) (*NATSJetStream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("commbus: jetstream new: %w", err)
	}
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       jsStreamName,
		Subjects:   []string{DtellPrefix + ">"},
		Storage:    jetstream.FileStorage,
		Retention:  jetstream.LimitsPolicy,
		Duplicates: jsDedupWindow,
		MaxAge:     jsMaxAge,
	})
	if err != nil {
		return nil, fmt.Errorf("commbus: ensure stream %s: %w", jsStreamName, err)
	}
	return &NATSJetStream{js: js, stream: stream, log: slog.With("component", "commbus", "transport", "jetstream")}, nil
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
// returns ack=true (Ack) or ack=false (Nak — redelivered with backoff up to MaxDeliver, then parked).
// The consumer delivers the backlog (DeliverAll) then live messages, in stream order.
func (b *NATSJetStream) Consume(subj, consumerID string, handler func(Message, bool) bool) (Consumer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	cons, err := b.stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       consumerID,
		Name:          consumerID,
		FilterSubject: subj,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		MaxDeliver:    DefaultMaxDeliver,
		AckWait:       jsAckWait,
	})
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
			b.log.Debug("dropping malformed durable tell", "subject", m.Subject(), "err", err)
			_ = m.Ack() // a malformed message is poison; ack it so it doesn't redeliver forever
			return
		}
		msg.Subject = m.Subject()
		backlog := false
		if meta, merr := m.Metadata(); merr == nil {
			backlog = meta.Sequence.Stream <= backlogEdge
		}
		if handler(msg, backlog) {
			_ = m.Ack()
			return
		}
		_ = m.Nak()
	})
	if err != nil {
		return nil, fmt.Errorf("commbus: consume %s: %w", consumerID, err)
	}
	b.log.Debug("durable tell consumer started", "subject", subj, "consumer", consumerID)
	return &natsConsumer{cc: cc}, nil
}

// Close is a logical close: it does not close the shared NATS connection (the transient bus owns it).
func (b *NATSJetStream) Close() error { return nil }

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

// Compile-time assertions.
var (
	_ JetStream = (*NATSJetStream)(nil)
	_ Consumer  = (*natsConsumer)(nil)
)
