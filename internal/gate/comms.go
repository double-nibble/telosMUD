package gate

// comms.go is the gate's side of the Phase-8 comms layer (docs/PHASE8-PLAN.md slice 8.2, P8-D1-B):
// the gate is the SINK for player-scoped cross-shard messages (channels, tells). The world is the
// SOURCE (it holds the authoritative author identity and publishes; P8-A2); the gate SUBSCRIBES on
// behalf of each connected player and renders received messages straight onto the existing writer
// path. The gate never publishes a chan/tell — its commbus handle is RoleGate, structurally
// subscribe-only on those subjects (the impersonation gate; commbus.go).
//
// # Why the comms client lives at CONNECTION scope (the load-bearing P8-D1 proof)
//
// The decisive Phase-8 topology argument (PHASE8-PLAN §1, P8-D1) is that comms-subscription lifetime
// tracks the CONNECTION — the gate's stable unit — NOT zone ownership, which moves on a handoff. The
// gate already survives a cross-shard walk: it keeps the same session + socket and merely re-dials the
// Play stream (gate.go, the re-dial loop / runStream). So the comms client is opened ONCE in handle,
// AFTER login (we then know the playerId) and OUTSIDE the per-shard re-dial loop, and torn down by a
// single defer on the same return that drops the connection. A re-dial (A→B) runs entirely inside
// runStream and NEVER touches this subscription — the player keeps receiving comms across the walk.
// A world-held subscription (rejected option A) would have to migrate on every handoff, layering a new
// neither/both-subscribed window onto the existing handoff window; the gate subscription does not move.
//
// # The writer path + slow-consumer backpressure (P8-A1)
//
// A received Message is rendered to a ServerFrame_Output-equivalent line and written via the SAME
// telnet.Conn.Write the per-stream writer goroutine uses. That Write is mutex-guarded (telnet.go,
// writeRaw under c.mu) so a comms line and a world frame never interleave mid-frame on the wire — one
// serialized writer, no parallel unbounded path. The slow-consumer protection is the bus's own bounded
// per-subscription buffer (commbus membus memSubDepth / the NATS pending limits): a blocked socket
// stalls only THIS subscription's delivery goroutine; Publish is non-blocking and drops on a full
// buffer, so one slow terminal never stalls the channel fan-out to siblings (the transient at-most-once
// posture comms already accepts).

import (
	"log/slog"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// commsClient is one connection's gate-side comms subscription set. It is created once per connection
// (openComms, called from handle after login) and closed once on teardown (close, via a single defer).
// Its lifetime is the CONNECTION's, never a stream's — a re-dial leaves it untouched (the handoff-
// transparency invariant). It holds no zone/shard state: the gate is stateless beyond live sockets, and
// comms is no exception.
type commsClient struct {
	log    *slog.Logger
	tc     *telnet.Conn
	player string // the playerId (the stub login name today; the comms identity, P8-D5/OQ-5)

	subs []commbus.Subscription // every live subscription this connection holds, closed on teardown
}

// openComms establishes the connection's comms subscriptions. Two subscriptions for slice 8.3:
//
//   - the player's PERSONAL tell subject — telos.comms.tell.<playerId> — a CONCRETE per-player subject
//     (never a telos.comms.tell.* wildcard: subscribe is not ACL-guarded, so the concrete-subject
//     choice is the only thing preventing a cross-player tell leak; PHASE8-PLAN 8.5 obligation). 8.5
//     gives this subject real senders; until then the synthetic test publishes here.
//   - the CHANNEL fan-out — telos.comms.chan.* (every channel). The gate has NO content (it cannot
//     enumerate the channel_defs or run their access predicate), so for 8.3 it subscribes the wildcard
//     and renders whatever the SOURCE WORLD published. The world is the authoritative SPEAK gate (it
//     checked channelDef.access against the live *Entity before publishing — P8-A8), and it rendered
//     the full line (format/color/sanitized $t) into Body, so the gate is a dumb sink. The per-player
//     enabled-channel set + per-channel subscribe (the receiver HEAR filter, OQ-3) lands with the
//     channel toggles + the comms-state subtree in slice 8.6; 8.3 shows every channel. The wildcard is
//     CHANNEL-ONLY (chan.*) — never tell.* — so it can never leak another player's directed tell.
//
// bus is the gate's RoleGate handle (never nil — a disabled bus when NATS is down yields no-op
// subscriptions, so comms simply delivers nothing and the connection is byte-identical to pre-Phase-8).
func openComms(log *slog.Logger, bus commbus.Bus, tc *telnet.Conn, player string) *commsClient {
	c := &commsClient{log: log, tc: tc, player: player}

	c.subscribe(bus, commbus.TellSubject(player), c.deliverTell)
	c.subscribe(bus, commbus.ChanPrefix+"*", c.deliverChannel) // every channel (8.3); per-enabled-set is 8.6
	return c
}

// subscribe registers one subscription and records it for teardown. A failed Subscribe is never fatal:
// comms is optional, so it logs and continues — the player still plays, just without that subscription
// (the NATS-down degradation). A nil/disabled bus returns a no-op Subscription (nil error), so it never
// errors here.
func (c *commsClient) subscribe(bus commbus.Bus, subject string, handler func(commbus.Message)) {
	sub, err := bus.Subscribe(subject, handler)
	if err != nil {
		c.log.Debug("comms subscribe failed", "subject", subject, "err", err)
		return
	}
	c.subs = append(c.subs, sub)
	c.log.Debug("comms subscribed", "subject", subject)
}

// deliverTell renders one received TELL onto the connection's socket via the EXISTING writer path
// (telnet.Conn.Write — the same mutex-guarded sink the per-stream writer uses). It runs on the bus's
// own per-subscription delivery goroutine (never a stream goroutine), so it touches only the conn-
// scoped tc, which is concurrency-safe. The 8.5 tell-rendering is a stand-in (a tell-shaped line); 8.5
// gives it real senders. AuthorName is the ENGINE-SET author (P8-A2): the gate renders, never authors.
func (c *commsClient) deliverTell(msg commbus.Message) {
	c.log.Debug("comms tell delivered", "subject", msg.Subject, "author", msg.AuthorName, "seq", msg.Seq)
	_ = c.tc.Write(msg.AuthorName + " tells you, '" + msg.Body + "'\r\n")
}

// deliverChannel renders one received CHANNEL line (Phase 8.3). The Body is the FULLY-rendered line the
// SOURCE WORLD produced from the channel_def's format/color with the player's text sanitized as the $t
// DATA arg (P8-A7) — so a `$`/ANSI/IAC in the original message is already literal and a player could
// not forge a "[gossip] Admin:" prefix. The gate writes it VERBATIM: it does not re-render, re-parse,
// or trust any field for markup — the engine=mechanism / content=flavor split keeps the channel format
// world-side (content) and the gate a dumb sink. A blocked socket stalls only this goroutine; the bus's
// bounded buffer drops rather than stalling the publisher (P8-A1, slow-consumer).
func (c *commsClient) deliverChannel(msg commbus.Message) {
	c.log.Debug("comms channel delivered", "subject", msg.Subject, "author", msg.AuthorName, "seq", msg.Seq)
	_ = c.tc.Write(msg.Body + "\r\n")
}

// close tears down every subscription this connection holds. It is called ONCE, via a single defer in
// handle, on the same return that drops the session — so the subscribe/unsubscribe pair is bound to the
// connection lifecycle and cannot leak (a leaked subscription is a slow resource leak and a ghost-
// presence bug; PHASE8-PLAN integration-risk #2). Idempotent: Unsubscribe is safe to call once per sub.
func (c *commsClient) close() {
	for _, sub := range c.subs {
		_ = sub.Unsubscribe()
	}
	c.subs = nil
	c.log.Debug("comms unsubscribed", "player", c.player)
}
