package world

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// linkDeadGrace is how long a player's in-zone presence survives after its stream
// drops unexpectedly (no clean quit). A re-dial (handoff, docs/PROTOCOL.md §5) or a
// reconnect within this window re-binds to the same player and resumes; otherwise
// the player is reaped. Tunable later.
const linkDeadGrace = 60 * time.Second

// Zone is the actor (docs/ARCHITECTURE.md §3). A single goroutine — Run — owns all
// rooms and players within the zone and is the *only* code that ever reads or
// mutates their state, so game logic needs no locks. Every other goroutine (each
// player's gRPC stream handler in server.go, future cross-zone senders) interacts
// with the zone exclusively by posting messages to inbox; none of them touch zone
// state directly.
//
// Lifecycle of a message: a producer calls post (from any goroutine) -> the message
// lands on the buffered inbox channel -> Run pulls it off and calls handle, which
// runs on the single zone goroutine. From there everything (join/input/leave) is
// sequential and single-threaded.
type Zone struct {
	id        string
	rooms     map[string]*Room
	players   map[string]*player
	startRoom string
	inbox     chan msg     // message queue; the only ingress to zone state
	log       *slog.Logger // scoped logger: component=zone, zone=<id>

	// shard, if set, is the world process hosting this zone. It is read (never
	// mutated through this field) by the zone goroutine to learn its sibling zones for
	// an intra-shard move and to populate/clear the shard token index. nil on a bare
	// test zone built via newZone/newDemoZone without a shard.
	shard *Shard

	// forwarding routes in-flight input for a player who has just left this zone via
	// an intra-shard transfer to the destination zone. The reader-loop goroutine is
	// separate, so a line it posted to THIS (source) zone in the window between the
	// transfer and its observing the new currentZone would otherwise hit a departed
	// player and be dropped. Instead handleInput re-posts it to the recorded
	// destination, which dedups by appliedSeq — nothing lost, nothing double-applied.
	// Written and read only by this zone's goroutine, so it needs no lock.
	forwarding map[string]*Zone

	// handoff, if set, initiates a cross-shard handoff when a player walks into a
	// zone NO shard on this process owns (set by the Shard). It runs asynchronously and
	// posts results back to the source zone as redirectMsg / handoffFailMsg. nil on a
	// single-shard zone, where cross-shard exits are sealed.
	handoff func(src *Zone, snap *handoffv1.PlayerSnapshot, destZone, destRoom string, epoch uint64)
}

// msg is anything the zone goroutine processes off its inbox. The interface keeps
// the inbox a single typed channel while letting handle switch on concrete type.
type msg interface{ zoneMsg() }

// joinMsg adds a pre-built player to the zone directly. Used by tests; the network
// path uses attachMsg (which creates or re-binds and then joins).
type joinMsg struct{ p *player }

// attachMsg binds a player's gRPC stream (its out channel) to a character. If the
// character is unknown it creates and joins a new player; if it already exists
// (a re-dial/reconnect within the link-death window) it re-binds the stream to the
// existing player, preserving appliedSeq so input replay dedups correctly.
type attachMsg struct {
	character string
	token     string // non-empty on a handoff re-dial; binds & activates a pending player
	out       chan *playv1.ServerFrame
	// curZone is the per-connection routing pointer the Play stream owns. The zone
	// Stores itself here once it binds the player, so the reader loop posts subsequent
	// input to this zone (and, after an intra-shard move, to the destination zone). nil
	// for test-only attaches that don't drive a real stream.
	curZone *atomic.Pointer[Zone]
}

// inputMsg carries one line of player input. seq is the gate's session-scoped input
// sequence (docs/PROTOCOL.md §5); seq==0 means unsequenced (tests/internal) and is
// always applied. A seq <= the player's appliedSeq is a replay and is dropped.
type inputMsg struct {
	id   string
	seq  uint64
	line string
}

// detachMsg signals that a player's stream dropped. out identifies which stream, so
// a stale detach from a superseded stream (after a re-attach) is ignored. A clean
// quit removes the player immediately; an unexpected drop starts the link-death grace.
type detachMsg struct {
	id  string
	out chan *playv1.ServerFrame
}

// reapMsg fires after the link-death grace to remove a player that never re-attached.
// gen guards against reaping a player that has since re-attached (new generation).
type reapMsg struct {
	id  string
	gen uint64
}

// leaveMsg removes a player from the zone immediately.
type leaveMsg struct{ id string }

// transferInMsg hands an existing player struct from a sibling zone on the SAME shard
// (an intra-shard cross-zone walk). The destination zone takes ownership: it places
// the player in room, Stores itself into the player's currentZone pointer so input
// now routes here, and shows the new room. The SAME out channel and appliedSeq are
// carried, so there is no snapshot, no epoch bump, no directory change — and replayed
// in-flight input (forwarded by the source) still dedups by appliedSeq.
type transferInMsg struct {
	p    *player
	room string
}

// redirectMsg is posted back by the async handoff coordinator once the destination
// shard is chosen and the directory updated: the zone sends the player a Redirect
// frame (the gate will re-dial the new shard). The player stays frozen.
type redirectMsg struct {
	id         string
	targetAddr string
	token      string
	resumeSeq  uint64
	epoch      uint64
}

// handoffFailMsg is posted back if the handoff could not be initiated, so the zone
// thaws the otherwise-stuck frozen player.
type handoffFailMsg struct {
	id     string
	reason string
}

// prepareMsg is the destination side: rehydrate the snapshot as a PENDING player in
// this zone and reply on the channel (nil on success). Posted by the Handoff server.
type prepareMsg struct {
	snap  *handoffv1.PlayerSnapshot
	room  string
	epoch uint64
	token string
	reply chan error
}

// abortPendingMsg discards a pending player by handoff token (source cancelled).
type abortPendingMsg struct{ token string }

// pendingExpireMsg fires if a pending player is never bound by the gate within the
// TTL; gen guards against expiring one that has since been activated/rebuilt.
type pendingExpireMsg struct {
	id  string
	gen uint64
}

func (joinMsg) zoneMsg()         {}
func (attachMsg) zoneMsg()       {}
func (inputMsg) zoneMsg()        {}
func (detachMsg) zoneMsg()       {}
func (reapMsg) zoneMsg()         {}
func (leaveMsg) zoneMsg()        {}
func (transferInMsg) zoneMsg()   {}
func (redirectMsg) zoneMsg()     {}
func (handoffFailMsg) zoneMsg()  {}
func (prepareMsg) zoneMsg()      {}
func (abortPendingMsg) zoneMsg() {}
func (pendingExpireMsg) zoneMsg() {}

func newZone(id string) *Zone {
	return &Zone{
		id:         id,
		rooms:      map[string]*Room{},
		players:    map[string]*player{},
		forwarding: map[string]*Zone{},
		inbox:      make(chan msg, 256),
		// Scoped logger so every line this zone emits is tagged with its id; all
		// the verbose control-flow tracing below goes through z.log at Debug.
		log: slog.With("component", "zone", "zone", id),
	}
}

// post enqueues a message for the zone goroutine. Safe to call from any goroutine —
// this is the *only* sanctioned way to reach zone state from outside the loop.
func (z *Zone) post(m msg) { z.inbox <- m }

// Run is the zone's single-threaded event loop and the heart of the actor model.
// It runs on one dedicated goroutine and serially handles inbox messages until ctx
// is cancelled. Because all state mutation funnels through here, no other goroutine
// ever races it.
func (z *Zone) Run(ctx context.Context) {
	z.log.Debug("zone loop start", "rooms", len(z.rooms), "start_room", z.startRoom)
	for {
		select {
		case <-ctx.Done():
			z.log.Debug("zone loop stop", "players", len(z.players))
			return
		case m := <-z.inbox:
			z.handle(m)
		}
	}
}

// handle dispatches one inbox message to the matching handler. Runs only on the
// zone goroutine (called from Run), so all handlers below are lock-free.
func (z *Zone) handle(m msg) {
	switch v := m.(type) {
	case joinMsg:
		z.log.Debug("inbox: join", "player", v.p.id)
		z.join(v.p)
	case attachMsg:
		z.log.Debug("inbox: attach", "player", v.character)
		z.attach(v.character, v.token, v.out, v.curZone)
	case transferInMsg:
		z.transferIn(v)
	case prepareMsg:
		z.prepare(v)
	case abortPendingMsg:
		z.abortPending(v.token)
	case pendingExpireMsg:
		z.pendingExpire(v.id, v.gen)
	case inputMsg:
		z.handleInput(v)
	case detachMsg:
		z.log.Debug("inbox: detach", "player", v.id)
		z.detach(v.id, v.out)
	case reapMsg:
		z.reap(v.id, v.gen)
	case redirectMsg:
		z.redirect(v)
	case handoffFailMsg:
		z.handoffFailed(v)
	case leaveMsg:
		z.log.Debug("inbox: leave", "player", v.id)
		z.leave(v.id)
	}
}

// handleInput applies one input line with exactly-once semantics. A sequenced line
// (seq>0) at or below the player's high-water mark is a replay — dropped before it
// can run a second time (docs/PROTOCOL.md §5). Otherwise the high-water advances and
// the line is dispatched.
func (z *Zone) handleInput(v inputMsg) {
	p := z.players[v.id]
	if p == nil {
		// The player may have just left via an intra-shard transfer while the separate
		// reader-loop goroutine was still posting to this (source) zone. Re-post the line
		// to the destination zone, which dedups by appliedSeq so it is neither lost nor
		// double-applied. Once the reader loop observes the new currentZone it posts
		// there directly and this forwarding entry is never consulted again.
		if dest := z.forwarding[v.id]; dest != nil {
			z.log.Debug("forwarding in-flight input to destination zone",
				"player", v.id, "seq", v.seq, "to_zone", dest.id)
			dest.post(v)
			return
		}
		// Input for a player the zone no longer knows about (e.g. leave/input race).
		z.log.Debug("inbox: input for unknown player", "player", v.id)
		return
	}
	if p.frozen {
		// A cross-shard handoff is in progress: this shard no longer acts for the
		// player. The gate buffers input typed during the redirect and replays it to
		// the destination shard (PROTOCOL.md §5); applying it here would double-act.
		z.log.Debug("input dropped: player frozen (handoff in progress)", "player", v.id, "seq", v.seq)
		return
	}
	if v.seq != 0 && v.seq <= p.appliedSeq {
		// Replay of an already-applied line: drop it. No dispatch, no output, so the
		// command's side effects happen exactly once across a re-dial.
		z.log.Debug("duplicate input dropped", "player", v.id, "seq", v.seq, "applied", p.appliedSeq)
		return
	}
	if v.seq != 0 {
		p.appliedSeq = v.seq
	}
	z.log.Debug("inbox: input", "player", v.id, "seq", v.seq, "line", v.line)
	z.dispatch(p, v.line)
}

// join places a newly connected player into the world: it picks a valid room
// (falling back to the start room), registers the player, announces the arrival to
// the room, shows the player their surroundings, and primes the prompt.
func (z *Zone) join(p *player) {
	if p.room == "" || z.rooms[p.room] == nil {
		p.room = z.startRoom
	}
	z.players[p.id] = p
	delete(z.forwarding, p.id) // present here again; no stale forward
	r := z.rooms[p.room]
	r.occupants[p.id] = true
	z.broadcast(r, p.id, p.name+" arrives.")
	z.lookRoom(p)
	p.send(promptFrame())
	z.log.Debug("player joined", "player", p.id, "room", p.room, "population", len(z.players))
}

// leave removes a player from the world: detaches them from their room, announces
// the departure, and forgets them. Safe to call for an unknown id (no-op).
func (z *Zone) leave(id string) {
	p := z.players[id]
	if p == nil {
		// Clean disconnect for a player who has since transferred to a sibling zone:
		// forward it to the current owner so the player is removed there, not leaked.
		if dest := z.forwarding[id]; dest != nil {
			dest.post(leaveMsg{id: id})
			return
		}
		z.log.Debug("leave: unknown player", "player", id)
		return
	}
	if r := z.rooms[p.room]; r != nil {
		delete(r.occupants, id)
		z.broadcast(r, id, p.name+" leaves.")
	}
	delete(z.players, id)
	z.log.Debug("player left", "player", id, "room", p.room, "population", len(z.players))
}

// transferIn receives a player handed over from a sibling zone on the same shard (the
// destination side of an intra-shard cross-zone walk; the source side is Zone.move).
// It takes ownership of the existing player struct — same out channel, same appliedSeq,
// no snapshot, no epoch bump — registers it here, points its currentZone at this zone so
// the reader loop now routes input to us, announces the arrival, and shows the room.
func (z *Zone) transferIn(m transferInMsg) {
	p := m.p
	room := m.room
	if z.rooms[room] == nil {
		room = z.startRoom
	}
	p.room = room
	z.players[p.id] = p
	// Clear any stale forwarding entry from a previous departure from THIS zone: the
	// player is present here again, so handleInput will route to them directly.
	delete(z.forwarding, p.id)
	// From now on the player's input belongs to this zone. The source already removed
	// the player and set up forwarding for any line still in flight to it.
	if p.currentZone != nil {
		p.currentZone.Store(z)
	}
	r := z.rooms[room]
	r.occupants[p.id] = true
	z.broadcast(r, p.id, p.name+" arrives.")
	z.lookRoom(p)
	p.send(promptFrame())
	z.log.Debug("intra-shard transfer in", "player", p.id, "room", room,
		"applied", p.appliedSeq, "population", len(z.players))
}

// attach binds a stream's out channel to a character. A new character is created and
// joined; an existing one (a re-dial or reconnect within the link-death window) is
// re-bound to the *same* player, preserving appliedSeq so replayed input dedups
// correctly. Either way an Attached frame goes out first; player.send stamps it with
// the resume point (appliedSeq) via ServerFrame.ack_input_seq.
func (z *Zone) attach(character, token string, out chan *playv1.ServerFrame, curZone *atomic.Pointer[Zone]) {
	p := z.players[character]
	switch {
	case p != nil && p.pending:
		// Handoff bind: the gate re-dialed here after a Redirect. Activate the player
		// Prepare rehydrated — this is the destination self-commit.
		if token == "" || token != p.token {
			z.log.Warn("handoff bind rejected: token mismatch", "player", character)
			out <- disconnectFrame("handoff token invalid")
			return
		}
		if z.shard != nil {
			z.shard.dropToken(p.token)
		}
		p.out = out
		p.currentZone = curZone
		if curZone != nil {
			curZone.Store(z)
		}
		p.pending = false
		p.frozen = false
		p.attachGen++
		p.send(attachedFrame(z.id)) // resume ack = appliedSeq carried in the snapshot
		if r := z.rooms[p.room]; r != nil {
			r.occupants[p.id] = true // only now does the player become visible in the room
			z.broadcast(r, p.id, p.name+" arrives.")
		}
		z.lookRoom(p)
		p.send(promptFrame())
		z.log.Debug("handoff committed: player activated", "player", character,
			"room", p.room, "applied", p.appliedSeq, "epoch", p.epoch)

	case token != "":
		// A handoff token was presented but no pending player matches it
		// (expired/aborted/never-prepared/forged). Reject rather than re-bind something
		// else or spawn a fresh character — that would silently lose the migrated state.
		z.log.Warn("handoff bind rejected: no pending player for token", "player", character)
		out <- disconnectFrame("handoff token invalid")
		return

	case p != nil && p.frozen:
		// A re-dial to the SOURCE shard mid-handoff: the handoff owns the player.
		// Re-binding would resume a frozen player and risk a both-own window. Reject.
		z.log.Warn("attach rejected: character is mid-handoff (frozen)", "player", character)
		out <- disconnectFrame("character is mid-transfer")
		return

	case p != nil:
		// Re-attach (link-dead resume): re-bind the existing player, preserving state.
		p.out = out
		p.currentZone = curZone
		if curZone != nil {
			curZone.Store(z)
		}
		p.detached = false
		p.attachGen++
		p.send(attachedFrame(z.id))
		z.log.Debug("player re-attached", "player", character, "applied_seq", p.appliedSeq, "gen", p.attachGen)
		z.lookRoom(p)
		p.send(promptFrame())

	default:
		// Fresh login. epoch starts at 1 (initial ownership); each handoff bumps it.
		p = &player{id: character, name: character, out: out, epoch: 1, currentZone: curZone}
		if curZone != nil {
			curZone.Store(z)
		}
		p.attachGen++
		p.send(attachedFrame(z.id))
		z.join(p) // registers, places, announces arrival, looks, prompts
	}
}

// prepare rehydrates a snapshot as a PENDING player in this zone (the destination
// side of Prepare). It is idempotent on the deterministic token and rejects an epoch
// at or below one already seen for the character. The pending player is in the zone's
// player map but not yet in its room's occupant set — invisible until the gate's
// re-dial activates it.
func (z *Zone) prepare(m prepareMsg) {
	character := m.snap.GetCharacterId()
	if existing := z.players[character]; existing != nil {
		switch {
		case existing.pending && existing.token == m.token:
			// Idempotent retry of the same Prepare.
			z.log.Debug("handoff prepare: idempotent retry", "player", character)
			m.reply <- nil
			return
		case m.epoch <= existing.epoch:
			m.reply <- status.Errorf(codes.FailedPrecondition, "stale epoch %d <= current %d", m.epoch, existing.epoch)
			return
		case existing.frozen:
			// A stale frozen copy left by a prior handoff AWAY from this shard, now
			// superseded by a newer handoff BACK (m.epoch > existing.epoch). Monotonic
			// epoch makes the return authoritative: discard the stale copy and rehydrate
			// fresh below. This is what makes A<->B round trips work; a never-returned
			// frozen copy is still GC'd later (freeze-timeout / discard signal, deferred).
			z.log.Debug("discarding stale frozen copy for return handoff",
				"player", character, "old_epoch", existing.epoch, "new_epoch", m.epoch)
			delete(z.players, character)
		default:
			// A genuinely present (live) player with this id.
			m.reply <- status.Errorf(codes.AlreadyExists, "character %q already present", character)
			return
		}
	}
	room := m.room
	if z.rooms[room] == nil {
		room = z.startRoom
	}
	p := &player{
		id:         character,
		name:       m.snap.GetName(),
		room:       room,
		appliedSeq: m.snap.GetAppliedSeq(),
		epoch:      m.epoch,
		pending:    true,
		token:      m.token,
	}
	z.players[character] = p
	if z.shard != nil {
		// Index the token so a Play attach (the gate's re-dial) can route the bind to
		// THIS zone even on a multi-zone shard.
		z.shard.indexToken(m.token, z)
	}
	gen := p.attachGen
	time.AfterFunc(pendingTTL, func() { z.post(pendingExpireMsg{id: character, gen: gen}) })
	z.log.Debug("handoff prepared: pending player rehydrated", "player", character,
		"room", room, "epoch", m.epoch, "applied", p.appliedSeq)
	m.reply <- nil
}

// abortPending discards a pending player by handoff token (the source cancelled).
func (z *Zone) abortPending(token string) {
	for id, p := range z.players {
		if p.pending && p.token == token {
			z.log.Debug("handoff aborted: discarding pending player", "player", id)
			delete(z.players, id)
			if z.shard != nil {
				z.shard.dropToken(token)
			}
			return
		}
	}
}

// pendingExpire discards a pending player the gate never bound within the TTL. The
// generation check ignores a stale timer for a player that has since been activated.
// (A future refinement keeps it link-dead instead, since the directory still points
// here — see PROTOCOL.md §5.)
func (z *Zone) pendingExpire(id string, gen uint64) {
	if p := z.players[id]; p != nil && p.pending && p.attachGen == gen {
		z.log.Debug("pending player expired (gate never bound)", "player", id)
		delete(z.players, id)
		if z.shard != nil {
			z.shard.dropToken(p.token)
		}
	}
}

// detach handles a player's stream dropping. out identifies which stream, so a stale
// detach from a stream already superseded by a re-attach is ignored. A clean quit
// removes the player at once; an unexpected drop marks the player link-dead and
// schedules a reap after the grace window (cancelled implicitly if it re-attaches and
// bumps attachGen).
func (z *Zone) detach(id string, out chan *playv1.ServerFrame) {
	p := z.players[id]
	if p == nil {
		// The player transferred out of this zone (intra-shard walk) while the separate
		// reader-loop goroutine still held the old currentZone, and the stream then
		// dropped. Forward the link-loss to the new owner so IT runs link-death; dropping
		// it here would strand the player alive in the destination. The transfer kept the
		// same out channel, so the destination's superseded-stream check still holds.
		if dest := z.forwarding[id]; dest != nil {
			dest.post(detachMsg{id: id, out: out})
		}
		return
	}
	if p.out != out {
		z.log.Debug("detach from superseded stream ignored", "player", id)
		return
	}
	if p.frozen {
		// Mid-handoff: the gate is re-dialing the destination shard. Do NOT remove the
		// player — the handoff owns its fate (commit -> discard, abort -> thaw).
		z.log.Debug("detach ignored: player frozen (handoff in progress)", "player", id)
		return
	}
	if p.quitting {
		z.log.Debug("clean quit, removing player", "player", id)
		z.leave(id)
		return
	}
	p.detached = true
	gen := p.attachGen
	z.log.Debug("player link-dead", "player", id, "grace", linkDeadGrace)
	time.AfterFunc(linkDeadGrace, func() { z.post(reapMsg{id: id, gen: gen}) })
}

// reap removes a link-dead player that never re-attached within the grace window. The
// generation check ensures a player that has since re-attached (bumping attachGen) is
// not removed by a stale timer.
func (z *Zone) reap(id string, gen uint64) {
	if p := z.players[id]; p != nil && p.detached && p.attachGen == gen {
		z.log.Debug("reaping link-dead player", "player", id)
		z.leave(id)
	}
}

// redirect tells a frozen player's client to re-dial the destination shard. Posted
// by the async handoff coordinator (Shard.beginHandoff) once the directory has
// recorded the new owner; the player stays frozen until the gate re-attaches there.
func (z *Zone) redirect(v redirectMsg) {
	p := z.players[v.id]
	if p == nil {
		return
	}
	p.epoch = v.epoch
	p.send(redirectFrame(v.targetAddr, v.token, v.resumeSeq))
	z.log.Debug("redirect sent", "player", v.id, "target", v.targetAddr, "epoch", v.epoch)
}

// handoffFailed thaws a player whose cross-shard move could not be initiated, so they
// are not left stuck frozen.
func (z *Zone) handoffFailed(v handoffFailMsg) {
	p := z.players[v.id]
	if p == nil {
		return
	}
	p.frozen = false
	z.log.Debug("handoff failed, thawing player", "player", v.id, "reason", v.reason)
	p.send(textFrame("The way is barred. (" + v.reason + ")"))
	p.send(promptFrame())
}

// broadcast sends markup to every occupant of r except exceptID (the actor who
// caused the event). Used for arrival/departure/say lines that others should see.
func (z *Zone) broadcast(r *Room, exceptID, markup string) {
	n := 0
	for id := range r.occupants {
		if id == exceptID {
			continue
		}
		if o := z.players[id]; o != nil {
			o.send(textFrame(markup))
			n++
		}
	}
	z.log.Debug("broadcast", "room", r.id, "except", exceptID, "recipients", n)
}
