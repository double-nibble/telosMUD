package gate

// comms.go is the gate's side of the Phase-8 comms layer (docs/PHASE8-PLAN.md slices 8.2 + 8.6): the
// gate is the SINK for player-scoped cross-shard messages (channels, tells). The world is the SOURCE (it
// holds the authoritative author identity and publishes; P8-A2); the gate SUBSCRIBES on behalf of each
// connected player and renders received messages straight onto the existing writer path. The gate never
// publishes a chan/tell — its commbus handle is RoleGate, structurally subscribe-only on those subjects
// (the impersonation gate; commbus.go).
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
//
// # The receiver HEAR-filter + the ignore funnel (slice 8.6)
//
// Two receiver-side policies the gate enforces, both driven by a per-player COMMS-CONFIG the SOURCE world
// publishes to commbus.ConfigSubject(player) (the world computes them — it has the live *Entity + the
// channel_defs; the gate has neither):
//
//   - HEAR-filter (the 8.3 carried-forward obligation, P8-D3 / OQ-3): the gate subscribes ONLY the
//     player's effective {enabled ∩ hearable} channel subjects (concrete ChanSubject(ref) each), NOT a
//     chan.* wildcard. On a config message it re-subscribes the named set (adding new refs, dropping
//     removed ones) under the connection-scoped teardown. So a channel the player DISABLED or CANNOT HEAR
//     simply has no subscription — the line never reaches the socket. This closes the CONTENT GUARDRAIL:
//     a RESTRICTED-access channel reaches only sockets whose world put it in their hear-set.
//   - IGNORE funnel (P8-A6): the gate caches the receiver's ignore list (author ids) and drops EVERY
//     inbound comms frame whose AuthorID is in it at a SINGLE chokepoint (ignored), shared by the channel
//     AND tell delivery paths — so a new comms frame type inherits the funnel automatically (it is the
//     ONE place every inbound comms line passes before render, not a per-path check). The receiver gate
//     is the authoritative ignore-enforcement point: it is the one place that sees every inbound line for
//     this player and holds the receiver's own list.
//
// Both pieces of state live on the CONNECTION (this commsClient), so a re-dial (handoff) leaves them
// untouched — handoff-transparent like the subscriptions. On login + on a handoff arrival + on every
// toggle the source world re-publishes the config, so the gate's filter is always recomputed by the
// authoritative world. The gate stays a CONTENT-FREE sink: it never runs an access predicate or names a
// channel; it only subscribes the refs the world handed it and drops by an author-id set.
//
// # The writer path + slow-consumer backpressure (P8-A1)
//
// A received Message is rendered to a ServerFrame_Output-equivalent line and written via the SAME
// telnet.Conn.Write the per-stream writer goroutine uses (mutex-guarded in telnet.go). The slow-consumer
// protection is the bus's own bounded per-subscription buffer: a blocked socket stalls only THIS
// subscription's delivery goroutine; Publish is non-blocking and drops on a full buffer.

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// commsClient is one connection's gate-side comms subscription set + receiver-side filter state. It is
// created once per connection (openComms, called from handle after login) and closed once on teardown
// (close, via a single defer). Its lifetime is the CONNECTION's, never a stream's — a re-dial leaves it
// untouched (the handoff-transparency invariant). It holds no zone/shard state.
//
// CONCURRENCY: the config handler (which mutates the channel subscription set + the ignore set) and the
// channel/tell delivery handlers all run on independent bus goroutines, so the mutable filter state is
// guarded by mu. tell + config subscriptions are stable for the connection; only the per-channel
// subscriptions and the ignore set change (on a config message), so the lock is held briefly.
type commsClient struct {
	log    *slog.Logger
	tc     *telnet.Conn
	player string // the playerId (the stub login name today; the comms identity, P8-D5/OQ-5)
	bus    commbus.Bus
	gmcp   *gmcpState // for the GMCP Comm.Channel.Text mirror (Phase 9.5); never nil

	mu       sync.Mutex
	stable   []commbus.Subscription          // tell + config subscriptions (lifetime = connection)
	chanSubs map[string]commbus.Subscription // channel ref -> its concrete subscription (HEAR-filter)
	ignore   map[string]struct{}             // receiver ignore list (author ids) — the funnel input
	closed   bool
}

// openComms establishes the connection's comms subscriptions. The STABLE subscriptions (lifetime = the
// connection):
//
//   - the player's PERSONAL tell subject — telos.comms.tell.<playerId> — a CONCRETE per-player subject
//     (never a tell.* wildcard: subscribe is not ACL-guarded, so the concrete-subject choice is the only
//     thing preventing a cross-player tell leak).
//   - the player's COMMS-CONFIG subject — telos.comms.config.<playerId> — a CONCRETE per-player subject
//     (never a config.* wildcard). The source world publishes the effective {enabled ∩ hearable} hear-set
//   - ignore list here; the gate applies them (re-subscribe the named channels + cache the ignore set).
//
// The CHANNEL subscriptions are added dynamically by applyConfig from the world's hear-set (the receiver
// HEAR-filter — the gate no longer subscribes a chan.* wildcard, so a channel the player disabled or
// cannot hear has no subscription and its lines never reach the socket).
//
// bus is the gate's RoleGate handle (never nil — a disabled bus when NATS is down yields no-op
// subscriptions, so comms simply delivers nothing and the connection is byte-identical to pre-Phase-8).
func openComms(log *slog.Logger, bus commbus.Bus, tc *telnet.Conn, gmcp *gmcpState, player string) *commsClient {
	c := &commsClient{
		log:      log,
		tc:       tc,
		player:   player,
		bus:      bus,
		gmcp:     gmcp,
		chanSubs: map[string]commbus.Subscription{},
		ignore:   map[string]struct{}{},
	}
	c.subscribeStable(commbus.TellSubject(player), c.deliverTell)
	c.subscribeStable(commbus.ConfigSubject(player), c.applyConfig)
	return c
}

// subscribeStable registers a connection-lifetime subscription (tell/config). A failed Subscribe is
// never fatal: comms is optional. A nil/disabled bus returns a no-op Subscription.
func (c *commsClient) subscribeStable(subject string, handler func(commbus.Message)) {
	sub, err := c.bus.Subscribe(subject, handler)
	if err != nil {
		c.log.Debug("comms subscribe failed", "subject", subject, "err", err)
		return
	}
	c.mu.Lock()
	c.stable = append(c.stable, sub)
	c.mu.Unlock()
	c.log.Debug("comms subscribed", "subject", subject)
}

// applyConfig is the receiver-side config handler (Phase 8.6): it applies the per-player comms-config the
// source world published — re-subscribing the player's effective {enabled ∩ hearable} channel set (the
// HEAR-filter) and caching the ignore list (the funnel input). Runs on the config subscription's bus
// goroutine. Idempotent: re-applying the same config is a no-op (the diff adds/drops nothing).
func (c *commsClient) applyConfig(msg commbus.Message) {
	payload, err := commbus.UnmarshalConfig(msg.Body)
	if err != nil {
		c.log.Debug("comms config unmarshal failed", "player", c.player, "err", err)
		return
	}

	// Build the desired channel-ref set from the world's hear-set.
	want := make(map[string]struct{}, len(payload.HearChannels))
	for _, ref := range payload.HearChannels {
		if ref != "" {
			want[ref] = struct{}{}
		}
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	// Update the ignore set (replace wholesale — the world sends the full list each time).
	c.ignore = make(map[string]struct{}, len(payload.Ignore))
	for _, id := range payload.Ignore {
		if id != "" {
			c.ignore[id] = struct{}{}
		}
	}
	// Drop channel subscriptions no longer wanted.
	var toDrop []commbus.Subscription
	for ref, sub := range c.chanSubs {
		if _, keep := want[ref]; !keep {
			toDrop = append(toDrop, sub)
			delete(c.chanSubs, ref)
		}
	}
	// Determine channel subscriptions to add (wanted but not yet subscribed).
	var toAdd []string
	for ref := range want {
		if _, have := c.chanSubs[ref]; !have {
			toAdd = append(toAdd, ref)
		}
	}
	c.mu.Unlock()

	// Unsubscribe dropped channels OUTSIDE the lock (Unsubscribe blocks until the delivery goroutine
	// drains — never hold the connection lock across it).
	for _, sub := range toDrop {
		_ = sub.Unsubscribe()
	}
	// Subscribe added channels; record each under the lock.
	for _, ref := range toAdd {
		sub, err := c.bus.Subscribe(commbus.ChanSubject(ref), c.deliverChannel)
		if err != nil {
			c.log.Debug("comms channel subscribe failed", "player", c.player, "channel", ref, "err", err)
			continue
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			_ = sub.Unsubscribe()
			return
		}
		// A concurrent applyConfig could have already added this ref; honor the first.
		if _, have := c.chanSubs[ref]; have {
			c.mu.Unlock()
			_ = sub.Unsubscribe()
			continue
		}
		c.chanSubs[ref] = sub
		c.mu.Unlock()
	}
	c.log.Debug("comms config applied", "player", c.player, "hear", len(want), "ignore", len(payload.Ignore))
}

// ignored is the SINGLE receiver-side ignore funnel (P8-A6): it reports whether an inbound comms frame's
// author is on the receiver's ignore list. EVERY inbound comms delivery path (channel, tell, and any
// future comms frame type) calls this ONE chokepoint before rendering, so the ignore enforcement is not
// per-path — a new comms frame type inherits it for free. You cannot ignore yourself into not hearing
// your own lines: the world never puts the receiver on their own ignore list (cmdIgnore refuses it), so
// an author == self is never in the set.
func (c *commsClient) ignored(authorID string) bool {
	if authorID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.ignore[authorID]
	return ok
}

// deliverTell writes one received TELL onto the connection's socket via the existing writer path. The
// SOURCE WORLD renders the FULL tell line into Body (the gate writes it VERBATIM — a pure sink). The
// ignore funnel applies first: an ignored author's tell is dropped at the receiver (P8-A6).
func (c *commsClient) deliverTell(msg commbus.Message) {
	if c.ignored(msg.AuthorID) {
		c.log.Debug("comms tell dropped (ignored author)", "author", msg.AuthorID)
		return
	}
	c.log.Debug("comms tell delivered", "subject", msg.Subject, "author", msg.AuthorName, "seq", msg.Seq)
	_ = c.tc.Write(msg.Body + "\r\n")
}

// deliverChannel renders one received CHANNEL line. The Body is the FULLY-rendered line the SOURCE WORLD
// produced from the channel_def's format/color with the player's text sanitized as $t (P8-A7). The gate
// writes it VERBATIM. The ignore funnel applies first: an ignored author's channel line is dropped at the
// receiver (P8-A6) — the SAME chokepoint the tell path uses.
func (c *commsClient) deliverChannel(msg commbus.Message) {
	if c.ignored(msg.AuthorID) {
		c.log.Debug("comms channel dropped (ignored author)", "author", msg.AuthorID)
		return
	}
	c.log.Debug("comms channel delivered", "subject", msg.Subject, "author", msg.AuthorName, "seq", msg.Seq)
	_ = c.tc.Write(msg.Body + "\r\n")

	// GMCP mirror (Phase 9.5): a rich client that advertised Comm gets the same line as structured
	// Comm.Channel.Text {channel, talker, text} so it can route to a per-channel tab. Same hear-set +
	// ignore funnel as the text line (we are past both gates here), so a muted/ignored line emits no
	// GMCP either. (text is the rendered line today; carrying the raw message text is a follow-up.)
	if c.gmcp.supported("Comm.Channel.Text") {
		payload, _ := json.Marshal(map[string]string{
			"channel": strings.TrimPrefix(msg.Subject, commbus.ChanPrefix),
			"talker":  msg.AuthorName,
			"text":    msg.Body,
		})
		_ = c.tc.WriteGMCP("Comm.Channel.Text", payload)
	}
}

// close tears down every subscription this connection holds (the stable tell/config subs + every dynamic
// channel sub). Called ONCE, via a single defer in handle, on the same return that drops the session — so
// the subscribe/unsubscribe pair is bound to the connection lifecycle and cannot leak. Idempotent.
func (c *commsClient) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	stable := c.stable
	chans := make([]commbus.Subscription, 0, len(c.chanSubs))
	for _, sub := range c.chanSubs {
		chans = append(chans, sub)
	}
	c.stable = nil
	c.chanSubs = map[string]commbus.Subscription{}
	c.mu.Unlock()

	for _, sub := range stable {
		_ = sub.Unsubscribe()
	}
	for _, sub := range chans {
		_ = sub.Unsubscribe()
	}
	c.log.Debug("comms unsubscribed", "player", c.player)
}
