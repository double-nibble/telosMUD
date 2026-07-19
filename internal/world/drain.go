package world

import (
	"context"
	"log/slog"
	"time"

	"github.com/double-nibble/telosmud/internal/metrics"
)

// initiateHandoff freezes the player and kicks off the async cross-shard handoff to (destZone, destRoom).
// It is the shared core of a cross-zone MOVE and a graceful DRAIN, factored out of the move path so both use
// the exact same freeze/snapshot/backstop sequence. departMsg (may be "") is the room line others see as the
// player leaves — a move shows "$n departs east."; a drain is silent (same room, just a different shard).
// Runs on the zone goroutine.
func (z *Zone) initiateHandoff(s *session, from *Entity, destZone string, destRoom ProtoRef, departMsg string) {
	// Combat exclusion is enforced by the move path; disengage anyway (belt-and-suspenders) BEFORE detaching
	// from the room so no `fighting` pointer crosses the shard boundary in the snapshot and any opponent's
	// link to the departing player is dropped while the room scan can still find them.
	z.disengage(s.entity)
	// Freeze first: from now on this shard stops acting for the player. frozenFrom remembers the room so
	// handoffFailed can restore the entity if the handoff can't be initiated (else its location stays nil and
	// the next room action null-derefs). handedOff=false until the directory CAS commits (the freeze-reaper
	// discriminator).
	s.frozen = true
	s.frozenFrom = from
	s.handedOff = false
	if departMsg != "" {
		// Presence concealment (#100): route the cross-shard departure through actConceal like every other
		// departure/arrival announce, so a hidden/sneaking or dark-room mover is silent to viewers who can't
		// see them — not a leaky "Someone departs east." The empty-departMsg DRAIN caller stays silent (the
		// guard above), and actConceal is equivalent to act for a non-concealed mover.
		z.actConceal(departMsg, s.entity, ToRoom)
	}
	Move(s.entity, nil) // detach from the room so they don't linger as a ghost during the in-flight handoff
	z.log.Debug("handoff initiated", "player", s.character, "dest_zone", destZone, "dest_room", destRoom, "epoch", s.epoch)
	// Backstop the freeze: if neither the redirect (success) nor handoffFailed (RPC timeout) resolves the
	// session within freezeTTL, freezeExpire reaps the orphan (handed off) or thaws it in place. The gen guard
	// ignores a stale timer for a session that has since rebound. AfterFunc only POSTS — single-writer holds.
	gen := s.attachGen
	time.AfterFunc(freezeTTL, func() { z.post(freezeExpireMsg{id: s.character, gen: gen}) })
	z.handoff(z, buildSnapshot(s), destZone, string(destRoom), s.epoch)
}

// drainZoneMsg tells a zone to hand every live player off to the zone's NEW owner (the standby the drain
// already flipped the lease to). Each player redirects into the SAME zone + room on the new shard, so the
// socket stays open across the move (zero drop). Phase 16.4b.
type drainZoneMsg struct{}

func (drainZoneMsg) zoneMsg() {}

// drainZone hands every eligible live player off to the zone's new owner. Runs on the zone goroutine, so the
// snapshot of players is race-free; each beginHandoff then runs async. A player already mid-handoff (frozen),
// pending, or link-dead is skipped — the freeze/reap machinery already owns them.
func (z *Zone) drainZone() {
	z.draining = true // gates the eager reap of handed-off orphans on redirect (BeginDrain + #42 rebalance)
	ids := make([]string, 0, len(z.players))
	for id := range z.players {
		ids = append(ids, id)
	}
	for _, id := range ids {
		z.drainPlayer(id)
	}
}

// drainPlayer hands one player off to the zone's new owner, in place (same zone id + current room). The zone
// lease was already flipped to the target, so beginHandoff resolves the destination to the new shard.
func (z *Zone) drainPlayer(id string) {
	s := z.players[id]
	if s == nil || s.frozen || s.pending || s.detached {
		return // already migrating / not attached — the existing machinery owns it
	}
	if isGateWedged(s) {
		// The gate has stopped draining the Play stream (a full outbound buffer of consecutive dropped frames,
		// Phase 16.3): the Redirect rides the SAME s.send/out path as every other frame, so a full buffer means
		// it too would be dropped and the gate would never re-dial. Redirecting it would report a zero-drop
		// that never happened (#336). Leave it resident so the deadline reclaims it from durable state (a clean
		// reconnect), counted honestly as a straggler — not a phantom Redirected. This is the same "resident
		// player holds the drain to its deadline" shape a link-dead player already has; a wedged gate detaches
		// shortly anyway (the gate's write-deadline reclaims the socket).
		//
		// SCOPE (this is the interim heuristic, not the full fix). The check is synchronous HERE, but the
		// Redirect is sent asynchronously LATER, after the handoff RPC. A player healthy at this instant that
		// wedges DURING the in-flight handoff still gets a dropped Redirect and is still counted Redirected —
		// the window this shrinks dramatically but cannot close. Only a gate ack on the Redirect (deferred
		// option 1) closes it. The rare false positive (a busy player briefly stalling 256 sends then
		// recovering) costs one clean reconnect instead of a seamless redirect — strictly better than the drop.
		return
	}
	room := s.entity.location
	if room == nil {
		return // not placed in a room yet (a just-attaching player); the drain deadline reclaims it
	}
	z.initiateHandoff(s, room, z.id, room.proto, "") // silent, same zone, current room — now owned by the peer
}

// isGateWedged reports whether a player's gate has stopped draining the Play stream — a full outbound
// buffer's worth of consecutive dropped frames (Phase 16.3, the same threshold that logs "gate write-deadline
// will reclaim the connection"). A wedged gate cannot receive a Redirect frame, so a drain must not treat it
// as redirectable (#336). consecutiveDrops is zone-owned, so this is race-free on the zone goroutine.
func isGateWedged(s *session) bool {
	return s.consecutiveDrops >= slowClientWedgedDrops
}

// DrainResult reports what a BeginDrain did: Redirected players kept their socket (zero drop); Reclaimed
// players were still resident at the deadline and will be dropped + resume from durable state on reconnect.
// ReclaimedInfra + ReclaimedClient split the Reclaimed total by fault (see reclaimTally) for observability.
type DrainResult struct {
	Redirected      int
	Reclaimed       int
	ReclaimedInfra  int
	ReclaimedClient int
}

// drainReclaimNotice is the player-visible line a straggler still resident at the drain deadline sees before
// its socket closes: a clean "reconnect" message rather than a silent link death. drainReclaimReason is the
// terse Disconnect reason the gate renders. The wording deliberately does NOT promise "resume where you left
// off": the straggler's flush is best-effort (enqueued to the async saver, which the post-drain shutdown may
// not fully drain — see the saver-drain-barrier follow-up), so a reconnect resumes from the last DURABLE
// state, which may trail the live state by a cadence tick.
const (
	drainReclaimNotice = "The server is restarting. You have been disconnected — reconnect to continue."
	drainReclaimReason = "server restarting; reconnect"
)

// reclaimTally splits deadline stragglers by FAULT: infra (a connected, in-world player the drain could not
// hand off in time — target selection / handoff RPC / sheer volume) vs client (un-redirectable for a
// client-side reason: link-dead, or never finished connecting so it was never placed in a room).
type reclaimTally struct {
	infra  int
	client int
}

// reclaimStragglersMsg asks a zone, ON ITS GOROUTINE, to clean-disconnect every player still resident at the
// drain deadline and report the fault split. resp is buffered(1) by the caller so the zone never blocks.
type reclaimStragglersMsg struct {
	resp chan reclaimTally
}

func (reclaimStragglersMsg) zoneMsg() {}

// reclaimStragglers runs on the zone goroutine (players map is race-free here): for every player still
// resident at the drain deadline it sends a clean "server restarting; reconnect" notice + Disconnect (so the
// client gets a graceful close, not a dead socket), classifies the straggler infra- vs client-fault, and
// reports the tally. A pending arrival (destination side of an inbound handoff, no bound stream) is left to
// the bind/reap machinery. A frozen session (mid-handoff) is COUNTED (infra — the handoff didn't finish in
// time) but not sent a Disconnect: its socket fate belongs to the handoff/freeze machinery, and injecting a
// frame could race a late redirect. The durable flush already happened (Shard.Drain, posted before this).
func (z *Zone) reclaimStragglers(resp chan reclaimTally) {
	var t reclaimTally
	for _, s := range z.players {
		if s.pending {
			continue // an inbound handoff arriving here, not one of our residents to reclaim
		}
		if s.frozen && s.handedOff {
			continue // handoff CAS committed — the destination shard owns this player, so it was in effect
			// REDIRECTED (the source copy just awaits the freeze reaper). Excluding it from the tally counts
			// it as Redirected (initial - reclaimed), not as an infra fault, and it keeps its socket for the
			// pending Redirect frame.
		}
		if isClientFaultStraggler(s) {
			t.client++
		} else {
			t.infra++
		}
		if !s.detached && !s.frozen {
			s.send(textFrame(drainReclaimNotice))
			s.send(drainDisconnectFrame())
		}
	}
	resp <- t
}

// isClientFaultStraggler reports whether a deadline straggler was un-redirectable for a CLIENT-side reason.
// A frozen session is checked FIRST: a mid-handoff player has its entity detached (location nil), so without
// this guard it would be miscounted as "never placed" — but it is an INFRA fault (the handoff RPC/target was
// too slow). Then: a WEDGED gate (drainPlayer skipped it because it can't receive the Redirect), link-dead
// (detached), or never finished connecting (no entity / no room placement) is a client fault — the drain
// machinery was ready, the client's connection was the thing that couldn't complete the move. Everything
// else — a healthy, in-world, connected player the drain simply could not move in the deadline — is an infra
// fault.
//
// A wedged gate is deliberately CLIENT, not infra (#336 suggested "infra"): the root cause is the client's
// stalled socket, the same category as link death, and misfiling it as infra would inflate the metric ops
// watch to decide whether the drain/handoff machinery itself is struggling.
func isClientFaultStraggler(s *session) bool {
	if s.frozen {
		return false
	}
	if isGateWedged(s) {
		return true
	}
	return s.detached || s.entity == nil || s.entity.location == nil
}

// TargetChooser selects the peer shard a draining zone's ownership + players go to. It returns the target's
// directory shard id and dial endpoint. incoming is how many live players this zone is about to send, so a
// load-aware selector can reserve that much headroom on the target and serialize against simultaneous
// drains (#41). Injected so a hermetic test supplies a fixed peer and production supplies the live-fleet,
// reservation-serialized selector (avoiding a draining or overloaded target).
type TargetChooser func(zoneID string, incoming int) (shardID, addr string, err error)

// DrainMarker publishes this shard's draining state so the drain-target selector excludes a shard that is
// itself draining (#41) — preventing a full-fleet-rollout ping-pong (A drains onto B while B drains onto A).
// *directory.Redis satisfies it; nil disables the marker (single-shard / dev).
type DrainMarker interface {
	SetDraining(ctx context.Context, shardID string, ttl time.Duration) error
	ClearDraining(ctx context.Context, shardID string) error
}

// WithDrainMarker wires the directory port BeginDrain uses to publish this shard's draining state (#41).
// Optional: without it the drain still works, it just doesn't advertise itself as ineligible to peers.
func (s *Shard) WithDrainMarker(m DrainMarker) *Shard {
	s.drainMarker = m
	return s
}

// DrainTargetReleaser retires this shard's drain-target reservations when its drain finishes (#284).
// *directory.Redis satisfies it; nil leaves every reservation to run out its own TTL.
type DrainTargetReleaser interface {
	// ReleaseDrainTarget drops the hold outright. Only for a target that will receive NO players.
	ReleaseDrainTarget(ctx context.Context, target, drainer string) error
	// ExpireDrainTargetSoon shortens the hold to expire in `in`. For a target whose handover SUCCEEDED.
	ExpireDrainTargetSoon(ctx context.Context, target, drainer string, in time.Duration) (bool, error)
}

// PresenceReflectWindow is how long a reservation is kept alive after a SUCCESSFUL handover: one presence
// heartbeat plus margin, which is how long the target takes to report the migrated players' weight.
//
// This is the crux of #284. The reservation's job is to bridge the window between the players landing on the
// target and its next heartbeat reflecting them. DELETING the hold the moment the drain completes would
// reopen exactly that window — a concurrent drainer would read the target's stale, low load, find no
// reservation, and OVER-COMMIT. But leaving it for the full reservation TTL double-counts those players for
// the remainder, once presence HAS caught up, needlessly denying a peer real headroom. Shortening it to about
// one heartbeat threads both. (orchestration review.)
//
// It must sit strictly between presence.DefaultHeartbeat (10s — below that, we drop the hold before the
// target can report) and the reservation TTL that cmd/telos-world configures — the TTL is now tied to the
// drain deadline (#334, drainReservationTTL == drainHandoffDeadline + PresenceReflectWindow), well above this
// window, so shortening to it is always meaningful. A drain-release test pins the lower bound; a
// cmd/telos-world test pins the upper bound against this exported constant.
//
// NOTE that ExpireDrainTargetSoon never EXTENDS an expiry — it only ever shortens a hold toward this window.
// Now that the reservation TTL spans the whole drain deadline (was ~15s), a hold retired after a SUCCESSFUL
// handover still has time left, so the shorten to ~one heartbeat bites reliably rather than being pre-empted
// by a near-lapsed TTL — the retire is precise for a fast drain and a slow one alike.
const PresenceReflectWindow = 12 * time.Second

// WithDrainTargetReleaser wires the port BeginDrain uses to retire the headroom it reserved on its targets,
// rather than letting each reservation sit until its full TTL lapses (#284).
//
// Without it, for the remainder of the reservation TTL after a drainer's players have landed AND registered
// in the target's presence, BOTH the reservation and the now-real migrated load count against that target.
// The over-count is conservative — it never overloads a target — but it blocks concurrent drainers from real
// headroom, which is precisely the fleet-rollout case the reservation exists to coordinate.
func (s *Shard) WithDrainTargetReleaser(r DrainTargetReleaser) *Shard {
	s.drainReleaser = r
	return s
}

// BeginDrain gracefully drains this shard for a rolling redeploy (Phase 16.4b): it stops accepting new
// fresh logins, hands each hosted zone's ownership to a chosen peer (the atomic fenced lease flip), fans the
// live players off to that peer over the cross-shard handoff (sockets stay open — zero dropped connections),
// and waits until every zone is empty or the deadline. Stragglers at the deadline are flushed and left to
// reconnect from durable state (counted as Reclaimed, not zero-drop). Runs OFF the zone goroutines (blocking
// directory/RPC I/O); safe to call from the SIGTERM handler. Requires leasing (WithZoneLeasing).
func (s *Shard) BeginDrain(ctx context.Context, choose TargetChooser, deadline time.Duration) (res DrainResult, err error) {
	s.mu.Lock()
	s.draining = true // reject new fresh logins from here on (a handoff bind is still accepted)
	s.mu.Unlock()
	// Advertise to the fleet that we are draining (#41), so a peer draining at the same moment does not pick
	// US as its target. The marker's TTL (2x the deadline) is the crash backstop; it is cleared on return
	// below. Best-effort — a marker write failure must not block the drain (we still reject fresh logins).
	if s.drainMarker != nil {
		if merr := s.drainMarker.SetDraining(ctx, s.shardID, 2*deadline); merr != nil {
			slog.Warn("drain: could not publish draining marker; peers may still target this shard", "err", merr)
		}
	}
	// If the drain ABORTS (a target choice / handover failed before completing), clear the flag so the shard
	// resumes accepting logins for whatever zones it still owns rather than getting stuck rejecting them. The
	// directory marker is cleared on EITHER outcome (on success the process exits, but clear it anyway so a
	// resumed/aborted shard isn't left ineligible; the TTL is only the crash backstop).
	defer func() {
		if s.drainMarker != nil {
			_ = s.drainMarker.ClearDraining(context.Background(), s.shardID)
		}
		if err != nil {
			s.mu.Lock()
			s.draining = false
			s.mu.Unlock()
		}
	}()

	// Local bootstrap zones (#212 core pack) are hosted unleased on EVERY shard, so there is no
	// ownership to hand to a peer (the target already built its own copy) — exclude them from the
	// drain's handoff + redirect accounting. Their players are not redirected (there is nowhere to
	// redirect them to); a clean shutdown still durably flushes them via s.Drain() below.
	//
	// INSTANCES are excluded from the HANDOVER, by their OWN predicate rather than by widening isLocalZone —
	// the two zone classes are unleased for completely different reasons and every other site treats them
	// differently. An instance in the handover set is a guaranteed zero-drop violation on every SIGTERM:
	// handoverZoneTo has no lease to flip, and drainPlayer hands each occupant off IN PLACE to `z.id` — an
	// instance id no peer can resolve, let alone host — so every Prepare fails, every occupant stays resident,
	// and all of them are dropped as stragglers at the deadline.
	//
	// They are NOT excluded from the ACCOUNTING. That is the other half and it is just as load-bearing: an
	// instance stays in `zones`, so it is counted in `initial`, durably flushed by s.Drain, and told to
	// clean-disconnect + classify its residents in step 3 exactly like every other zone. Dropping instances
	// out of the set entirely would trade an OVER-count of Redirected (they were never redirectable) for an
	// UNDER-count that is far worse: their occupants would vanish from the tally, get no reclaim notice, and
	// be dropped with the process with nothing in the readout saying so. What an instance's occupant gets is a
	// clean reconnect from durable state — the drain's ordinary degraded outcome — not silence.
	//
	// Their occupants ARE walked out first — step 0 below (#72). That is what turns an instance occupant's
	// SIGTERM outcome from "dropped, reconnect from durable state" into the ordinary seamless redirect: once
	// they are standing in their anchor zone, they are an ordinary resident of an ordinary leased zone and the
	// handover below moves them with everyone else.
	zones := make([]*Zone, 0)     // the ACCOUNTING set: every zone this shard owes a flush + reclaim
	handover := make([]*Zone, 0)  // the subset with a lease to hand a peer: the redirect + quiescence set
	instances := make([]*Zone, 0) // the subset to walk back out to their exit anchors first
	for _, z := range s.zonesList() {
		if s.isLocalZone(z.id) {
			continue
		}
		zones = append(zones, z)
		if z.isInstance() {
			instances = append(instances, z)
			continue
		}
		handover = append(handover, z)
	}

	// 0. Walk every instance's occupants back out to the zone+room they entered from (#72), BEFORE any lease
	// moves and before the population snapshot below.
	//
	// The ORDER is the whole design. Ejecting first means an occupant is counted in — and redirected from —
	// the anchor zone they are actually standing in when step 1 runs, so the tally needs no special case and
	// the player needs no special handling: they are simply a resident of a drainable zone. Ejecting after the
	// snapshot would count them twice; ejecting after step 1 would push them into a zone whose lease had
	// already gone to a peer and whose own drain had already fanned its players off.
	//
	// It BLOCKS (see ejectInstanceOccupants), and that is not a cost to be optimized away — it is what bounds
	// the eject's claim window in time. It is bounded by its own barrier, selects on ctx throughout, and
	// degrades to the pre-#72 behavior (occupants reclaimed as stragglers) rather than to a stall.
	s.ejectInstanceOccupants(ctx, instances)

	// Snapshot the population AFTER the eject so Redirected = initial - stragglers-at-deadline stays honest:
	// an ejected player is now resident in their anchor zone and will be counted there. Still over `zones`, so
	// any occupant the eject could NOT move (no anchor, a wedged instance, a barrier timeout) is still in the
	// denominator and comes back out as Reclaimed in step 3 rather than vanishing from the tally.
	initial := int64(0)
	for _, z := range zones {
		initial += z.pop.Load()
	}

	// 1. Hand each zone's ownership to a peer, then tell the zone to fan its players off to it. Pass the
	// zone's live population so a load-aware chooser reserves that much headroom on the target (#41).
	// A per-zone choose/handover FAILURE is NOT fatal to the drain: it must never abort before the durable
	// flush in step 3 (a directory outage during SIGTERM would otherwise drop every resident player's last
	// delta). Instead, skip redirecting that zone — its players stay resident and are flushed + reclaimed
	// (reconnect from durable state) below. That converts "no peer / handover failed" from data loss into a
	// clean reconnect, which is the whole point of the drain ladder.
	// The DISTINCT targets this drain reserved headroom on, and whether any zone actually handed off to each.
	// Retired once per target at completion (#284) — never per zone: ReserveDrainTarget accumulates a
	// drainer's zones into ONE hash field, so a per-zone retire would wipe the sibling zones' reservations
	// for that drainer/target pair and under-count the headroom a concurrent drainer sees.
	//
	// The two cases retire DIFFERENTLY. A target that received players keeps its hold for about one presence
	// heartbeat (it is still bridging the blind window). A target that received none — its handover failed —
	// is holding headroom for players that will never arrive, so its hold goes at once.
	sentPlayers := map[string]bool{}
	defer func() { s.retireDrainTargets(sentPlayers) }()

	for _, z := range handover {
		targetID, targetAddr, cerr := choose(z.id, int(z.pop.Load()))
		if cerr != nil {
			slog.Warn("drain: no target for zone; its players will be reclaimed from durable state",
				"zone", z.id, "err", cerr)
			continue
		}
		// Record the target even if the handover below fails: `choose` already reserved headroom on it, and
		// an unretired reservation for an abandoned zone is exactly the stale hold #284 is about.
		if _, seen := sentPlayers[targetID]; !seen {
			sentPlayers[targetID] = false
		}
		if herr := s.handoverZoneTo(ctx, z.id, targetID, targetAddr); herr != nil {
			slog.Warn("drain: zone handover failed; its players will be reclaimed from durable state",
				"zone", z.id, "err", herr)
			continue
		}
		sentPlayers[targetID] = true
		z.post(drainZoneMsg{})
	}

	// 2. Wait until every zone has QUIESCED (players redirected) or the deadline elapses. Quiescence, not
	// `pop == 0`: a zone can read empty while it still owes a parked logout flush, or while a player is in
	// flight to it on the intra-shard transfer path (#409) — see allZonesQuiescent and Zone.quiescent. This
	// matches what RebalanceZone, the single-zone analog, has always waited for.
	//
	// The RESIDENCY wait is over `handover`, NOT `zones`: an instance was never redirected, so nothing will
	// ever make its pop fall and waiting on one would burn the whole deadline every time — starving the
	// durable flush in step 3, which is the part that actually protects the players in it.
	//
	// The `incoming` gate is over `zones`, INCLUDING instances, and that asymmetry is deliberate. Quiescence
	// is three counters and only two of them are about redirection; `incoming` is #409's in-flight
	// intra-shard ARRIVAL claim, taken by claimTransferTarget in the same mu hold that resolves the
	// destination. Excluding instances from it would say "nobody is in flight" while a live session is, and
	// step 3 would then order the flush + straggler reclaim AHEAD of the arrival — the player lands in a zone
	// that has already been flushed and disconnected and is dropped with the process, uncounted and
	// unflushed. That is exactly the failure allZonesQuiescent's doc-comment describes.
	//
	// Unreachable today (no entry path routes into an instance; that is slice 3, #72), and armed the moment
	// slice 3 points claimTransferTarget at an instance destination — which is why it is closed by
	// construction here rather than left as a note. It costs nothing: an OCCUPIED instance has pop > 0, not
	// incoming > 0, so the "don't wait on a pinned instance" property above is untouched.
	dl := time.After(deadline)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		if allZonesQuiescent(handover) && noArrivalsInFlight(zones) {
			break wait
		}
		select {
		case <-ctx.Done():
			break wait
		case <-dl:
			break wait
		case <-tick.C:
		}
	}

	// 3. Flush every zone durably (stragglers included), then have each zone clean-disconnect + classify the
	// stragglers still resident at the deadline (#43). The reclaim runs ON the zone goroutine (safe socket +
	// player-map access); post it AFTER Drain so the durable flush handler is ordered ahead of the disconnect
	// on each zone's FIFO inbox. The zone loops are still live here — worldCtx is cancelled only after
	// BeginDrain returns. The flush is only ENQUEUED to the async saver, not confirmed durable (a saver-drain
	// barrier is a tracked follow-up); the reclaim notice's wording is honest about that.
	//
	// This step runs over `zones`, the ACCOUNTING set, so it covers the instances the handover skipped: their
	// occupants are flushed durably and get a reclaim notice + a tally entry rather than disappearing.
	//
	// Both the post and the collect select on ctx so a zone whose loop has stopped consuming (a lease fence
	// cancelling worldCtx mid-drain, or a wedged handler) can never block shutdown past the drain deadline —
	// on either timeout the zone's residents are counted best-effort as infra-fault via the atomic pop.
	//
	// They ALSO select on z.dead, which is what z.post does and what a raw channel send here has to do by
	// hand (#411). A zone can be torn down between this drain's zonesList() snapshot and now: the instance
	// reaper rides runCtx and keeps sweeping during a drain — s.draining gates minting, not reaping — so an
	// empty instance is retired out from under us. Its inbox is still BUFFERED, so the send succeeds against
	// a stopped actor, and the collect then waits for a reply that nobody is left to send: the drain sits
	// there until the caller's context expires, which on SIGTERM is the whole shutdown deadline (~45s in
	// cmd/telos-world) spent on a shard with nothing left to do. Availability, not durability — the flush
	// barriers run on their own fresh contexts — which is precisely why it would read as "shutdown is slow".
	if dropped := s.Drain(ctx); dropped > 0 {
		slog.Warn("drain: some straggler flushes never reached the saver queue; those players will load stale state",
			"dropped", dropped)
	}
	resps := make([]chan reclaimTally, len(zones))
	for i, z := range zones {
		ch := make(chan reclaimTally, 1)
		select {
		case z.inbox <- reclaimStragglersMsg{resp: ch}:
			resps[i] = ch
		case <-z.dead:
			resps[i] = nil // torn down mid-drain; it is quiescent by UnhostZone's precondition, so nothing is owed
		case <-ctx.Done():
			resps[i] = nil // couldn't post; accounted in the collect loop below
		}
	}
	for i, z := range zones {
		if resps[i] == nil {
			res.ReclaimedInfra += int(z.pop.Load()) // never posted (ctx expired, or the zone is gone); best-effort
			continue
		}
		select {
		case t := <-resps[i]:
			res.ReclaimedInfra += t.infra
			res.ReclaimedClient += t.client
		case <-z.dead:
			// Torn down after we posted: nothing will drain that inbox. UnhostZone only removes a QUIESCENT
			// zone, so pop is 0 here and this adds nothing — it is the wait that had to end, not the count.
			res.ReclaimedInfra += int(z.pop.Load())
		case <-ctx.Done():
			// Posted but the zone didn't answer before the drain ctx expired; count its residents (pop, which
			// includes any pending arrival — a minor over-count acceptable in this degraded path) as infra.
			res.ReclaimedInfra += int(z.pop.Load())
		}
	}
	res.Reclaimed = res.ReclaimedInfra + res.ReclaimedClient
	res.Redirected = int(initial) - res.Reclaimed
	if res.Redirected < 0 {
		res.Redirected = 0 // players who quit during the drain aren't "redirected"; keep it non-negative
	}
	metrics.DrainRedirected(ctx, res.Redirected)
	metrics.DrainReclaimed(ctx, res.ReclaimedInfra, "infra")
	metrics.DrainReclaimed(ctx, res.ReclaimedClient, "client")
	return res, nil
}

// allZonesQuiescent reports whether every zone in zs has nothing left outstanding — no resident player, no
// parked logout flush, and no intra-shard transfer in flight toward it (Zone.quiescent). It is BeginDrain's
// wait-until-empty predicate, factored out so the three counters that make up "empty" can be pinned by a
// table-driven test rather than only through a timing-sensitive drain.
//
// `pop == 0` alone was the old predicate and is NOT emptiness. A player walking between two zones of THIS
// shard has already left the source's players map and only bumps the destination's pop when the destination
// dequeues their transferInMsg; for the width of that queue hop both zones read pop 0 while a live session is
// in flight (#409). Concluding "drained" there lets the durable flush + straggler reclaim in step 3 be
// ordered AHEAD of the arrival, so the player lands in a zone that has already been flushed and disconnected
// and is then dropped with the process — uncounted, unflushed. `stashed` is the same trap one hop back.
func allZonesQuiescent(zs []*Zone) bool {
	for _, z := range zs {
		if !z.quiescent() {
			return false
		}
	}
	return true
}

// noArrivalsInFlight reports whether NO zone in zs has an intra-shard transfer claimed toward it (#409). It is
// the one third of quiescence that BeginDrain applies to EVERY hosted zone rather than only to the ones it can
// hand a peer.
//
// The split exists because the three counters answer different questions. `pop` and `stashed` are about
// RESIDENCY, which for a zone the drain cannot redirect (an instance) will never fall — waiting on it just
// burns the deadline. `incoming` is about a live session that is ALREADY in flight to that zone on this shard,
// which has nothing to do with whether the zone is redirectable: concluding "drained" with one outstanding
// orders step 3's durable flush + straggler reclaim ahead of the arrival, and the player is dropped with the
// process, uncounted and unflushed.
func noArrivalsInFlight(zs []*Zone) bool {
	for _, z := range zs {
		if z.incoming.Load() != 0 {
			return false
		}
	}
	return true
}

// isDraining reports whether a graceful drain is in progress (fresh logins are refused). Guarded by mu.
func (s *Shard) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// retireDrainTargets retires the headroom this drain reserved, once per distinct target (#284). Called from
// BeginDrain's defer, so it runs on every exit path — success, abort, or a mid-drain error.
//
// A target that RECEIVED players has its hold shortened to about one presence heartbeat: it is still bridging
// the window before that target's heartbeat reports the migrated weight, and deleting it now would let a
// concurrent drainer read a stale low load and over-commit. A target that received NONE (its handover failed)
// is holding headroom for players that will never arrive, so its hold is dropped outright.
//
// Best-effort: the per-field TTL is the correctness backstop, and a drain that is exiting must not fail
// because Redis blinked. Each call gets its own short deadline, and a FRESH context: BeginDrain's ctx is
// typically already past its deadline by the time this defer runs (the drain deadline is what got us here),
// which would cancel the retire before it left the process.
func (s *Shard) retireDrainTargets(sentPlayers map[string]bool) {
	if s.drainReleaser == nil || s.shardID == "" || len(sentPlayers) == 0 {
		return
	}
	for target, sent := range sentPlayers {
		ctx, cancel := context.WithTimeout(context.Background(), drainReleaseTimeout)
		var err error
		if sent {
			var held bool
			held, err = s.drainReleaser.ExpireDrainTargetSoon(ctx, target, s.shardID, PresenceReflectWindow)
			if err == nil && !held {
				// The hold we are retiring was ALREADY GONE while this drain was still sending players to
				// this target. That is the precise signature of "our reservation lapsed before we finished"
				// — the concern raised in #384, which was closed on the argument that the 42s TTL exceeds
				// the ~12s presence-reflection bridge it covers by 30s regardless of how long the step-1
				// selection loop ran. This line is the check on that argument: it converts an unfalsifiable
				// theory into an operational fact. If it never fires, the reasoning held; if it does, #384
				// reopens with a measured duration attached rather than a hypothesis.
				slog.Warn("drain: the reservation on a target had already lapsed while we were still sending to it; "+
					"the drain outran its hold (see #384 — report this with the drain's duration)",
					"event", "drain_reservation_lapsed", "target", target, "drainer", s.shardID)
			}
		} else {
			err = s.drainReleaser.ReleaseDrainTarget(ctx, target, s.shardID)
		}
		if err != nil {
			slog.Warn("drain: could not retire the reservation on a target; it will expire on its own TTL",
				"target", target, "drainer", s.shardID, "sent_players", sent, "err", err)
		} else {
			slog.Debug("drain: retired target reservation", "target", target, "drainer", s.shardID, "sent_players", sent)
		}
		cancel()
	}
}

// drainReleaseTimeout bounds one reservation retire. Short: the shard is exiting, and the TTL already covers
// us if this never lands.
const drainReleaseTimeout = 2 * time.Second
