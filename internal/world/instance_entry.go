package world

import (
	"context"
	"log/slog"
	"time"
)

// instance_entry.go — how a player gets INTO an instance, and how they get back OUT (#72, slice 3 of the
// instanced-zone work). #410 built the identity half, #411 built the lifecycle half and left this note on
// MintInstance: "NEVER CALL THIS ON A ZONE GOROUTINE". This file is what honors it.
//
// # The two-phase mint, and why it cannot be one phase
//
// MintInstance does a full buildZone — every room spawned, every boot reset run, every proto resolved — plus a
// scopes.seedZone store round trip, synchronously on the caller's goroutine. That is hundreds of milliseconds
// to seconds. Calling it from the ENTRANCE zone's actor would freeze that zone for the whole build: its
// heartbeat stops, so combat rounds stop landing and affect ticks stop firing; every other occupant's input
// sits unread in the inbox; and a long enough build fills that inbox and starts blocking the PRODUCERS — the
// stream reader goroutines, the saver's ack drainer, a cross-shard Prepare. One player opening a dungeon door
// would stall a town square. It is filed as #417 and resolved here rather than worked around.
//
// So entry is asynchronous, in three hops, and the player-visible contract is a brief pause:
//
//  1. The ENTRANCE ZONE validates (the harm gate, the account, the template) and posts an instanceMintReq to
//     the shard's mint queue. It returns IMMEDIATELY; the player keeps playing, in the entrance room.
//  2. A shard-level MINT WORKER — off every zone goroutine — calls MintInstance.
//  3. The worker posts an instanceReadyMsg back to the ENTRANCE zone, which re-validates everything that
//     could have changed in the gap and then does an ordinary transferOut into the new instance.
//
// Hop 3 is a normal intra-shard transfer, deliberately: transferOut/transferIn, hence claimTransferTarget
// (#409's claim protocol) and rehomeSubtree. It is NOT a raw Move + setPlayer, and the reason is sharper for
// an instance than for any other destination. Every instance allocates rids from its own 1-based allocator in
// the same order, so the mob in room 3 of copy A and the mob in room 3 of copy B have the SAME rid. A handle
// resolved against the wrong copy therefore resolves to a real, plausible, WRONG entity — deterministically,
// every time, not as a rare nondeterministic collision. rehomeSubtree re-homing the arriving subtree into the
// destination's identity space is what keeps that from happening.
//
// # Everything that can go wrong between the phases
//
// The gap is unbounded (a queued mint behind a slow build), and the player is live in it. Hop 3 therefore
// re-validates rather than assuming: the session may have quit, gone link-dead, been reaped, walked to another
// zone, started a fight, died, or begun a cross-shard handoff. The mint may have failed. The shard may have
// begun draining or be shutting down. The instance may already have been reaped. The entrance zone itself may
// have been unhosted. Each is handled and each produces either a clean player-facing line or a silent,
// bounded, self-cleaning no-op — never a wedge, and never a session in two places.
//
// A mint that succeeds but whose player is gone leaves a live, EMPTY instance. That is not a leak: it is
// quiescent by definition, and the reaper retires it once instanceMintGrace has elapsed. Deliberately not
// torn down eagerly here — an eager UnhostZone from the zone goroutine would be a blocking teardown (it waits
// on the actor) on an actor loop, which is the very thing this file exists to avoid.

// instanceMintQueueDepth bounds the shard's pending mint backlog.
//
// It is a REFUSAL bound, not a buffer: the enqueue is non-blocking (a zone goroutine may never block on a
// queue drained by workers doing store I/O), so a full queue is a clean "the way will not open" to the player.
// Sized well above the per-account mint rate limit times any plausible concurrent-account count, so reaching
// it means the workers are genuinely stalled — at which point refusing is right.
const instanceMintQueueDepth = 64

// instanceMintWorkers is how many mints may build CONCURRENTLY on a shard.
//
// Small on purpose. A mint is CPU- and allocation-heavy (a whole zone's entities plus a Lua VM) and does store
// I/O; the caps in instance.go already bound how many instances may EXIST, and this bounds how much work
// creating them may consume at once. More workers would not make any single player's pause shorter — the
// build is the cost — and would let a burst of entries compete with the zone actors for the same cores.
const instanceMintWorkers = 2

// instanceMintReq is one queued entry request: everything the worker needs to build, plus everything the
// entrance zone needs to finish the transfer when the result comes back.
//
// It carries the ORIGIN ZONE POINTER rather than a zone id. The reply must land on the exact actor that
// validated the request and still holds the session; resolving an id later could find a DIFFERENT zone object
// under the same id (a teardown plus a re-host), which would post an entry authorization into a zone that
// never granted it. The pointer also makes "the entrance zone was unhosted mid-flight" self-handling: post
// selects on z.dead, so the reply is abandoned rather than filling a buffer nobody will drain.
type instanceMintReq struct {
	origin     *Zone    // the entrance zone actor: it validated this and it will finish the transfer
	originRoom ProtoRef // the room the player is standing in — the exit ANCHOR
	character  string   // whose entry this is; re-resolved on the origin's goroutine at hop 3
	template   string   // the content zone to copy
	account    string   // from the VERIFIED session assertion; the cap is charged here
}

// instanceReadyMsg is hop 3: the worker's result, delivered back to the entrance zone's inbox.
//
// zoneID is empty exactly when err is non-nil. The error is carried rather than logged-and-dropped because
// the PLAYER is owed an answer: they were told the way was opening, and a failed mint that says nothing is
// indistinguishable from a hung game.
type instanceReadyMsg struct {
	character  string
	template   string
	zoneID     string
	originRoom ProtoRef
	err        error
}

func (instanceReadyMsg) zoneMsg() {}

// evictToAnchorMsg asks a zone to move one occupant OUT to their exit anchor. The single mechanism behind
// every INVOLUNTARY exit from an instance (#72): the drain eject, and a respawn in an instance whose template
// authors no start room.
//
// It is a MESSAGE rather than a direct call, and that is load-bearing for the respawn caller. respawnPlayer
// runs deep inside die(), inside dealDamage, inside a combat round or a Lua harm op — a stack whose frames go
// on touching the victim's entity after it returns. A cross-zone transfer RELEASES OWNERSHIP of the session
// and entity to another goroutine, so performing one from inside that stack would leave every frame above it
// reading an object another actor now owns. Posting to ourselves defers the move to a clean top-of-loop stack
// where nobody is mid-way through anything.
//
// drainEject distinguishes the SIGTERM path, which claims its destination differently — see claimEjectTarget.
type evictToAnchorMsg struct {
	character  string
	reason     string // player-facing; why they are being moved
	drainEject bool
}

func (evictToAnchorMsg) zoneMsg() {}

// ejectInstanceMsg asks an instance to evict EVERY occupant to their anchors and report how many it moved.
// BeginDrain's step 0 (#72), posted to each instance before the drain touches any lease.
//
// resp is buffered(1) by the caller, so the zone never blocks answering — the same contract
// reclaimStragglersMsg uses.
type ejectInstanceMsg struct {
	resp chan int
}

func (ejectInstanceMsg) zoneMsg() {}

// runInstanceMintWorker drains the shard's mint queue, building each instance OFF every zone goroutine.
// Started by Run (instanceMintWorkers of them).
//
// It never touches zone state: MintInstance takes s.mu for its bookkeeping and hands the built zone its own
// actor, and the result goes back to the entrance zone through the inbox like every other cross-goroutine
// interaction. Single-writer is untouched.
func (s *Shard) runInstanceMintWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-s.mintQ:
			s.serveMintRequest(ctx, req)
		}
	}
}

// serveMintRequest performs one mint and delivers the result. Always delivers: a mint that failed still owes
// the entrance zone a message, because that message is what clears the session's pending flag and tells the
// player. Off-goroutine.
func (s *Shard) serveMintRequest(ctx context.Context, req instanceMintReq) {
	z, err := s.MintInstance(ctx, req.template, req.account)
	reply := instanceReadyMsg{
		character:  req.character,
		template:   req.template,
		originRoom: req.originRoom,
		err:        err,
	}
	if err == nil {
		reply.zoneID = z.id
	}
	// post, not a raw send: the entrance zone may have been unhosted while we were building (the reaper runs
	// during a drain, a rebalance can unhost, the shard may be stopping). post selects on z.dead and abandons
	// the send, so this can never block a worker forever on an inbox nobody will drain again. The freshly
	// minted instance is then empty and quiescent, and the reaper retires it after the mint grace.
	req.origin.post(reply)
}

// requestInstanceEntry is the ENGINE-side entry primitive: validate, then hand the build to a worker and
// return. Runs ON the entrance zone's goroutine. Returns false when the request was refused (the caller has
// already been told why).
//
// SECURITY — the target gate runs FIRST, before anything else happens. A forced relocation of another player is
// HARM, and an instance is a far worse destination to be forced into than a room: it is private,
// attacker-chosen content on the other side of a zone boundary, and the victim cannot be seen or helped from
// outside it. Without a gate, builder-authored content could pull a player out of a safe room into a dungeon of
// the author's choosing.
//
// The gate is the CALLER's to apply, because only the caller knows the invocation (who is acting), and this
// function refuses outright if no decision was supplied. In v1 the only caller is the Lua surface and its rule
// is SELF-ONLY: the target must be the invoking actor (luainstance.go). That is deliberately stricter than the
// mayRelocate gate hTeleport/hRecall use, which does not cover this primitive's threat model — it does not gate
// a MOB actor at all (pvpAllowed short-circuits on !isPlayer(actor) before the safe-room veto), so a mob script
// in a temple would otherwise pass it unconditionally. See luainstance.go for the full argument and for what a
// consented party-summon would additionally require.
func (z *Zone) requestInstanceEntry(s *session, template string, gated bool) bool {
	if s == nil || s.entity == nil {
		return false
	}
	if !gated {
		// Fail closed. A caller that cannot answer "may this actor relocate this target" has no business
		// moving anybody, and defaulting to "yes" here would silently un-gate every future call site.
		z.log.Error("BUG: instance entry requested with no harm-gate decision; refusing", "player", s.character)
		return false
	}
	switch {
	case s.frozen || s.pending:
		return false // a cross-shard handoff owns this session; it is not ours to move
	case s.detached || s.quitting:
		return false // link-dead or on the way out
	case s.instanceMintPending:
		s.send(textFrame("The way is already opening. Be patient."))
		return false
	case z.isInstance():
		// No nesting in v1. An instance of an instance is refused at the mint sink anyway (the template would
		// carry a '#'), but refusing HERE gives the player a sentence instead of a build failure, and states
		// the rule where content authors will read it.
		s.send(textFrame("You cannot open a way from inside one."))
		return false
	case position(s.entity) == posFighting:
		// Same rule move() enforces: you cannot leave a zone while fighting. Applying it at REQUEST time (as
		// well as at hop 3) means a player cannot start a mint, then engage, and have the arrival yank them
		// out of a fight — which would take their opponent's fighting pointer across a zone boundary.
		s.send(textFrame("You can't leave while fighting! Flee first."))
		return false
	case s.entity.location == nil:
		return false // not placed in a room: nothing to anchor to
	}
	if s.account == "" {
		// The caps are charged per ACCOUNT and MintInstance refuses an empty one rather than sharing a bucket.
		// A session gets its account from the VERIFIED assertion only, so this is the dev/unverified path or an
		// insecure keyless handoff arrival. Refusing is the correct fail-closed: the alternative is an
		// uncapped, unattributable mint.
		z.log.Warn("refusing instance entry: the session carries no verified account, so the per-account "+
			"instance cap cannot be charged", "player", s.character, "template", template)
		s.send(textFrame("The way does not open for you."))
		return false
	}
	if z.shard == nil {
		return false // a bare test zone: there is no shard to mint on
	}
	req := instanceMintReq{
		origin:     z,
		originRoom: s.entity.location.proto,
		character:  s.character,
		template:   template,
		account:    s.account,
	}
	// NON-BLOCKING. We are on a zone actor; the queue is drained by workers doing store I/O and a full one
	// means they are stalled. Blocking here would convert a slow mint into a frozen entrance zone, which is
	// the exact failure this whole file exists to prevent.
	select {
	case z.shard.mintQ <- req:
	default:
		z.log.Warn("instance mint queue full; refusing entry", "player", s.character, "template", template)
		s.send(textFrame("The way will not open right now. Try again shortly."))
		return false
	}
	s.instanceMintPending = true
	s.send(textFrame("The way begins to open..."))
	z.log.Debug("instance entry queued", "player", s.character, "template", template, "account", s.account)
	return true
}

// instanceReady is hop 3, on the ENTRANCE zone's goroutine: the mint finished (or failed) and the player must
// now be moved (or told). Every precondition requestInstanceEntry checked is re-checked, because the gap
// between the two is unbounded and the player was live for all of it.
//
// EVERY PATH THAT DECIDES NOT TO MOVE THE PLAYER CALLS abandonInstance FIRST. A mint that succeeded but was
// never entered has a live, empty, unreachable zone behind it and a cap slot charged to the account. Leaving
// that slot pinned until the reaper gets to it costs the account ~3 minutes (the mint grace plus the idle
// ticks) for two commands — an accidental self-lockout for a player who changed their mind, and a cheap
// self-denial-of-service for anybody who wants one. Marking it abandoned frees the slot immediately and lets
// the reaper skip the grace; see abandonInstance for why it is a flag and not a record deletion.
func (z *Zone) instanceReady(m instanceReadyMsg) {
	// abandon is the "the mint landed but nobody is going in" cleanup, shared by every refusal below. Guarded on
	// a successful mint (a failed one reserved nothing to release) and on the shard existing (a bare test zone).
	abandon := func() {
		if m.err == nil && m.zoneID != "" && z.shard != nil {
			z.shard.abandonInstance(m.zoneID)
		}
	}
	s := z.players[m.character]
	if s == nil {
		// Left, quit, was reaped, or walked to a sibling zone while we built. The instance is live and empty;
		// it is quiescent, so the reaper retires it. Nothing else to clean up here — and nothing to clear, since
		// the session (with its pending flag) is gone or is owned by another actor.
		//
		// This is the path an abandonment abuse would take deliberately (ask, then quit), so it is exactly the
		// one that must not leave a slot charged to an account nobody can bill it back to.
		abandon()
		z.log.Debug("instance ready but the player is no longer here; the empty instance will be reaped",
			"player", m.character, "zone", m.zoneID)
		return
	}
	s.instanceMintPending = false
	if m.err != nil {
		// Every MintInstance refusal lands here: a cap, the rate limit, a draining shard, a shutting-down
		// shard, an unknown or start-roomless template. The player gets one line; the operator gets the reason.
		z.log.Info("instance mint failed", "player", m.character, "template", m.template, "err", m.err)
		s.send(textFrame("The way fails to open."))
		return
	}
	// Re-validate the SESSION. Anything that would have refused the request refuses the arrival, and for the
	// same reasons — except that here the instance already exists, so a refusal leaves it to the reaper.
	switch {
	case s.frozen || s.pending || s.detached || s.quitting:
		abandon()
		z.log.Debug("instance ready but the session is no longer eligible to move",
			"player", m.character, "zone", m.zoneID)
		return
	case s.entity == nil || s.entity.location == nil:
		abandon()
		return
	case position(s.entity) == posFighting:
		// They engaged during the pause. Refuse rather than yank: move() refuses for the same reason, and
		// dragging a fighting player across a zone boundary is what the combat exclusion exists to prevent.
		abandon()
		s.send(textFrame("The way closes again — you are fighting."))
		return
	}
	// Resolve AND claim the destination in ONE hold of s.mu (#409). A nil claim means the instance is no
	// longer a legitimate destination: the reaper retired it (it was quiescent — nobody ever entered), or the
	// shard began draining, in which case sending anyone INTO it would make them a drain straggler.
	dest := z.shard.claimTransferTarget(m.zoneID)
	if dest == nil {
		// No claim was taken (claimTransferTarget claims nothing when it returns nil), so marking the record
		// abandoned cannot strand one. If the reaper already retired the zone the record is gone too and this is
		// a no-op; if the shard is draining, the flag simply lets the reaper skip the grace.
		abandon()
		z.log.Info("instance is no longer a valid destination; entry abandoned",
			"player", m.character, "zone", m.zoneID)
		s.send(textFrame("The way closes before you can step through."))
		return
	}
	// Record the EXIT ANCHOR before releasing ownership. It must be set here, on the goroutine that still owns
	// the session, and BEFORE transferOut: the moment we post, the destination owns the session and any write
	// from here is a race. The anchor is where they are standing NOW rather than the room recorded at request
	// time — they may have walked across the entrance zone during the pause, and the honest anchor is the door
	// they actually went in by.
	s.anchorZone = z.id
	s.anchorRoom = s.entity.location.proto
	z.log.Debug("entering instance", "player", m.character, "zone", m.zoneID,
		"anchor_zone", s.anchorZone, "anchor_room", s.anchorRoom)
	// An EMPTY destination room, not the instance's start room read off its zone object. The instance's rooms
	// are keyed by the TEMPLATE's authored refs, so the ref is knowable here — but z.startRoom belongs to the
	// destination actor, and reading another zone's fields from this goroutine is exactly the shared-mutation
	// the single-writer rule forbids. transferIn's resolveRoom("") lands the arrival in the destination's own
	// start room, resolved on the destination's goroutine. MintInstance guarantees the template HAS one.
	z.transferOut(s, dest, "", "$n steps through and is gone.")
}

// evictToAnchor moves one occupant of THIS instance out to their exit anchor. The single involuntary-exit
// mechanism (#72): the drain eject and the anchorless-respawn fallback both land here.
//
// It runs on the instance's own goroutine and hands the session over with the ordinary intra-shard transfer,
// so the claim is taken and released by the SAME goroutine (transferOut's compensator owns it on every exit
// path). That is the shape the earlier drain-eject attempt got wrong: it claimed on the DRAIN goroutine and
// released only in the zone's handler, so a reaper teardown, a wedged actor or a cancelled context leaked the
// claim permanently — after which the zone could never satisfy quiescent() and every later drain burned its
// full deadline waiting for a counter that would never come down.
func (z *Zone) evictToAnchor(m evictToAnchorMsg) bool {
	s := z.players[m.character]
	if s == nil || s.entity == nil {
		return false
	}
	if !z.isInstance() {
		// Already out (they walked out under their own power between the post and now, or this was posted to
		// an authored zone by mistake). Nothing to do, and the anchor is already cleared by transferIn.
		return false
	}
	if s.frozen || s.pending {
		return false // a cross-shard handoff owns this session
	}
	if s.anchorZone == "" {
		// Should be impossible: entry sets the anchor before it releases the session, and transferIn only
		// clears it on arrival OUTSIDE an instance. Say so loudly — a player with no way out of a dungeon is
		// exactly the wedge this slice exists to prevent — but do not invent a destination, which could
		// teleport them somewhere they have no business being.
		z.log.Error("cannot evict a player from an instance: the session carries NO EXIT ANCHOR",
			"player", m.character)
		return false
	}
	from := s.entity.location
	if from == nil {
		return false // not placed; nothing to move out of
	}
	if m.reason != "" {
		s.send(textFrame(m.reason))
	}
	// Claim the destination. The DRAIN path claims differently (see claimEjectTarget): claimTransferTarget
	// refuses everything once s.draining is set, which is set before the eject runs, so on that path it would
	// refuse every anchor and every occupant would fall through to a handoff that resolves nowhere yet.
	var dest *Zone
	if z.shard != nil {
		if m.drainEject {
			dest = z.shard.claimEjectTarget(s.anchorZone)
		} else {
			dest = z.shard.claimTransferTarget(s.anchorZone)
		}
	}
	if dest != nil {
		// Ordinary intra-shard transfer home. transferOut disengages, detaches, forwards in-flight input and
		// hands the session over; transferIn re-homes the subtree and CLEARS the anchor on arrival.
		z.transferOut(s, dest, s.anchorRoom, "$n vanishes.")
		return true
	}
	// The anchor zone is not a destination on this shard: it was rebalanced to a peer while the player was
	// inside, or the drain has already handed it over. Follow it across the shard boundary exactly as move()
	// would for any other cross-shard exit. The handoff clears nothing itself — the session is rebuilt from a
	// snapshot at the destination, which carries no anchor, so the anchor dies with the source copy.
	if z.handoff == nil {
		z.log.Warn("cannot evict a player to their anchor: it is not hosted here and this shard cannot hand off",
			"player", m.character, "anchor_zone", s.anchorZone)
		return false
	}
	z.log.Info("evicting an instance occupant to their anchor across a shard boundary",
		"player", m.character, "anchor_zone", s.anchorZone, "anchor_room", s.anchorRoom)
	z.initiateHandoff(s, from, s.anchorZone, s.anchorRoom, "")
	return true
}

// ejectAllToAnchors evicts every occupant of this instance and reports how many were moved. The zone-goroutine
// half of BeginDrain's step 0 (#72).
//
// It snapshots the ids before iterating: evictToAnchor mutates z.players (transferOut calls delPlayer), and
// ranging a map while deleting from it is how a drain silently skips somebody.
func (z *Zone) ejectAllToAnchors(resp chan int) {
	ids := make([]string, 0, len(z.players))
	for id := range z.players {
		ids = append(ids, id)
	}
	moved := 0
	for _, id := range ids {
		if z.evictToAnchor(evictToAnchorMsg{
			character:  id,
			reason:     drainEjectNotice,
			drainEject: true,
		}) {
			moved++
		}
	}
	if moved > 0 {
		z.log.Info("ejected instance occupants to their anchors for a drain", "zone", z.id, "moved", moved)
	}
	resp <- moved
}

// drainEjectNotice is what an instance occupant sees as a graceful drain walks them back out to the door they
// came in by. It is deliberately in-world rather than operational: they are ABOUT to be redirected seamlessly
// from the anchor zone (that is the whole point of ejecting before the handover), so this is not a disconnect
// warning and must not read like one.
const drainEjectNotice = "The way behind you closes, and you find yourself back where you entered."

// drainEjectBarrier bounds how long BeginDrain waits for ONE instance to finish ejecting its occupants.
//
// The wait is what makes the eject safe (see claimEjectTarget): the eject window is open only while BeginDrain
// is blocked here, so a claim can never be taken at an unbounded later time — after step 3 has flushed and
// disconnected the destination. Generous relative to the work (a handful of transfers on a live actor), short
// relative to any drain deadline, so a wedged instance costs the drain a bounded pause and then falls back to
// its occupants being reclaimed as stragglers — the pre-#72 outcome, not a new failure. Var for tests.
var drainEjectBarrier = 5 * time.Second

// ejectInstanceOccupants is BeginDrain's step 0: walk every instance's occupants back out to their anchors,
// BEFORE any lease is handed over and before the population snapshot, so an ejected player is counted (and
// redirected) in the zone they are actually redirected from.
//
// Without this, SIGTERM disconnects every dungeon occupant. #411 excluded instances from the drain HANDOVER —
// correctly, there is no lease to flip and drainPlayer would hand each occupant to an instance id no peer can
// resolve — but that left them resident, so every one of them was a deadline straggler, dropped and told to
// reconnect. On every rolling deploy.
//
// THE THREE THINGS THE EARLIER ATTEMPT GOT WRONG, and how each is avoided:
//
//   - It used a blocking z.post with no context on the SIGTERM path. One saturated inbox then blocked
//     BeginDrain before step 1 and the whole shard was SIGKILLed at the shutdown deadline. Here the send
//     selects on ctx.Done() and z.dead, matching what step 3 already does.
//   - It split the claim from the release across goroutines: the drain goroutine claimed the destination and
//     only the instance's handler released it, so a reaper teardown, a wedged actor or a cancelled ctx leaked
//     the claim permanently. Here the DRAIN goroutine claims nothing at all — the instance's own goroutine
//     claims and releases through transferOut's compensator.
//   - It let a claim be taken at an unbounded later time by skipping the `draining` refusal. Here the skip is
//     scoped by an explicit window (claimEjectTarget) that is open only while this function is blocked on its
//     barrier, and is closed under the SAME mutex the claim is taken under.
//
// THE REAPER KEEPS SWEEPING throughout: s.draining gates minting, not reaping, so an instance that empties
// (including one this function just emptied) can be unhosted out from under us at any point. Both the post and
// the collect therefore watch z.dead, and a torn-down instance is simply one with nothing left to eject.
func (s *Shard) ejectInstanceOccupants(ctx context.Context, instances []*Zone) {
	if len(instances) == 0 {
		return
	}
	// Open the eject window and close it on EVERY exit path. While it is open, claimEjectTarget will admit an
	// arrival into a draining shard's zone; the barrier below is what bounds how long that can be true.
	s.mu.Lock()
	s.instanceEjectWindow = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.instanceEjectWindow = false
		s.mu.Unlock()
	}()

	// ONE deadline for the whole eject phase, armed before the first post and selected on by BOTH loops.
	//
	// The post needs it as much as the collect does, and that is the earlier attempt's first HIGH finding
	// stated exactly: a `z.inbox <-` send with no time bound blocks as soon as one instance's inbox is
	// saturated (a wedged actor, or simply a busy one), and it blocks BeginDrain before step 1 — before any
	// lease is handed over, before the durable flush. On SIGTERM that is the entire shutdown budget spent on
	// the eject, and then SIGKILL with nothing flushed for anybody on the shard. ctx.Done() is NOT sufficient
	// cover: the drain's context is typically the whole shutdown deadline, so honoring only it converts a
	// blocked eject into "shutdown took 45 seconds and then dropped everyone", which is the same outcome.
	deadline := time.After(drainEjectBarrier)
	resps := make([]chan int, len(instances))
	for i, z := range instances {
		ch := make(chan int, 1)
		select {
		case z.inbox <- ejectInstanceMsg{resp: ch}:
			resps[i] = ch
		case <-z.dead:
			resps[i] = nil // reaped mid-drain: quiescent by UnhostZone's precondition, so nobody is in it
		case <-ctx.Done():
			resps[i] = nil // the drain is out of time; its occupants fall back to being reclaimed
		case <-deadline:
			resps[i] = nil
			slog.Warn("drain: could not even deliver an eject to an instance (its inbox is saturated); its "+
				"occupants will be reclaimed from durable state instead of redirected", "zone", z.id)
		}
	}
	total := 0
	for i, z := range instances {
		if resps[i] == nil {
			continue
		}
		select {
		case n := <-resps[i]:
			total += n
		case <-z.dead:
		case <-ctx.Done():
		case <-deadline:
			// A wedged instance costs the drain this bounded pause, once. Its occupants stay resident and are
			// flushed + reclaimed by step 3 — the degraded outcome, which is exactly what happened to EVERY
			// instance occupant before this step existed.
			slog.Warn("drain: an instance did not finish ejecting its occupants in time; they will be "+
				"reclaimed from durable state instead of redirected", "zone", z.id)
		}
	}
	if total > 0 {
		slog.Info("drain: ejected instance occupants to their exit anchors", "players", total)
	}
}
