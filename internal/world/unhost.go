package world

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// unhost.go — the runtime zone-REMOVE primitive (#288), the inverse of HostZone's runtime zone-add.
//
// A coordinator rebalance moves a zone's players AND its lease from shard A to shard B. Before this, A kept
// the zone OBJECT forever: an empty, unowned, un-renewed zone whose actor goroutine went on pulsing its
// heartbeat, running resets, and holding a Lua VM. Inert — the lease is not renewed so there is no
// double-write, and the directory points at B so nothing routes to A's copy — but a shard that lives through
// many rebalances accumulates one zombie per migration. BeginDrain never cared (the process exits), but
// RebalanceZone leaves the source running.
//
// Tearing a zone down is easy to get wrong in ways that ARE correctness bugs, so the primitive is deliberately
// narrow. See UnhostZone's preconditions.
//
// This could not ship before #315. HostZone's idempotent early return — "already hosted, do nothing" — was
// what kept a replayed AdoptZone harmless, and it only holds because s.zones is never pruned. Pruning it while
// a captured AdoptZone was still replayable would have let an attacker REBUILD a torn-down zone. #315 bound
// the signature to the zone's lease generation, so a replay is now refused at the door and the early return
// stops being load-bearing.

// unhostActorGrace bounds how long UnhostZone waits for a zone's actor goroutine to return after its context
// is cancelled. The loop only has to finish the message or pulse it is mid-way through, so this is generous;
// exceeding it means the zone goroutine is wedged, which is a bug worth surfacing rather than hiding. Var for
// tests.
var unhostActorGrace = 10 * time.Second

// armZoneActorLocked derives a per-zone cancellable context from the shard's run ctx and records its cancel +
// a done channel, so ONE zone's actor can be stopped without stopping the shard. The goroutine it is armed for
// must `defer cancel()` (releasing the parent's child reference) and `defer s.disarmZoneActor(zoneID, done)`.
//
// The caller must hold mu, and must arm in the SAME lock hold that publishes the zone into s.zones. The
// invariant "in s.zones ⟺ actor armed and cancellable" has to be atomic: HostZone does slow work (a blocking
// scope subscribe) between publishing the zone and launching its actor, and an UnhostZone landing in that
// window would find no cancel, skip both the cancel and the wait, and report the zone gone — while HostZone
// went on to launch an actor for a zone nobody can ever stop. That is the #288 orphan goroutine, recreated by
// the primitive that exists to prevent it.
func (s *Shard) armZoneActorLocked(parent context.Context, zoneID string) (context.Context, context.CancelFunc, chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.actorStop[zoneID] = cancel
	s.actorDone[zoneID] = done
	return ctx, cancel, done
}

// disarmZoneActor is deferred by the actor goroutine itself: it announces that the goroutine has returned (so
// the UnhostZone waiting on it can stop) and drops the bookkeeping.
//
// It takes its OWN done channel and clears the map entries only while they still point at it. A zone can be
// torn down and re-hosted under the same id, and HostZone may arm the successor's actor before this
// predecessor's goroutine has finished unwinding. Deleting by id alone would then evict the successor's
// entries — leaving a live zone with no cancel and no done channel, so the NEXT UnhostZone would stall for
// the whole grace period and give up on a zone whose actor was perfectly healthy.
func (s *Shard) disarmZoneActor(zoneID string, done chan struct{}) {
	s.mu.Lock()
	if s.actorDone[zoneID] == done {
		delete(s.actorDone, zoneID)
		delete(s.actorStop, zoneID)
	}
	s.mu.Unlock()
	close(done)
}

// UnhostZone stops one zone's actor and removes it from this shard: the inverse of HostZone. It is idempotent
// (a zone this shard does not host is a nil no-op) and safe to call while the shard keeps serving its other
// zones.
//
// PRECONDITIONS, all enforced, all refusals:
//
//   - This shard must NOT be the zone's live owner in the directory. Tearing down a zone we still own would
//     leave the lease renewing under nothing, and every player the directory routes here would be told the
//     zone is not hosted. The caller must have handed the lease away FIRST (handoverZoneTo), which is exactly
//     the order RebalanceZone uses. With no leaser configured (single-shard/dev) there is no ownership to
//     check and the caller is trusted.
//   - The zone must be QUIESCENT: no resident players, and no create-window logout snapshot still parked
//     waiting for its CreateCharacter to return. A resident has session state only the zone goroutine may
//     touch; there is no correct way to evict them from here. A parked snapshot is a durable write this zone
//     still owes — the createdMsg that replays it is delivered to THIS actor's inbox, so stopping the actor
//     first loses it silently. `pop == 0` alone is NOT emptiness; see Zone.quiescent. Drain first
//     (drainZoneMsg + reclaimStragglers), and let the create round-trip finish.
//   - It must not be a local bootstrap zone (#212 core pack, hosted unleased on every shard) or this shard's
//     home zone. A local zone is unleased and every shard hosts its own copy, so no rebalance ever moves it.
//     The home zone is where the Play login path falls back when it cannot honor a player's durable zone; with
//     it gone that path returns Unavailable, and the gate does not currently re-resolve on an Unavailable at
//     Attach — it drops the player silently (#324). So unhosting home would turn a shard that is still
//     accepting logins into a black hole. Once #324 lands this guard can be reconsidered.
//
// A late message posted to a torn-down zone is harmless ONCE the zone is quiescent: the inbox stays buffered,
// so `post` does not block, and every reply-bearing message's sender already selects on its own context (the
// saver's presence probe treats a timeout as "player gone", which is the safe default). The message is simply
// never handled. The quiescence precondition is what makes that true — a createdMsg carrying a parked durable
// write is precisely a late message that is NOT harmless.
//
// The zone's memory is freed promptly but not instantly: a pending-TTL or link-death time.AfterFunc armed for
// a player who has since departed still captures the zone and keeps it reachable until it fires (bounded by
// linkDeadGrace). That is a one-shot delay, not the per-migration accumulation this closes.
func (s *Shard) UnhostZone(ctx context.Context, id string) error {
	s.mu.Lock()
	z := s.zones[id]
	if z == nil {
		s.mu.Unlock()
		return nil // not hosted: idempotent
	}
	if s.isLocalZone(id) { // localZones is construction-immutable (WithLocalZones runs before Run)
		s.mu.Unlock()
		return fmt.Errorf("unhost %q: refusing to remove a local bootstrap zone", id)
	}
	if id == s.home {
		s.mu.Unlock()
		return fmt.Errorf("unhost %q: refusing to remove this shard's home zone (the login fallback)", id)
	}
	s.mu.Unlock()

	// Ownership check OFF mu (it does directory I/O). A read failure fails closed: leaving a zombie zone is a
	// leak, tearing down a zone we may still own is a correctness break.
	if s.leaser != nil {
		owner, _, err := s.leaser.ZoneLease(ctx, id)
		if err != nil {
			return fmt.Errorf("unhost %q: read lease owner: %w", id, err)
		}
		if owner == s.shardID {
			return fmt.Errorf("unhost %q: this shard still owns the zone's lease — hand it over first", id)
		}
		// An UNOWNED zone is not automatically safe to drop. A lapsed lease reads as ownerless, and if we are
		// still renewing this zone we are about to reclaim it on the next tick — so "not us" would be read as
		// "not ours", we would cancel that renewer, and the zone would stay ownerless with nobody serving it.
		// A zone we deliberately handed away has had its renewal stopped and its handedOff flag set; that, not
		// the absence of an owner, is what makes it ours to drop.
		if owner == "" && s.renewingLocked(id) {
			return fmt.Errorf("unhost %q: the lease is unowned but this shard is still renewing it — "+
				"hand it over first", id)
		}
	}

	// Re-take mu and do the removal atomically against a concurrent HostZone/zoneByID. The population check
	// belongs INSIDE the lock: a player attaching resolves the zone through zoneByID under this same mutex, so
	// once we have removed the zone from s.zones no new attach can find it, and any attach that already found
	// it has bumped pop.
	//
	// CAREFUL: "any attach that already found it has bumped pop" is NOT true as stated, for any path that
	// resolves the zone under mu and then delivers ASYNCHRONOUSLY through the inbox — pop is bumped by the
	// handler, not by the resolve. The intra-shard transfer path is fixed here: claimTransferTarget takes an
	// `incoming` claim in the same mu hold as the resolve, and quiescent() folds it in (#409). Login attach
	// (server.go, zoneByID then zone.post(attachMsg)) and cross-shard Prepare (handoff_server.go) have the
	// same shape and are NOT yet covered — see the follow-up issue. Do not add a third such path without a
	// claim.
	s.mu.Lock()
	if s.zones[id] != z {
		s.mu.Unlock()
		return nil // a concurrent UnhostZone/HostZone got here first
	}
	if !z.quiescent() {
		s.mu.Unlock()
		return fmt.Errorf("unhost %q: not quiescent (%d resident player(s), %d parked logout flush(es), "+
			"%d inbound transfer(s) in flight) — drain the zone first",
			id, z.pop.Load(), z.stashed.Load(), z.incoming.Load())
	}
	delete(s.zones, id)
	// A pending handoff token indexed to this zone can never be bound now; drop it rather than leave the index
	// pointing at a stopped actor.
	for token, tz := range s.tokenIndex {
		if tz == z {
			delete(s.tokenIndex, token)
		}
	}
	// The residency index (#321) is kept clean by the quiescence precondition above: pop==0 => z.players is
	// empty => no residentZone entry points here. This sweep is belt-and-suspenders so a future change that
	// ever unhosts a non-quiescent zone can't leave the index pointing at a stopped actor — mirroring the
	// tokenIndex sweep. residentZone has its own lock, taken briefly here under s.mu (leaf order preserved:
	// residentMu never reaches back for s.mu).
	s.residentMu.Lock()
	for character, rz := range s.residentZone {
		if rz == z {
			delete(s.residentZone, character)
		}
	}
	s.residentMu.Unlock()
	// Clear the handed-off flag WITH the zone. It exists to tell this shard's renewal loop "stop renewing, do
	// not fence" for a zone we gave away. Leaving it set outlives the zone object, and a later re-adoption
	// takes HostZone's full BUILD path (the zone is gone, so the re-adoption branch that clears the flag never
	// runs) — its fresh renewal loop would then read the stale flag and bail on its first tick, hosting and
	// serving a zone whose lease it never renews. That is the #288 split-brain, re-entered through the door we
	// just opened.
	delete(s.handedOff, id)
	stopLease := s.leaseStop[id]
	delete(s.leaseStop, id)
	stopActor := s.actorStop[id]
	done := s.actorDone[id]
	// Drop the zone from region-scope delivery in the SAME lock hold that removed it from s.zones. Outside the
	// lock, a HostZone re-adopting this id could publish and re-register its replacement first, and our delete
	// would then evict the LIVE zone's region mapping — silently cutting it out of region-scope deltas.
	s.scopes.unregisterZone(id)
	s.mu.Unlock()

	if stopLease != nil {
		stopLease() // idempotent; markZoneHandedOff normally stopped renewal at the flip
	}
	if stopActor != nil {
		stopActor()
	}

	// Wait for the actor to return. The zone goroutine tears down its Lua VM in Run's defer, on the only
	// goroutine that ever touched it, so "the zone is gone" is not true until this closes.
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("unhost %q: %w waiting for the zone actor to stop", id, ctx.Err())
		case <-time.After(unhostActorGrace):
			return fmt.Errorf("unhost %q: zone actor did not stop within %s", id, unhostActorGrace)
		}
	}
	// The actor is gone for good, so nothing will ever drain this inbox again. Announce it, and every later
	// `post` abandons its send instead of filling the buffer and then blocking a sender forever — including
	// the saver's shared drainer, which acks back into a zone inbox WITHOUT a context to bail on.
	close(z.dead)
	slog.Info("unhosted zone at runtime", "zone", id, "shard", s.addr)
	return nil
}

// renewingLocked reports whether this shard is actively renewing zoneID's lease — i.e. it has a live renewal
// goroutine and has not deliberately handed the zone away. Caller holds mu.
func (s *Shard) renewingLocked(zoneID string) bool {
	_, renewing := s.leaseStop[zoneID]
	return renewing && !s.handedOff[zoneID]
}

// unadoptTimeout bounds a compensating un-adopt (#327). It runs on a context of its own — the caller's is,
// by definition, already cancelled — and only has to outlast a directory read plus the zone actor's last
// message. Var for tests.
var unadoptTimeout = 15 * time.Second

// unadoptZone tears down a zone this shard adopted for a handover that never landed (#327). Its sole caller is
// the adopting renewer (renewZoneLease), which invokes it when the adoption fails to confirm within
// adoptConfirmDeadline — because the source's drain deadline elapsed mid-RPC (it returned before its
// HandoverZone flip and kept the lease), or the source died mid-drain, or it lost a race. Either way this
// shard is left hosting a zone that no directory entry points to, whose actor runs resets and subscribes to
// scope forever. Single-writer is never violated (the lease stayed with the source), which is why this is a
// leak rather than a correctness bug — and why it can be best-effort.
//
// It retires the zone's renewal FIRST. That is safe precisely because the caller IS that renewal, returning: a
// landed flip would have set confirmed and skipped this path, so an unadopted zone is by definition one whose
// adoption never confirmed — this shard never held its lease, there is nothing to reclaim, and retiring the
// registration is what lets it past UnhostZone's "still renewing => about to reclaim => refuse" guard.
//
// It takes its own context: the caller's renewal ctx is being cancelled by the retire above. It logs rather
// than returns, because there is nothing the renewer can do about a failure here — a refusal means the zone
// acquired players or owes a durable write, in which case keeping it is the correct outcome.
func (s *Shard) unadoptZone(id, why string) {
	s.retireZoneRenewal(id)
	ctx, cancel := context.WithTimeout(context.Background(), unadoptTimeout)
	defer cancel()
	if err := s.UnhostZone(ctx, id); err != nil {
		slog.Warn("could not un-adopt an abandoned zone; its actor goroutine lives until restart",
			"zone", id, "reason", why, "err", err)
		return
	}
	slog.Info("un-adopted an abandoned zone", "zone", id, "reason", why)
}
