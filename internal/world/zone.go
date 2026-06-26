package world

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
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
	rooms     map[ProtoRef]*Entity // room entities, keyed by their ProtoRef (MUDLIB §4)
	players   map[string]*session  // connection state, keyed by character id
	startRoom ProtoRef             // ProtoRef of the room a fresh login spawns in
	rids      ridAllocator         // per-zone RuntimeID source for entities (identity.go)
	inbox     chan msg             // message queue; the only ingress to zone state
	log       *slog.Logger         // scoped logger: component=zone, zone=<id>

	// protos is the per-SHARD prototype cache (prototype.go), shared READ-ONLY across all
	// the shard's zone goroutines. The zone reads it via spawn; it is never mutated after
	// shard construction, so the cross-goroutine sharing needs no lock. A bare test zone
	// (newZone alone) gets its own private cache so spawn still works standalone.
	protos *protoCache

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

	// pulses is the per-zone heartbeat scheduler (pulse.go). Its callbacks fire ON THIS
	// zone goroutine, driven by the ticker case in Run's select, so they have full
	// single-writer access to zone state — combat rounds (Phase 6) and affect ticks
	// (Phase 5) hang off it. Plain zone-owned data; only this goroutine touches it.
	pulses *pulseScheduler
}

// msg is anything the zone goroutine processes off its inbox. The interface keeps
// the inbox a single typed channel while letting handle switch on concrete type.
type msg interface{ zoneMsg() }

// joinMsg adds a pre-built session (carrying its entity) to the zone directly. Used by
// tests; the network path uses attachMsg (which creates or re-binds and then joins).
type joinMsg struct{ s *session }

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
	// resumeEpoch is the player's last-recorded ownership epoch, read from the directory
	// OFF the zone goroutine (server.go) before this attach was posted. Only the fresh-login
	// (default) branch of attach consults it, seeding s.epoch = max(1, resumeEpoch) so the
	// next cross-shard move computes resumeEpoch+1 — which the placement CAS accepts. 0 on a
	// brand-new character or a token re-dial (which carries its own epoch).
	resumeEpoch uint64
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

// transferInMsg hands an existing session (and its entity) from a sibling zone on the
// SAME shard (an intra-shard cross-zone walk). The destination zone takes ownership: it
// Moves the entity into room, Stores itself into the session's currentZone pointer so
// input now routes here, and shows the new room. The SAME out channel and appliedSeq are
// carried, so there is no snapshot, no epoch bump, no directory change — and replayed
// in-flight input (forwarded by the source) still dedups by appliedSeq.
type transferInMsg struct {
	s    *session
	room ProtoRef
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
	room  ProtoRef
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

// freezeExpireMsg fires if a frozen (source-side, in-flight handoff) player is still
// frozen after freezeTTL — the backstop for a handoff that neither thawed (RPC timeout)
// nor was reclaimed. gen (the session's attachGen at freeze time) guards against acting
// on a session that has since rebound/rebuilt. See Zone.freezeExpire for thaw-vs-reap.
type freezeExpireMsg struct {
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
func (freezeExpireMsg) zoneMsg()  {}

func newZone(id string) *Zone {
	return &Zone{
		id:         id,
		rooms:      map[ProtoRef]*Entity{},
		players:    map[string]*session{},
		forwarding: map[string]*Zone{},
		inbox:      make(chan msg, 256),
		// A private, empty prototype cache by default. A shard-hosted zone has this
		// replaced with the shared per-shard cache (newShard); a bare test zone keeps its
		// own so spawn works standalone.
		protos: newProtoCache(),
		// Per-zone heartbeat scheduler (pulse.go). Empty until something registers a
		// callback; the ticker in Run is a cheap no-op until then.
		pulses: newPulseScheduler(),
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
	// The heartbeat: one ticker owned by the loop goroutine. On each tick the loop calls
	// pulses.tick INLINE — so every periodic/delayed callback runs on THIS goroutine with
	// the same single-writer access a command handler has (pulse.go). The ticker only fires
	// a select wakeup; it never touches entity state itself, and with no registered
	// callbacks tick is a no-op, so adding it cannot perturb the deterministic tests (they
	// register none) — it just costs one cheap wakeup per pulseInterval on an idle zone.
	ticker := time.NewTicker(pulseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			z.log.Debug("zone loop stop", "players", len(z.players))
			return
		case m := <-z.inbox:
			z.handle(m)
		case <-ticker.C:
			z.pulses.tick()
		}
	}
}

// handle dispatches one inbox message to the matching handler. Runs only on the
// zone goroutine (called from Run), so all handlers below are lock-free.
//
// It is wrapped in a recover() that is the process-survival net: an unrecovered panic
// in ANY handler (attach/prepare/redirect/handoffFailed/detach/reap/...) would otherwise
// propagate out of Zone.Run and crash the WHOLE world process — every zone, every player.
// On a panic we log the offending message type + stack and CONTINUE the loop. This layers
// with dispatchSafe (the COMMAND path's nicer per-player message); this outer net catches
// everything else. The underlying bug should still be fixed — the source nil-derefs below
// (attach pending-bind, prepare unknown-room) are guarded so this net is rarely tripped.
func (z *Zone) handle(m msg) {
	defer func() {
		if r := recover(); r != nil {
			z.log.Error("zone handler panicked; zone survived",
				"msg_type", fmt.Sprintf("%T", m), "panic", r, "stack", string(debug.Stack()))
		}
	}()
	switch v := m.(type) {
	case joinMsg:
		z.log.Debug("inbox: join", "player", v.s.character)
		z.join(v.s)
	case attachMsg:
		z.log.Debug("inbox: attach", "player", v.character)
		z.attach(v.character, v.token, v.out, v.curZone, v.resumeEpoch)
	case transferInMsg:
		z.transferIn(v)
	case prepareMsg:
		z.prepare(v)
	case abortPendingMsg:
		z.abortPending(v.token)
	case pendingExpireMsg:
		z.pendingExpire(v.id, v.gen)
	case freezeExpireMsg:
		z.freezeExpire(v.id, v.gen)
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
	s := z.players[v.id]
	if s == nil {
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
	if s.frozen {
		// A cross-shard handoff is in progress: this shard no longer acts for the
		// player. The gate buffers input typed during the redirect and replays it to
		// the destination shard (PROTOCOL.md §5); applying it here would double-act.
		z.log.Debug("input dropped: player frozen (handoff in progress)", "player", v.id, "seq", v.seq)
		return
	}
	if v.seq != 0 && v.seq <= s.appliedSeq {
		// Replay of an already-applied line: drop it. No dispatch, no output, so the
		// command's side effects happen exactly once across a re-dial.
		z.log.Debug("duplicate input dropped", "player", v.id, "seq", v.seq, "applied", s.appliedSeq)
		return
	}
	if v.seq != 0 {
		s.appliedSeq = v.seq
	}
	z.log.Debug("inbox: input", "player", v.id, "seq", v.seq, "line", v.line)
	z.dispatchSafe(s, v.line)
}

// dispatchSafe runs one command with panic recovery. A bug in a handler must NEVER crash the
// zone goroutine: an unrecovered panic there is fatal to the whole world process and every
// player on every zone it hosts (a single malformed command would be a DoS). On a panic we
// log the stack, tell the offending player their command failed, and the zone keeps serving
// everyone else. This is the safety net; the underlying bug should still be fixed.
func (z *Zone) dispatchSafe(s *session, line string) {
	defer func() {
		if r := recover(); r != nil {
			z.log.Error("command handler panicked; zone survived",
				"player", s.character, "line", line, "panic", r, "stack", string(debug.Stack()))
			s.send(textFrame("Something went wrong with that command."))
			s.send(promptFrame())
		}
	}()
	z.dispatch(s, line)
}

// join places a newly connected player into the world: it picks a valid room
// (falling back to the start room), registers the player, announces the arrival to
// the room, shows the player their surroundings, and primes the prompt.
func (z *Zone) join(s *session) {
	r := z.resolveRoom("") // fresh join always lands in the start room
	if r == nil {
		// Empty-world boot (bare-engine invariant, docs/PHASE4-PLAN.md §7.5): the zone hosts
		// no rooms (no content loaded / no start room), so there is nowhere to place the
		// player. Reject the login cleanly rather than registering a roomless player and then
		// null-deref'ing in lookRoom/act. The player is NOT added to z.players, so no later
		// command finds a placeless session.
		z.log.Warn("login rejected: zone has no rooms (empty world)", "player", s.character, "zone", z.id)
		s.send(textFrame("This world has no rooms yet. There is nowhere to enter."))
		// Close the stream (like transferIn's empty-dest rejection) rather than leave a
		// registered-nowhere session the player can type into a void — no prompt.
		s.send(disconnectFrame("world has no content"))
		return
	}
	z.players[s.character] = s
	delete(z.forwarding, s.character) // present here again; no stale forward
	Move(s.entity, r)
	z.act("$n arrives.", s.entity, nil, nil, "", "", ToRoom)
	z.lookRoom(s)
	s.send(promptFrame())
	z.log.Debug("player joined", "player", s.character, "room", r.proto, "population", len(z.players))
}

// resolveRoom returns the room entity for the given ProtoRef, falling back to the start
// room when ref is empty or names no room this zone hosts. This is the single place the
// old "if z.rooms[room] == nil { room = z.startRoom }" guard lives now that rooms are
// entities keyed by ProtoRef.
func (z *Zone) resolveRoom(ref ProtoRef) *Entity {
	if r := z.rooms[ref]; r != nil {
		return r
	}
	return z.rooms[z.startRoom]
}

// leave removes a player from the world: detaches them from their room, announces
// the departure, and forgets them. Safe to call for an unknown id (no-op).
func (z *Zone) leave(id string) {
	s := z.players[id]
	if s == nil {
		// Clean disconnect for a player who has since transferred to a sibling zone:
		// forward it to the current owner so the player is removed there, not leaked.
		if dest := z.forwarding[id]; dest != nil {
			dest.post(leaveMsg{id: id})
			return
		}
		z.log.Debug("leave: unknown player", "player", id)
		return
	}
	if r := s.entity.location; r != nil {
		z.act("$n leaves.", s.entity, nil, nil, "", "", ToRoom)
		Move(s.entity, nil)
	}
	delete(z.players, id)
	z.log.Debug("player left", "player", id, "population", len(z.players))
}

// transferIn receives a player handed over from a sibling zone on the same shard (the
// destination side of an intra-shard cross-zone walk; the source side is Zone.move).
// It takes ownership of the existing player struct — same out channel, same appliedSeq,
// no snapshot, no epoch bump — registers it here, points its currentZone at this zone so
// the reader loop now routes input to us, announces the arrival, and shows the room.
func (z *Zone) transferIn(m transferInMsg) {
	s := m.s
	r := z.resolveRoom(m.room)
	if r == nil {
		// The destination zone hosts no rooms (empty-world boot): it cannot place the
		// transferred player. Disconnect cleanly rather than null-deref'ing in lookRoom. The
		// player keeps no presence here; the source already released it, so the session is
		// dropped (a real placement controller would re-route, Phase 10).
		z.log.Warn("intra-shard transfer rejected: destination has no rooms", "player", s.character, "zone", z.id)
		if s.currentZone != nil {
			s.currentZone.Store(z)
		}
		s.send(disconnectFrame("destination has no rooms"))
		return
	}
	// The entity now belongs to this zone: re-home it (rid allocator, zone owner) so a
	// future target reference resolves here, then place it in the destination room.
	s.entity.zone = z
	z.players[s.character] = s
	// Clear any stale forwarding entry from a previous departure from THIS zone: the
	// player is present here again, so handleInput will route to them directly.
	delete(z.forwarding, s.character)
	// From now on the player's input belongs to this zone. The source already removed
	// the player and set up forwarding for any line still in flight to it.
	if s.currentZone != nil {
		s.currentZone.Store(z)
	}
	Move(s.entity, r)
	z.act("$n arrives.", s.entity, nil, nil, "", "", ToRoom)
	z.lookRoom(s)
	s.send(promptFrame())
	z.log.Debug("intra-shard transfer in", "player", s.character, "room", r.proto,
		"applied", s.appliedSeq, "population", len(z.players))
}

// attach binds a stream's out channel to a character. A new character is created and
// joined; an existing one (a re-dial or reconnect within the link-death window) is
// re-bound to the *same* session, preserving appliedSeq so replayed input dedups
// correctly. Either way an Attached frame goes out first; session.send stamps it with
// the resume point (appliedSeq) via ServerFrame.ack_input_seq.
func (z *Zone) attach(character, token string, out chan *playv1.ServerFrame, curZone *atomic.Pointer[Zone], resumeEpoch uint64) {
	s := z.players[character]
	switch {
	case s != nil && s.pending:
		// Handoff bind: the gate re-dialed here after a Redirect. Activate the session
		// Prepare rehydrated — this is the destination self-commit.
		if token == "" || token != s.token {
			z.log.Warn("handoff bind rejected: token mismatch", "player", character)
			out <- disconnectFrame("handoff token invalid")
			return
		}
		if z.shard != nil {
			z.shard.dropToken(s.token)
		}
		s.out = out
		s.currentZone = curZone
		if curZone != nil {
			curZone.Store(z)
		}
		s.pending = false
		s.frozen = false
		s.attachGen++
		s.send(attachedFrame(z.id)) // resume ack = appliedSeq carried in the snapshot
		// prepare parked the entity's location at the destination room WITHOUT adding it
		// to the room contents (pending = invisible). Move now makes it visible. Guard the
		// location read: prepare now rejects an unplaceable room, but a defensive fallback
		// to the start room (resolveRoom("")) keeps this branch from ever null-deref'ing.
		r := z.resolveRoom("") // start-room fallback
		if s.entity.location != nil {
			r = z.resolveRoom(s.entity.location.proto)
		}
		if r != nil {
			Move(s.entity, r) // only now does the player become visible in the room
			z.act("$n arrives.", s.entity, nil, nil, "", "", ToRoom)
		}
		z.lookRoom(s)
		s.send(promptFrame())
		z.log.Debug("handoff committed: player activated", "player", character,
			"room", roomRef(s.entity.location), "applied", s.appliedSeq, "epoch", s.epoch)

	case token != "":
		// A handoff token was presented but no pending player matches it
		// (expired/aborted/never-prepared/forged). Reject rather than re-bind something
		// else or spawn a fresh character — that would silently lose the migrated state.
		z.log.Warn("handoff bind rejected: no pending player for token", "player", character)
		out <- disconnectFrame("handoff token invalid")
		return

	case s != nil && s.frozen:
		// A re-dial to the SOURCE shard mid-handoff: the handoff owns the player.
		// Re-binding would resume a frozen player and risk a both-own window. Reject.
		z.log.Warn("attach rejected: character is mid-handoff (frozen)", "player", character)
		out <- disconnectFrame("character is mid-transfer")
		return

	case s != nil:
		// Re-attach (link-dead resume): re-bind the existing session, preserving state.
		s.out = out
		s.currentZone = curZone
		if curZone != nil {
			curZone.Store(z)
		}
		s.detached = false
		s.attachGen++
		s.send(attachedFrame(z.id))
		z.log.Debug("player re-attached", "player", character, "applied_seq", s.appliedSeq, "gen", s.attachGen)
		z.lookRoom(s)
		s.send(promptFrame())

	default:
		// Fresh login. Seed the epoch from the directory's persisted placement (read off the
		// zone goroutine in server.go, threaded in as resumeEpoch) so it stays globally
		// monotonic per player: a relog after any prior cross-shard move resumes at the stored
		// epoch, and the NEXT move computes stored+1 — which the placement CAS accepts. Seed to
		// EXACTLY the stored value (not +1); brand-new characters (resumeEpoch 0) start at 1.
		epoch := resumeEpoch
		if epoch < 1 {
			epoch = 1
		}
		s = &session{character: character, out: out, epoch: epoch, currentZone: curZone}
		z.newPlayerEntity(s, character)
		z.log.Debug("fresh login epoch seeded", "player", character, "epoch", epoch, "resume", resumeEpoch)
		if curZone != nil {
			curZone.Store(z)
		}
		s.attachGen++
		s.send(attachedFrame(z.id))
		z.join(s) // registers, places, announces arrival, looks, prompts
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
	r := z.resolveRoom(m.room)
	if r == nil {
		// This zone can't place the player anywhere — the target room is unknown AND the
		// start room hasn't spawned (e.g. a just-restarted destination mid-boot). Reject
		// cleanly rather than parking a pending entity with a nil location (a landmine that
		// later null-derefs on bind). The source thaws via handoffFailed.
		z.log.Warn("handoff prepare rejected: no placeable room", "player", character, "room", m.room)
		m.reply <- status.Errorf(codes.FailedPrecondition, "zone %q cannot place room %q", z.id, m.room)
		return
	}
	s := &session{
		character:  character,
		appliedSeq: m.snap.GetAppliedSeq(),
		epoch:      m.epoch,
		pending:    true,
		token:      m.token,
	}
	e := z.newPlayerEntity(s, character)
	e.short = m.snap.GetName()
	e.keywords = []string{m.snap.GetName()}
	// Park the entity AT the destination room (location set) but NOT in its contents —
	// a pending player is invisible until the gate's re-dial activates it (attach Moves
	// it into the room then). location is how attach later recovers the destination room.
	e.location = r
	z.players[character] = s
	if z.shard != nil {
		// Index the token so a Play attach (the gate's re-dial) can route the bind to
		// THIS zone even on a multi-zone shard.
		z.shard.indexToken(m.token, z)
	}
	gen := s.attachGen
	time.AfterFunc(pendingTTL, func() { z.post(pendingExpireMsg{id: character, gen: gen}) })
	z.log.Debug("handoff prepared: pending player rehydrated", "player", character,
		"room", r.proto, "epoch", m.epoch, "applied", s.appliedSeq)
	m.reply <- nil
}

// abortPending discards a pending player by handoff token (the source cancelled).
func (z *Zone) abortPending(token string) {
	for id, s := range z.players {
		if s.pending && s.token == token {
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
	if s := z.players[id]; s != nil && s.pending && s.attachGen == gen {
		z.log.Debug("pending player expired (gate never bound)", "player", id)
		delete(z.players, id)
		if z.shard != nil {
			z.shard.dropToken(s.token)
		}
	}
}

// freezeTTL bounds how long a source-side frozen player (an in-flight cross-shard handoff)
// may linger before the backstop reaper fires. It MUST be >= pendingTTL and longer than
// handoffRPCTimeout so the normal resolutions win first: the RPC timeout thaws a failed
// handoff (handoffFailMsg) and a successful one redirects, both well before this. This only
// catches the leftover: a frozen copy that never got cleaned up (e.g. a dead gate never
// re-dialed after a successful redirect, or a path that froze but neither posted result).
// It is a package var (not a const) so a test can shrink it to exercise the reaper quickly.
var freezeTTL = pendingTTL

// freezeExpire is the backstop for a frozen source-side player still frozen after freezeTTL.
// The gen check ignores a stale timer for a session that has since rebound/rebuilt (a return
// handoff, a re-attach). It then discriminates on s.redirected:
//
//   - redirected: the handoff SUCCEEDED — the directory points at the destination, so this
//     source copy is an ORPHAN. Remove it (and drop its token) so the character can reconnect
//     to the source without hitting the frozen "mid-transfer" reject. Thawing it would be a
//     both-own bug (two shards acting for one player), so we never thaw a redirected copy.
//   - not redirected: the handoff never completed — the directory never moved, so reclaiming
//     the source IS correct. THAW IN PLACE: restore via frozenFrom (like handoffFailed) and
//     tell the player the way is barred (timeout). The placement CAS stays the arbiter: we
//     only reclaim when the directory never recorded the move.
func (z *Zone) freezeExpire(id string, gen uint64) {
	s := z.players[id]
	if s == nil || !s.frozen || s.attachGen != gen {
		return // already resolved (thawed/redirected-and-reaped) or rebound
	}
	if s.redirected {
		// Successful handoff's orphaned source copy: remove it so reconnect to the source works.
		z.log.Debug("freeze timeout: reaping orphaned redirected source copy", "player", id)
		delete(z.players, id)
		if z.shard != nil && s.token != "" {
			z.shard.dropToken(s.token)
		}
		return
	}
	// Handoff never completed: thaw in place and restore to the room they tried to leave.
	z.log.Debug("freeze timeout: thawing un-redirected player in place", "player", id)
	s.frozen = false
	if s.frozenFrom != nil {
		Move(s.entity, s.frozenFrom)
		z.act("$n arrives.", s.entity, nil, nil, "", "", ToRoom)
		s.frozenFrom = nil
	}
	s.send(textFrame("The way is barred. (handoff timed out)"))
	s.send(promptFrame())
}

// detach handles a player's stream dropping. out identifies which stream, so a stale
// detach from a stream already superseded by a re-attach is ignored. A clean quit
// removes the player at once; an unexpected drop marks the player link-dead and
// schedules a reap after the grace window (cancelled implicitly if it re-attaches and
// bumps attachGen).
func (z *Zone) detach(id string, out chan *playv1.ServerFrame) {
	s := z.players[id]
	if s == nil {
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
	if s.out != out {
		z.log.Debug("detach from superseded stream ignored", "player", id)
		return
	}
	if s.frozen {
		// Mid-handoff: the gate is re-dialing the destination shard. Do NOT remove the
		// player — the handoff owns its fate (commit -> discard, abort -> thaw).
		z.log.Debug("detach ignored: player frozen (handoff in progress)", "player", id)
		return
	}
	if s.quitting {
		z.log.Debug("clean quit, removing player", "player", id)
		z.leave(id)
		return
	}
	s.detached = true
	gen := s.attachGen
	z.log.Debug("player link-dead", "player", id, "grace", linkDeadGrace)
	time.AfterFunc(linkDeadGrace, func() { z.post(reapMsg{id: id, gen: gen}) })
}

// reap removes a link-dead player that never re-attached within the grace window. The
// generation check ensures a player that has since re-attached (bumping attachGen) is
// not removed by a stale timer.
func (z *Zone) reap(id string, gen uint64) {
	if s := z.players[id]; s != nil && s.detached && s.attachGen == gen {
		z.log.Debug("reaping link-dead player", "player", id)
		z.leave(id)
	}
}

// redirect tells a frozen player's client to re-dial the destination shard. Posted
// by the async handoff coordinator (Shard.beginHandoff) once the directory has
// recorded the new owner; the player stays frozen until the gate re-attaches there.
func (z *Zone) redirect(v redirectMsg) {
	s := z.players[v.id]
	if s == nil {
		return
	}
	s.frozenFrom = nil // committed to leaving: the failure-restore is no longer needed
	s.epoch = v.epoch
	s.send(redirectFrame(v.targetAddr, v.token, v.resumeSeq))
	// Mark the handoff as SUCCEEDED (directory now points at the destination). The freeze-
	// timeout reaper reads this: a redirected frozen copy is an orphan to remove, not thaw.
	s.redirected = true
	z.log.Debug("redirect sent", "player", v.id, "target", v.targetAddr, "epoch", v.epoch)
}

// handoffFailed thaws a player whose cross-shard move could not be initiated, so they
// are not left stuck frozen.
func (z *Zone) handoffFailed(v handoffFailMsg) {
	s := z.players[v.id]
	if s == nil {
		return
	}
	s.frozen = false
	// Restore the entity to the room it tried to leave: move() detached it (location=nil)
	// for the in-flight handoff. Without this re-attach the location stays nil and the next
	// look/move null-derefs (commands.go lookRoom/move read s.entity.location).
	if s.frozenFrom != nil {
		Move(s.entity, s.frozenFrom)
		z.act("$n arrives.", s.entity, nil, nil, "", "", ToRoom)
		s.frozenFrom = nil
	}
	z.log.Debug("handoff failed, thawing player", "player", v.id, "reason", v.reason)
	s.send(textFrame("The way is barred. (" + v.reason + ")"))
	s.send(promptFrame())
}

// Arrival/departure/say lines that others should see now flow through Zone.act
// (act.go) — one perspective-aware call replaces the old broadcast helper. act walks
// the same uniform containment tree (room.contents, MUDLIB §4) and reaches each player
// through its PlayerControlled session sink, so the bystander text is unchanged.
