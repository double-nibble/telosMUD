package world

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/textsan"
)

// tell.go is the SOURCE-WORLD half of Phase-8 tells (docs/PHASE8-PLAN.md slice 8.5, OQ-1 = DURABLE-
// ALWAYS, OQ-4 = world-drains + cursor-in-character-state). Tells are ENGINE mechanism (not content):
// every tell is a JetStream durable message, and "online delivery" is just a fast durable consumer
// being live — there is NO separate NATS-core online path, which eliminates the online->offline logout
// race (a tell to a player logging out is never lost). The flow:
//
//	tell <name> <msg>
//	 └─ SENDER world: resolve target via directory.PlayerShard (the EPOCH-AUTHORITATIVE map, NEVER
//	    presence — P8-A4); a resolve MISS refuses the tell to the sender ("no player by that name").
//	 └─ sanitize (textsan, P8-A7), ENGINE-SET author from the live *Entity (P8-A2), per-author Seq
//	    (P8-A3), ALWAYS-set IdempotencyKey ("<author>:<seq>").
//	 └─ PublishDurable to telos.comms.dtell.<targetPlayerId> (the durable stream).
//	 └─ TARGET world: a per-player durable consumer drains dtell.<target> (backlog on login, then
//	    live), dedups via the per-player delivered-cursor in character state JSONB (OQ-4), renders, and
//	    EMITS each to commbus.TellSubject(target) over the RoleWorld transient bus so the GATE (sink,
//	    already subscribed from 8.2) renders it. Online => near-real-time; offline => the backlog is
//	    drained PACED on next login ("while you were away…").
//
// # Where each obligation is enforced
//
//   - ROUTE-VIA-DIRECTORY: tellTarget reads z.dir().PlayerShard, never z.shard.presence (P8-A4).
//   - SANITIZE: cmdTell runs textsan.CleanLine on the body (P8-A7), same as channels.
//   - ENGINE-AUTHOR: AuthorID/AuthorName from s.character / the live *Entity, never a client field
//     (P8-A2); Seq from the server-held commSource counter (commNextSeq), the same monotonic source
//     channels use, so a sender's tells AND channel lines share one per-author order.
//   - IDEMPOTENCY-KEY: ALWAYS set (commbus.NewIdempotencyKey) — the MemJetStream cannot dedup an empty
//     key; the publish-side dedup window + the consumer-side delivered-cursor are the two P8-A5 layers.
//   - DELIVERED-CURSOR: the drain renders a message only when its Seq strictly exceeds the per-sender
//     cursor (session.tellCursor, mirrored to StateJSON.Tells), then advances it — the consumer-side
//     idempotency layer atop JetStream's at-least-once delivery.
//
// # The durability guarantee (precise — NOT literal exactly-once)
//
// A durable tell is RENDER-AT-LEAST-ONCE / USUALLY-EXACTLY-ONCE / NEVER-LOST:
//
//   - NEVER LOST: the tell is durable from PublishDurable until acked, so a target logging out (or a
//     shard crashing) between publish and delivery still receives it on next login — there is no core
//     path on which to drop it (durable-always, OQ-1).
//   - EXACTLY ONCE in steady state: the strictly-greater cursor gate suppresses any redelivery whose
//     Seq is <= the stored cursor (the JetStream dedup window covers recent redeliveries; the persisted
//     cursor covers redeliveries after that window).
//   - The ONE exception: a crash in the NARROW window between the gate emit and the next PERSISTENCE of
//     the advanced cursor (the cursor advances in memory immediately but rides the save cadence to
//     durable storage). On restart that single tell can re-render ONCE before the re-advanced cursor
//     re-suppresses it. This is BOUNDED — the cursor re-advances and re-suppresses; it is never a loop
//     and never a storm. It is therefore at-least-once with usually-exactly-once rendering, not a
//     literal exactly-once durable guarantee.

// loginDrainPace caps how fast the login-time backlog drain emits "while you were away…" tells to a
// freshly-joined player (P8-A5 backlog pacing): one tell per loginDrainPace so a long backlog does not
// flood the socket in a single burst. The LIVE path (a tell to an online player) is not paced — only
// the initial backlog catch-up. A package var so a test can shrink it.
var loginDrainPace = 50 * time.Millisecond

// tellDeliverMsg carries one drained durable tell from the off-zone consumer goroutine TO the zone
// goroutine, which owns the target session + its delivered-cursor (single-writer). The consumer blocks
// on ack until the zone replies whether the message was handled (rendered or idempotently suppressed —
// both ack) or could not be (no such resident right now — NAK so it redelivers, bounded by maxDeliver).
type tellDeliverMsg struct {
	target  string          // the resident player id this tell is for
	msg     commbus.Message // the durable tell (engine-set author, seq, body); msg.Trace carries the producer link
	backlog bool            // true => an OFFLINE catch-up tell (rendered "while you were away…")
	ack     chan bool       // the zone replies true=ack (handled/suppressed), false=nak (retry)
	// enqueued is when the off-zone consumer POSTED this to the inbox (#467). The zone reads it at DEQUEUE to
	// record inbox queue-wait — precisely the saturation signal at the hot-zone one-core ceiling. It carries
	// an immutable timestamp + msg.Trace (a serialized SpanContext), NEVER a cancellable context, so a message
	// handled after its originating stream was cancelled cannot outlive a dead parent.
	enqueued time.Time
}

func (tellDeliverMsg) zoneMsg() {}

// --- The sender side: the tell / reply commands -----------------------------------------------

// cmdTell is the `tell <name> <msg>` source path. It resolves the target via the directory, sanitizes,
// stamps the engine-set author + seq + idempotency key, and PublishDurable's to the target's durable
// subject. Runs on the zone goroutine via dispatch; the directory read + the durable publish are
// blocking I/O, so they run on a short-lived goroutine OFF the zone loop (the cmdWho/login-read
// discipline) and the result is reported back to the sender's out channel. It never releases ownership.
func (z *Zone) cmdTell(s *session, rest string) {
	name, body := split(rest)
	if name == "" || strings.TrimSpace(body) == "" {
		s.send(textFrame("Tell whom what?"))
		return
	}
	z.sendTell(s, name, body)
}

// cmdReply is `reply <msg>`: a tell to the last player who told YOU (session.lastTellFrom). No prior
// tell => nothing to reply to.
func (z *Zone) cmdReply(s *session, body string) {
	if strings.TrimSpace(body) == "" {
		s.send(textFrame("Reply what?"))
		return
	}
	if s.lastTellFrom == "" {
		s.send(textFrame("You have no one to reply to."))
		return
	}
	z.sendTell(s, s.lastTellFrom, body)
}

// sendTell is the shared publish path for tell + reply. targetName is the resolved-by-name target (the
// player id == the login name today, OQ-5). It captures the engine-set author + sanitized body ON the
// zone goroutine (single-writer reads of the live entity), then ENQUEUES the tell to the shard's
// single-writer durable-tell PUBLISHER (comm.go), which resolves + assigns the per-author seq +
// publishes IN FIFO ORDER. Routing the seq-assign + publish through one ordered worker (not a goroutine
// per tell) is what guarantees PER-SENDER ORDER (P8-A3): two quick tells from one sender are appended to
// the durable stream in send order, so the drain delivers — and the delivered-cursor advances — in
// order. (A goroutine-per-tell would race the seq-assign + append and could reorder, which the cursor
// would then mis-suppress.)
func (z *Zone) sendTell(s *session, targetName, body string) {
	cs := z.commSourceOrNil()
	if cs == nil || z.commsBus() == nil || z.tellJS() == nil {
		s.send(textFrame("Tells are temporarily offline."))
		return
	}
	actor := s.entity
	if actor == nil {
		return
	}

	// SANITIZE THE TARGET TOKEN (P8-A8, subject-injection — security LOW-1). targetName becomes BOTH a
	// directory key (PlayerShard) AND, via DtellSubject, a NATS subject token. CleanName strips control/
	// non-graphic runes + caps the length (the world-ingress name discipline), and then we REJECT any
	// NATS subject metacharacter — a dot (token separator), '*'/'>' (wildcards), or whitespace. Without
	// this, the dir==nil branch (single-shard/bare run, which publishes WITHOUT a directory existence
	// check) would let an attacker-chosen "Bob.>"-style token reach the broker as a crafted subject. The
	// gRPC ingress already CleanLine's input, so this is defense-in-depth that does NOT rely on the
	// directory existence check being present (the P8-A8 subject defense stands on its own here).
	target := safeTellTarget(targetName)
	if target == "" {
		s.send(textFrame("There is no player by that name."))
		return
	}

	// RATE-LIMIT (P8-A1 — security LOW-2): tells are rate-limited per AUTHOR with the SAME token bucket
	// channels use (commRateOK), enforced HERE on the zone goroutine BEFORE the tell reaches the durable
	// stream. An over-limit tell is dropped with a SENDER-ONLY notice — it never reaches the publisher,
	// the stream, or the target, so a flood throttles the sender and cannot inflate a victim's durable
	// backlog / login-drain or degrade any other player. Channels and tells share one per-author budget
	// (both are player-authored comms from the same entity).
	if !z.commRateOK(s.character) {
		s.send(textFrame("You are sending tells too fast."))
		return
	}

	// ENGINE-SET author (P8-A2): the LIVE entity, never a client field. SANITIZE (P8-A7): the body is
	// DATA — CleanLine strips control/ANSI/IAC so the render can never forge a prefix; the same
	// sanitizer the channel + input paths use. The Seq is assigned LATER, in the FIFO publisher, so it
	// is monotonic in publish order (the order the stream is appended).
	clean := textsan.CleanLine(strings.TrimSpace(body))
	cs.enqueueTell(tellJob{
		authorID:   s.character,
		authorName: actor.Name(),
		target:     target,
		body:       clean,
		out:        s.out,
		dir:        z.dir(),
		log:        z.log,
	})

	// Capture the SENT side into the in-session tell ring (#349, tellhistory.go). This is OPTIMISTIC and
	// captures at enqueue time, on the zone goroutine, BEFORE the async publisher (publishOne, comm.go) runs
	// its directory existence check + echo. KNOWN DIVERGENCE (accepted for slice 1): on a resolve-miss
	// ("no player by that name") or a publish failure ("tells temporarily offline"), publishOne shows the
	// sender an ERROR and never emits the "You tell X" echo — yet this entry is already in the ring, so
	// `tells` then shows a "You told X" line the sender never actually saw. It is the sender's OWN ring
	// (no privacy leak), and the resolve-miss already reveals non-existence via the error, so the wart is
	// cosmetic. The clean fix — record at publishOne's confirmed-echo point — needs the session/ring threaded
	// onto tellJob and a post back to the zone goroutine (session state is zone-owned); deferred as a
	// follow-up. The sender lacks the target's live *Entity, so the display name falls back to the
	// (sanitized) target id, which is also what a successful live echo shows. Zone goroutine, single-writer.
	s.recordTell(tellLogEntry{outbound: true, other: target, otherName: target, body: clean, at: time.Now()})
}

// safeTellTarget sanitizes a player-supplied tell target into a token safe to use as BOTH a directory
// key and a NATS subject token (P8-A8, subject-injection — security LOW-1). It CleanName's the token
// (control/non-graphic strip + length cap, the world-ingress name discipline) and then rejects any NATS
// subject metacharacter — a dot ('.', the token separator), a wildcard ('*'/'>'), or whitespace — by
// returning "" (the caller refuses the tell). It does NOT mutate a valid token (a relog/reply target
// passes through unchanged); it only refuses a crafted one, so the subject space can never receive an
// attacker-chosen extra token even on the dir==nil (no-directory-existence-check) path.
func safeTellTarget(name string) string {
	clean := textsan.CleanName(strings.TrimSpace(name), maxPlayerNameRunes)
	if clean == "" {
		return ""
	}
	if strings.ContainsAny(clean, ".*>") || strings.ContainsAny(clean, " \t\r\n") {
		return "" // a subject metacharacter / whitespace: refuse rather than inject into the subject space
	}
	return clean
}

// --- The target side: the resident durable consumer + the drain ------------------------------

// startTellConsumer starts (or no-ops if already running) the per-player durable consumer for a
// resident this shard now hosts (called on join / handoff arrival). The consumer drains the player's
// durable dtell backlog (then live messages) on a bus goroutine and posts each to the player's CURRENT
// zone via tellDeliverMsg, gating its JetStream ack on the zone's render-or-suppress result. Keyed per
// player on the shard's commSource so a player walking zone->zone within this shard keeps ONE consumer
// (the route func follows them via the session's currentZone atomic pointer). Called from a zone
// goroutine; the Consume call may do broker I/O (bounded inside the commbus impl).
func (z *Zone) startTellConsumer(s *session) {
	cs := z.commSourceOrNil()
	if cs == nil || cs.js == nil || s == nil {
		return
	}
	playerID := s.character
	startZone := z
	curZone := s.currentZone
	// route resolves the zone CURRENTLY hosting the player: the session's currentZone atomic pointer
	// (updated by every move/handoff) when present, else the zone that started the consumer (a bare
	// test session has no currentZone). This is the ONLY cross-goroutine read the consumer makes, and
	// it is an atomic load — never a read of zone-owned state.
	route := func() *Zone {
		if curZone != nil {
			if z := curZone.Load(); z != nil {
				return z
			}
		}
		return startZone
	}
	cs.startConsumer(playerID, route)
}

// stopTellConsumer tears down a resident's durable consumer (clean quit/leave, or a handed-off orphan
// reap). The player is no longer hosted here, so their durable tells should accumulate in the stream
// for their next host to drain — not be delivered to a socket that is gone.
func (z *Zone) stopTellConsumer(playerID string) {
	cs := z.commSourceOrNil()
	if cs == nil {
		return
	}
	cs.stopConsumer(playerID)
}

// deliverDrainedTell handles one drained durable tell ON the zone goroutine (single-writer over the
// target session + cursor). It returns whether the consumer should ACK:
//
//   - target not currently a resident here (raced a leave/handoff): NAK (false) so it redelivers,
//     bounded by maxDeliver — the new host's consumer will pick it up. A poison case (the player is
//     simply gone for good) parks after maxDeliver rather than storming.
//   - Seq <= the per-sender delivered-cursor: a REDELIVERY after the dedup window — suppress (render
//     nothing) but ACK (true): it is already delivered, advancing nothing. This is the consumer-side
//     suppression layer (P8-A5) — exactly-once in steady state; see the header for the bounded
//     re-render-once-on-crash exception.
//   - otherwise: render + emit to the gate via the transient bus, advance the cursor, set lastTellFrom
//     for `reply`, and ACK.
//
// deliverTellTraced wraps deliverDrainedTell in the zone-side tell-delivery span (#467, zone-mailbox half).
// The span STARTS HERE — on the zone goroutine at DEQUEUE, not when the message was enqueued — so a message
// DROPPED before it is dequeued (postOrDrop under load) never starts a span at all: there is no orphan by
// construction, and this is why a started-never-ended span cannot happen on the shed path. It LINKS to the
// producer that published the tell (bounded, baggage-free via commbus.ProducerLink) rather than parenting to
// it, and records inbox queue-wait (now − enqueued) — the hot-zone saturation signal. The span's ctx is
// threaded into the render+emit so the gate-delivery publish continues this trace instead of rooting anew.
func (z *Zone) deliverTellTraced(m tellDeliverMsg) bool {
	ctx, span := tracer().Start(context.Background(), "zone.deliver_tell",
		trace.WithLinks(commbus.ProducerLink(m.msg)),
		trace.WithAttributes(
			attribute.String("telos.zone", z.metricZone()), // template, never the player-mintable instance id (#470)
			attribute.Bool("telos.bus.backlog", m.backlog),
		),
	)
	if !m.enqueued.IsZero() {
		span.SetAttributes(attribute.Int64("telos.zone.queue_wait_ms", time.Since(m.enqueued).Milliseconds()))
	}
	defer span.End()
	return z.deliverDrainedTell(ctx, m)
}

func (z *Zone) deliverDrainedTell(ctx context.Context, m tellDeliverMsg) bool {
	s := z.players[m.target]
	if s == nil {
		return false // not ours right now: NAK -> redeliver (bounded); the player's real host drains it
	}
	author := m.msg.AuthorID
	if s.tellCursor == nil {
		s.tellCursor = map[string]uint64{}
	}
	if m.msg.Seq <= s.tellCursor[author] {
		// Already delivered (a redelivery <= the cursor): suppress but ack — steady-state exactly-once.
		z.log.Debug("durable tell suppressed (already delivered)", "to", m.target, "from", author, "seq", m.msg.Seq)
		return true
	}

	// Render + EMIT to the gate (the sink) over the transient bus on the target's concrete tell subject
	// (commbus.TellSubject) — the SAME subject the gate subscribed in 8.2, so it renders. The world is
	// the source; the gate stays a pure sink. The body is already sanitized (the sender side cleaned it).
	// An OFFLINE backlog tell (drained on login) gets the "while you were away…" wording; a live tell
	// gets the present-tense form.
	var line string
	if m.backlog {
		line = "While you were away, " + m.msg.AuthorName + " told you, '" + m.msg.Body + "'"
	} else {
		line = m.msg.AuthorName + " tells you, '" + m.msg.Body + "'"
	}
	if err := z.commsBus().Publish(ctx, commbus.TellSubject(m.target), commbus.Message{
		AuthorID:   m.msg.AuthorID,
		AuthorName: m.msg.AuthorName,
		Seq:        m.msg.Seq,
		Body:       line,
	}); err != nil {
		// Emit failed (a closed/disabled bus): NAK so it redelivers; the player still gets it once the
		// bus recovers. Never lose the tell.
		z.log.Debug("durable tell emit failed; will redeliver", "to", m.target, "err", err)
		return false
	}

	s.tellCursor[author] = m.msg.Seq // advance the per-sender cursor (rides StateJSON.Tells on save)
	s.lastTellFrom = author          // the `reply` target
	z.log.Debug("durable tell delivered", "to", m.target, "from", author, "seq", m.msg.Seq)

	// Capture the RECEIVED side into the in-session tell ring (#349, tellhistory.go), for BOTH a live tell and
	// a backlog (offline catch-up) drain — a backlog tell was really received, so it belongs in the history.
	// But SKIP a tell whose author this player IGNORES: the ignore funnel drops it at the GATE before the
	// player ever sees it (the world emits unfiltered above), so recording it would put a line in the history
	// the player never actually saw. Skipping here keeps the ring == what was rendered to the socket. The body
	// is the sanitized delivered body (m.msg.Body), not the framed render line (that prefix is re-added by the
	// history render). Zone goroutine, single-writer.
	if !s.comms.ignored(author) {
		s.recordTell(tellLogEntry{outbound: false, other: author, otherName: m.msg.AuthorName, body: m.msg.Body, at: time.Now()})
	}

	// AFK auto-reply (Phase 8.6): a LIVE tell to an AFK target sends a one-line "X is AFK: <msg>" back to
	// the SENDER over their concrete tell subject — so the sender learns the target is away. NOT for a
	// backlog (offline catch-up) drain: those are old tells whose senders should not get a stale
	// auto-reply now. The reply is itself a transient tell on commbus.TellSubject(author); a disabled bus
	// makes it a clean no-op. The sender's gate renders it like any tell (and the sender's own ignore
	// funnel applies — an auto-reply from a player they ignore is still dropped at their gate).
	if !m.backlog {
		if reply := afkAutoReply(s); reply != "" {
			_ = z.commsBus().Publish(context.Background(), commbus.TellSubject(author), commbus.Message{
				AuthorID:   m.target, // the AFK player is the author of the auto-reply
				AuthorName: s.entity.Name(),
				Body:       reply,
			})
		}
	}
	return true
}

// --- Cursor persistence (OQ-4: the delivered-cursor in character state JSONB) -----------------

// dumpTellCursor renders the session's in-memory delivered-cursor into its durable form (Phase 8.5,
// OQ-4). Empty/nil when the player has received no tells (the common case + the backward-compat
// default). Size-guarded: at most tellCursorMaxSenders senders are kept (a dropped old sender at worst
// risks one re-render of a long-aged tell). Runs on the zone goroutine; copies into a fresh map so the
// saver never aliases live session state.
func dumpTellCursor(s *session) *TellCursorJSON {
	if s == nil || len(s.tellCursor) == 0 {
		return nil
	}
	out := make(map[string]uint64, len(s.tellCursor))
	for id, seq := range s.tellCursor {
		out[id] = seq
	}
	out = capTellCursor(out)
	return &TellCursorJSON{Delivered: out}
}

// loadTellCursor installs a persisted delivered-cursor onto the session (Phase 8.5, OQ-4). A nil/empty
// cursor (a pre-8.5 save or a player who never received a tell) installs nothing — the cursor is then
// lazily created on first delivery. Runs on the zone goroutine.
func loadTellCursor(s *session, c *TellCursorJSON) {
	if s == nil || c == nil || len(c.Delivered) == 0 {
		return
	}
	s.tellCursor = make(map[string]uint64, len(c.Delivered))
	for id, seq := range c.Delivered {
		s.tellCursor[id] = seq
	}
}

// capTellCursor trims the cursor to tellCursorMaxSenders, keeping the senders with the HIGHEST seqs (a
// rough "most-recently-active" proxy — the cursor is a dedup optimization, not a ledger). Returns the
// input unchanged when under cap.
func capTellCursor(m map[string]uint64) map[string]uint64 {
	if len(m) <= tellCursorMaxSenders {
		return m
	}
	// Find the seq threshold that keeps the top tellCursorMaxSenders by a simple selection: collect
	// seqs, sort descending, keep keys at/above the cutoff. Bounded work (only on an over-cap save).
	seqs := make([]uint64, 0, len(m))
	for _, v := range m {
		seqs = append(seqs, v)
	}
	// Partial: find the (tellCursorMaxSenders)-th largest via sort (cap rarely hit; clarity over speed).
	sortDescUint64(seqs)
	cutoff := seqs[tellCursorMaxSenders-1]
	out := make(map[string]uint64, tellCursorMaxSenders)
	for k, v := range m {
		if v >= cutoff && len(out) < tellCursorMaxSenders {
			out[k] = v
		}
	}
	return out
}

func sortDescUint64(a []uint64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] > a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// tellCursorProbeMsg / lastTellProbeMsg are synchronous read-backs of a resident's durable-tell
// delivered-cursor and last-tell-sender, answered ON the zone goroutine (single-writer). They exist so
// an observer (a test, or a future ops/debug command) can read this zone-owned state race-free via the
// inbox rather than touching the session from another goroutine. They mutate nothing.
type tellCursorProbeMsg struct {
	id     string
	author string
	reply  chan uint64
}

type lastTellProbeMsg struct {
	id    string
	reply chan string
}

func (tellCursorProbeMsg) zoneMsg() {}
func (lastTellProbeMsg) zoneMsg()   {}

// probeTellCursor answers tellCursorProbeMsg: the delivered Seq for (player, author), or 0 if unknown.
func (z *Zone) probeTellCursor(m tellCursorProbeMsg) {
	var v uint64
	if s := z.players[m.id]; s != nil && s.tellCursor != nil {
		v = s.tellCursor[m.author]
	}
	m.reply <- v
}

// probeLastTell answers lastTellProbeMsg: the player's lastTellFrom, or "" if unknown.
func (z *Zone) probeLastTell(m lastTellProbeMsg) {
	var v string
	if s := z.players[m.id]; s != nil {
		v = s.lastTellFrom
	}
	m.reply <- v
}
