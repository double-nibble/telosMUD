package world

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// placement.go is the world's writer for the directory's per-player placement record (#320).
//
// The placement hash answers three questions: which shard hosts a player (handoff fencing), which ZONE
// they were last resident in (reconnect routing), and what their ownership epoch is (the monotonic CAS
// fence). Before #320 its ONLY writer was the cross-shard handoff CAS (Shard.beginHandoff), so a player
// who had never been handed off had no placement at all — unroutable on reconnect, and invisible to the
// tell/mail existence oracle, which answered "there is no player by that name" for them.
//
// This file adds the missing write: every time a player becomes RESIDENT in a zone on this shard — a fresh
// login, a link-dead resume, a cross-shard arrival, or an intra-shard zone transfer — we record
// (shard, zone, epoch). RegisterPlacement accepts an EQUAL epoch rather than reusing the strictly-greater
// handoff CAS, because several of those paths legitimately re-assert an epoch they already hold.
//
// WHY SAME-EPOCH ACCEPTANCE IS SAFE (the load-bearing invariant). The original derivation — "only the
// handoff coordinator bumps the epoch, and exactly one casPlacement commits each bump" — is NO LONGER
// TRUE as of #432, and leaving it here would have the next reader build on a false premise. A fresh
// LOGIN now bumps the epoch too (server.go), and it commits that bump through this accept-equal
// register, not through casPlacement.
//
// The conclusion survives on a stronger footing: every ownership claim — login and handoff alike — mints
// its epoch from ONE atomic counter, `characters.owner_epoch`, via CharacterStore.ClaimCharacter. No two
// claimants can ever receive the same value, so an epoch STILL maps to exactly one shard; it is now
// mint exclusivity that guarantees it rather than CAS exclusivity. An equal-epoch write therefore
// remains incapable of naming a different shard: only the single holder of that epoch can present it,
// and all it can rewrite is the ZONE within the shard that already owns the player.
//
// The write is BLOCKING Redis I/O and every call site runs on a zone goroutine, so it is handed to a
// background worker. The hand-off is a COALESCING map, not a FIFO queue: only a player's LATEST placement
// matters, so a slow directory collapses a burst into one write per player rather than dropping the tail.
// That distinction is load-bearing. A dropped *login* registration would be harmless — the player's prior
// record is still correct — but a dropped *zone-transfer* registration leaves the record naming the zone
// they walked out of, and if those two zones later land on different shards, the reconnect routes to a
// shard that cannot host their durable zone_ref and start-rooms them. That is exactly the data loss #320
// exists to kill, so we must not drop it. Coalescing bounds memory by RESIDENT COUNT (not by event rate)
// while never blocking an actor loop and never discarding a meaningful write.

// placementOp is one pending placement write. Either "playerID is resident in zoneID on this shard at
// epoch" (clear == false), or the clean-logout tombstone "playerID left this shard at epoch" (clear == true).
//
// Both ride the same coalescing map, and that is deliberate: if a player quits and immediately reconnects,
// the pending clear is REPLACED by the fresh registration rather than being applied after it. Last write
// wins, and the last write is the truth.
type placementOp struct {
	playerID string
	zoneID   string
	epoch    uint64
	nonce    uint64 // the session's per-session fence value (#329): stamped on register, matched on clear
	clear    bool
}

// placementWriteTimeout bounds one Redis round trip on the background worker, so a wedged directory cannot
// make the drain loop unresponsive to shutdown.
const placementWriteTimeout = 2 * time.Second

// placementWriter collects pending placement writes from the zone goroutines and drains them off-goroutine.
// pending is keyed by player, so a burst of zone changes for one player collapses to that player's latest
// placement. signal is a 1-slot doorbell: a full signal channel already means "there is work", so a
// non-blocking send is sufficient and can never block a caller.
//
// barriers is the shutdown-barrier queue (#331). A caller (FlushPlacement) registers a channel here and the
// drain loop closes it once it has written every op that was pending when the barrier was collected. Both
// offer and barrier registration take mu, and the drain loop collects pending ops AND barriers under the
// same lock, so any op offered before a barrier was registered is guaranteed written before that barrier
// closes. Without this, a clean-logout tombstone enqueued microseconds before stopWorld cancels the world
// context is dropped, leaving the record naming a shard that is exiting (the tell/mail oracle then reports
// the player as hosted on a dead shard until their next login).
type placementWriter struct {
	mu       sync.Mutex
	pending  map[string]placementOp
	barriers []chan struct{}
	signal   chan struct{}
}

func newPlacementWriter() *placementWriter {
	return &placementWriter{
		pending: map[string]placementOp{},
		signal:  make(chan struct{}, 1),
	}
}

// offer records op as the player's latest pending placement. Safe to call from a zone goroutine: it takes
// a short mutex, does no I/O, and never blocks (the doorbell send is non-blocking).
func (w *placementWriter) offer(op placementOp) {
	w.mu.Lock()
	w.pending[op.playerID] = op // last write wins: only the newest zone for a player matters
	w.mu.Unlock()
	select {
	case w.signal <- struct{}{}:
	default: // a pending doorbell already means "drain me"
	}
}

// take atomically removes and returns everything pending, plus every barrier registered so far. The two are
// taken under ONE lock so the caller can write the ops and only then close the barriers, giving the barrier
// its guarantee: every op that was pending when the barrier was collected is written before it closes.
func (w *placementWriter) take() ([]placementOp, []chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	barriers := w.barriers
	w.barriers = nil
	if len(w.pending) == 0 {
		return nil, barriers
	}
	ops := make([]placementOp, 0, len(w.pending))
	for _, op := range w.pending {
		ops = append(ops, op)
	}
	w.pending = map[string]placementOp{}
	return ops, barriers
}

// addBarrier registers a shutdown barrier and rings the doorbell so the drain loop wakes to collect it even
// when nothing is pending. The drain loop closes the returned channel after writing every op collected
// alongside it.
func (w *placementWriter) addBarrier() chan struct{} {
	ch := make(chan struct{})
	w.mu.Lock()
	w.barriers = append(w.barriers, ch)
	w.mu.Unlock()
	select {
	case w.signal <- struct{}{}:
	default:
	}
	return ch
}

// registerPlacement records that this player now lives in this zone on this shard. Safe to call from a zone
// goroutine — it never blocks and never does I/O. A no-op without a directory (single-shard/dev) or without
// a shard id.
//
// The shard id comes from WithZoneLeasing, which is the SAME axis routing depends on: gate zone-routing
// (ShardForZone) and cross-shard handoff (destShardID) both require zone leases. An unleased shard is not a
// coherent routing destination, so it has nothing meaningful to write here — do not "fix" this guard by
// substituting a different identity.
func (z *Zone) registerPlacement(s *session) {
	if z.shard == nil || z.shard.dir == nil || z.shard.shardID == "" || s == nil || s.character == "" {
		return
	}
	zoneID := placementZoneRef(z, s)
	if zoneID == "" {
		// The anchorless-instance fail-safe (see placementZoneRef). This site must SKIP rather than write the
		// empty ref, and the asymmetry with clearPlacement below is not a style choice — the two Lua scripts
		// treat an empty zone differently. `registerPlacement` HSETs the field UNCONDITIONALLY, so an empty ref
		// here would CLOBBER the stored zone with "" and destroy the very anchor we are trying to preserve;
		// `clearPlayerShard` guards the field on ARGV[2] ~= '', so there an empty ref genuinely means "leave it
		// alone". Skipping leaves the player's last good placement standing, which is #411's behavior and the
		// right degraded outcome.
		return
	}
	z.shard.placement.offer(placementOp{
		playerID: s.character, zoneID: zoneID, epoch: s.epoch, nonce: s.nonce,
	})
}

// placementZoneRef is the zone a placement write should NAME for this session — the shared rule for both the
// registration and the logout tombstone (#72), and one of the write-side halves of the instance-shaped
// durable-location guard (#411; see durableLocation for the durable row).
//
// The placement record is the reconnect-ROUTING spine since #320: the gate resolves a returning player by
// asking ShardForZone for the recorded zone. Its invariant is "the recorded zone is the zone that holds the
// session". An instance takes no lease and is in no directory at all, so a record naming one resolves to no
// shard and the reconnect dead-ends; and if it somehow DID resolve it would route a player into a private copy
// by id, one they may never have entered. So a player inside an instance records their exit ANCHOR.
//
// The anchor keeps the #320 invariant honest rather than bending it, which is why this is a positive write and
// not the #411 skip it replaces. Recording nothing at all (the #411 behavior) merely left the last good record
// standing, which was the anchor only by luck of the last walk; this makes it the anchor by intent, and keeps
// it CURRENT as the player walks between rooms of the instance.
//
// BE PRECISE ABOUT THE SHARD INVARIANT — it holds AT ENTRY and is not maintained afterwards.
//
// At entry it is true by construction: you enter an instance from a room, so the entrance zone is hosted here,
// and MintInstance builds the copy on this shard. So the anchor names a zone THIS shard hosts, ShardForZone
// resolves to us, and the shard holding the live instance is the one that answers a reconnect.
//
// NOTHING KEEPS IT TRUE FOR THE DURATION OF THE VISIT. The anchor zone is an ordinary leased zone and can be
// rebalanced or drained to a PEER while the player is inside the instance. The anchor is not updated when that
// happens (it names a zone id, and the id does not move), so from then on the record routes a reconnect to the
// PEER — which has no session for this character and fresh-logs them from durable state, while THIS shard is
// still holding the live one. That is the double-own shape, and the outcome is the classic one: two copies of
// the character, the loser's state discarded at save time.
//
// This is NOT a regression and not specific to instances: pre-#72 the record named the last authored zone the
// player stood in, and a rebalance of THAT zone produced exactly the same race. The degradation is identical;
// the anchor neither introduces nor worsens it. It is stated here only because the paragraph above used to
// assert the same-shard property flatly, which would let a future reader take it as a maintained safety
// invariant and build on it. It is an entry-time property that decays. The fence for it is filed separately.
//
// The "" fallback is for an anchorless instance occupant, which entry makes impossible. At the sink an empty
// zone means "leave the stored zone alone" (the clearPlayerShard script treats ARGV[2] == ” as a no-op on the
// field; RegisterPlacement likewise), so the degraded outcome is #411's — the last authored zone stands.
func placementZoneRef(z *Zone, s *session) string {
	if !z.isInstance() {
		return z.id
	}
	if s.anchorZone != "" {
		return s.anchorZone
	}
	// z.log already carries zone= and template= (newInstanceZone).
	z.log.Warn("placement write for a player inside an instance with NO EXIT ANCHOR (entry should have set "+
		"one); recording no zone rather than an ephemeral id that resolves to no shard", "player", s.character)
	return ""
}

// clearPlacement enqueues the clean-logout tombstone: drop this player's `shard` field, keeping their epoch
// (the handoff fence) and zone (the reconnect routing key). Safe to call from a zone goroutine.
//
// ONLY for a clean, player-initiated quit.
//
// NOT for link death. Note that this is NOT because reconnect routing needs the shard field — since #320
// the gate routes by ZONE (ShardForZone names the zone's current owner), so a tombstoned link-dead player
// would still be routed straight back to the shard holding their detached session. The reason is simpler
// and narrower: while the session is detached it is still HELD here, entity and all, for the whole grace.
// The record should say so. Tombstoning would claim the player is hosted nowhere while this shard is very
// much hosting them.
//
// NOT mid-handoff either: `detach` returns early on `frozen`, the destination owns the record by then, and
// the directory-side fence would reject us anyway.
//
// WHAT THE DIRECTORY FENCE DOES NOT COVER (distsys + test review). `clearPlayerShard` applies when the
// record still names (this shard, this epoch). A player who quits and relogs on the SAME shard resumes the
// SAME epoch — registerPlacement accepts an equal epoch by design — so a late-draining clear would match
// the live record on both axes and blank a connected player's shard field. The fence cannot see it.
//
// What makes that safe is ORDERING, not the fence: `detach` offers the clear and only then calls `leave`,
// so a relog's registration is always offered AFTER the clear; and one serial writer drains a per-player
// coalescing map, so the register either replaces the pending clear or is written strictly after it. Final
// state is the register, every time.
//
// That is a real invariant, and it is load-bearing. If a second writer of this record ever appears on the
// same shard, or the placement writer becomes concurrent, a stale clear could evict a live placement with
// no self-heal. Tracked in #329.
func (z *Zone) clearPlacement(s *session) {
	if z.shard == nil || z.shard.dir == nil || z.shard.shardID == "" || s == nil || s.character == "" {
		return
	}
	// Carry the zone. The writer coalesces per player, so a logout offered while a zone-change registration
	// is still pending REPLACES it — without this the tombstone would leave the record naming the zone the
	// player walked out of, and a later reconnect would route by that stale zone.
	//
	// From inside an instance this carries the exit ANCHOR (placementZoneRef, #72) and the tombstone STILL
	// FIRES. Both halves matter, and they are why this site cannot simply mirror whatever registerPlacement
	// does:
	//
	//   - It must never skip. The clear is what stops the record from claiming a shard that is exiting still
	//     hosts this player; skipping it is a stale-routing bug of its own.
	//   - It must never carry z.id. ClearPlayerShard deliberately PRESERVES `zone` across the tombstone
	//     because it is the reconnect routing key, so writing an instance id here is the one write on this
	//     path that OUTLIVES both the session and the instance. The instance is reaped seconds later,
	//     ShardForZone then finds no lease for it, and per the #320 policy the gate does not fall back to
	//     place.ShardID — so the player would route by home zone until some future registerPlacement happened
	//     to overwrite the field. Nothing self-heals it in between.
	//
	// The anchor is a durable authored ref, so it is safe to OUTLIVE the instance in a way the instance id
	// never was: it names a real leased zone that resolves for as long as the content defines it. A quit from
	// inside a dungeon therefore comes back at the dungeon door.
	zoneID := placementZoneRef(z, s)
	z.shard.placement.offer(placementOp{playerID: s.character, zoneID: zoneID, epoch: s.epoch, nonce: s.nonce, clear: true})
}

// runPlacementWriter drains pending placements off every zone goroutine, performing the blocking Redis
// writes. Started by Shard.Run; exits with the shard's context.
//
// Dropping whatever is still pending on a HARD stop (ctx cancelled without a barrier — a lease fence) is
// safe for REGISTRATIONS: on a drain the destination's own handoff CAS is the truth, and a queued
// source-shard registration drained late would be epoch-fenced against it anyway. It is NOT safe for the
// clean-logout TOMBSTONE — a dropped clear leaves the record naming a shard that is exiting — which is why a
// graceful shutdown calls FlushPlacement before stopWorld to drain the queue through the barrier first (#331).
func (s *Shard) runPlacementWriter(ctx context.Context) {
	if s.dir == nil || s.placement == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.placement.signal:
			ops, barriers := s.placement.take()
			for _, op := range ops {
				if ctx.Err() != nil {
					// Do not close the barriers: a hard-cancelled writer cannot honor the "everything before
					// me is written" promise. FlushPlacement's ctx/dead select unblocks its caller instead.
					return
				}
				s.writePlacement(ctx, op)
			}
			// Re-check AFTER the writes: a cancel during the LAST op's write (writePlacement returns early on a
			// dead ctx) leaves the top-of-loop guard unreached, so without this a torn-down drain would still
			// close the barrier and hand FlushPlacement a false success. A cancel here means at least one op may
			// not have landed — leave the barrier for the dead-channel watch.
			if ctx.Err() != nil {
				return
			}
			for _, b := range barriers {
				close(b) // every op collected alongside this barrier was attempted before the barrier closes
			}
		}
	}
}

// FlushPlacement blocks until every placement op enqueued so far has been written, or ctx expires, or the
// writer's run context is already cancelled (#331).
//
// Call it between the drain and the shutdown that cancels the world context, alongside FlushSaver. The
// writer returns on ctx cancel WITHOUT draining its pending map, so without this barrier a clean-logout
// tombstone enqueued last — microseconds before stopWorld — is thrown away, and the placement record keeps
// naming a shard that is exiting. That stale row makes the tell/mail existence oracle report the player as
// hosted on a dead shard until their next login rewrites it.
//
// It watches the shard's run context as well as the caller's: a lease fence can cancel worldCtx and stop the
// writer, and without that watch the barrier's wait would stall for the caller's whole timeout on a path
// that has nothing left to flush. A no-op without a directory (nothing is ever enqueued).
func (s *Shard) FlushPlacement(ctx context.Context) error {
	if s.dir == nil || s.placement == nil {
		return nil
	}
	s.mu.Lock()
	runCtx := s.runCtx
	s.mu.Unlock()
	var dead <-chan struct{}
	if runCtx != nil {
		dead = runCtx.Done()
	}
	barrier := s.placement.addBarrier()
	select {
	case <-barrier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-dead:
		return errors.New("placement writer stopped before the barrier completed")
	}
}

// writePlacement performs one RegisterPlacement round trip. Failures are logged, never retried: the record
// is rewritten on the player's next zone change or relog, and a retry loop here would queue behind a wedged
// Redis while players keep moving.
func (s *Shard) writePlacement(ctx context.Context, op placementOp) {
	wctx, cancel := context.WithTimeout(ctx, placementWriteTimeout)
	defer cancel()

	if op.clear {
		ok, err := s.dir.ClearPlayerShard(wctx, op.playerID, s.shardID, op.zoneID, op.epoch, op.nonce)
		switch {
		case err != nil:
			// Fail-safe: the record keeps naming this shard. The player's next login rewrites it, and until
			// then a reconnect routes here — which is exactly the pre-#70 behavior.
			slog.Warn("placement tombstone failed", "component", "world",
				"player", op.playerID, "zone", op.zoneID, "epoch", op.epoch, "err", err)
		case !ok:
			// The fence rejected us: the record no longer names this shard at this epoch, because the player
			// already re-registered somewhere (a fast relog) or was handed off. Either way, not ours to clear.
			slog.Debug("placement tombstone fenced out", "component", "world",
				"player", op.playerID, "shard", s.shardID, "epoch", op.epoch)
		default:
			slog.Debug("placement tombstoned on clean logout", "component", "world",
				"player", op.playerID, "shard", s.shardID, "epoch", op.epoch)
		}
		return
	}

	ok, err := s.dir.RegisterPlacement(wctx, op.playerID, s.shardID, op.zoneID, op.epoch, op.nonce)
	switch {
	case err != nil:
		slog.Warn("placement write failed", "component", "world",
			"player", op.playerID, "zone", op.zoneID, "epoch", op.epoch, "err", err)
	case !ok:
		// A NEWER epoch owns the record — an in-flight handoff moved the player out from under this write.
		// Correct and expected; the destination's own registration is the truth.
		slog.Debug("placement write superseded by a newer epoch", "component", "world",
			"player", op.playerID, "zone", op.zoneID, "epoch", op.epoch)
	default:
		slog.Debug("placement recorded", "component", "world",
			"player", op.playerID, "shard", s.shardID, "zone", op.zoneID, "epoch", op.epoch)
	}
}
