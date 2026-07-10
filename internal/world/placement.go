package world

import (
	"context"
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
// (shard, zone, epoch). None of those advance the epoch, which is why the directory exposes a separate
// RegisterPlacement that accepts an equal epoch rather than reusing the strictly-greater handoff CAS.
//
// WHY SAME-EPOCH ACCEPTANCE IS SAFE (the load-bearing invariant, distsys review): an epoch value maps to
// exactly ONE shard. Only the handoff coordinator bumps it, and exactly one casPlacement commits each bump,
// so between handoffs the player sits on a single shard and only intra-shard transfers run at a constant
// epoch. An equal-epoch write can therefore only ever rewrite the ZONE within the shard that already owns
// the player — never name a different shard. That is what makes "accept equal" incapable of rolling a
// player back to a shard they left.
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

// placementOp is one pending placement write: "playerID is resident in zoneID on this shard at epoch".
type placementOp struct {
	playerID string
	zoneID   string
	epoch    uint64
}

// placementWriteTimeout bounds one Redis round trip on the background worker, so a wedged directory cannot
// make the drain loop unresponsive to shutdown.
const placementWriteTimeout = 2 * time.Second

// placementWriter collects pending placement writes from the zone goroutines and drains them off-goroutine.
// pending is keyed by player, so a burst of zone changes for one player collapses to that player's latest
// placement. signal is a 1-slot doorbell: a full signal channel already means "there is work", so a
// non-blocking send is sufficient and can never block a caller.
type placementWriter struct {
	mu      sync.Mutex
	pending map[string]placementOp
	signal  chan struct{}
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

// take atomically removes and returns everything pending.
func (w *placementWriter) take() []placementOp {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	ops := make([]placementOp, 0, len(w.pending))
	for _, op := range w.pending {
		ops = append(ops, op)
	}
	w.pending = map[string]placementOp{}
	return ops
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
	z.shard.placement.offer(placementOp{playerID: s.character, zoneID: z.id, epoch: s.epoch})
}

// runPlacementWriter drains pending placements off every zone goroutine, performing the blocking Redis
// writes. Started by Shard.Run; exits with the shard's context.
//
// Dropping whatever is still pending at shutdown is safe: on a drain, the destination's own handoff CAS is
// the truth, and a queued source-shard registration drained late would be epoch-fenced against it anyway.
func (s *Shard) runPlacementWriter(ctx context.Context) {
	if s.dir == nil || s.placement == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.placement.signal:
			for _, op := range s.placement.take() {
				if ctx.Err() != nil {
					return
				}
				s.writePlacement(ctx, op)
			}
		}
	}
}

// writePlacement performs one RegisterPlacement round trip. Failures are logged, never retried: the record
// is rewritten on the player's next zone change or relog, and a retry loop here would queue behind a wedged
// Redis while players keep moving.
func (s *Shard) writePlacement(ctx context.Context, op placementOp) {
	wctx, cancel := context.WithTimeout(ctx, placementWriteTimeout)
	defer cancel()
	ok, err := s.dir.RegisterPlacement(wctx, op.playerID, s.shardID, op.zoneID, op.epoch)
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
