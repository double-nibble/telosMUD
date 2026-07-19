package world

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// saver.go is the async write path of the durability ladder (docs/PHASE4-PLAN.md §4). It is the
// ONLY place character I/O happens on the write side, and it NEVER runs on a zone goroutine —
// mirroring beginHandoff's spawned-goroutine pattern. The zone goroutine PRODUCES a CharSnapshot
// (dumpCharacter, on-goroutine, race-free) and hands it to this saver over a buffered channel;
// the saver does the blocking Redis checkpoint + Postgres CAS off-goroutine and posts the result
// back to the zone inbox as a saveConflictMsg / saveOkMsg — it never mutates entity state itself.
//
// # Durability ladder (each tier off the zone goroutine)
//
//	shard memory (authoritative, zone goroutine)
//	   │  ~10s  -> Redis checkpoint  (shrinks the crash window)
//	   ▼  ~60s / logout / drain -> Postgres CAS (durable record)
//
// The reason field of a save request tells the saver which tiers to write: a cheap ~10s tick
// hits only Redis; a ~60s tick / logout / drain hits Postgres too (and a fresh checkpoint).

// saveReason names why a save was requested, which selects how far down the ladder it writes.
type saveReason int

const (
	// saveCheckpoint is the cheap ~10s tier: write the Redis checkpoint only. It shrinks the
	// crash window without paying a Postgres write every 10s.
	saveCheckpoint saveReason = iota
	// saveFlush is the durable tier: write the Redis checkpoint AND the Postgres CAS. Fired on
	// the ~60s cadence and on shard drain — a flush of a still-LIVE player. On a CAS miss it
	// posts a saveConflictMsg so the live session re-dumps current state and re-enqueues
	// (Zone.saveConflict): the zone goroutine is the authority while the player is present.
	saveFlush
	// saveFinal is the LOGOUT/leave flush — the player is being removed from the zone in the
	// same handler that enqueued this save, so by the time a CAS miss would post a conflict back
	// the session may be GONE and the zone could not re-dump it (the data would be silently lost;
	// see docs/PERSISTENCE.md §6 and the durability-ladder note in saver.handle). A final flush
	// therefore carries DATA the saver owns off-goroutine and, on a CAS miss, re-reads the current
	// version, rebases this snapshot onto it, and retries the CAS in place (bounded) rather than
	// bouncing it back to a session that no longer exists.
	//
	// The rebase is bounded to CONTENTION, and only within one ownership epoch. That bound is the
	// #432 fix, and it is worth stating what the code used to believe instead: the zone PROBE
	// (finalizeFlush -> zonePresent, reading this process's z.players) was treated as the safety
	// property — "if the character is gone here, the rebase is safe." It cannot be. It answers only
	// for THIS process, so a live session on another shard was structurally invisible to it, and both
	// shards independently concluded their own z.players was the authority. The rebase then made the
	// state_version CAS succeed by construction, and a stale shard's 60-second-old logout snapshot
	// force-wrote over the live owner's state — a rollback, and a duplication primitive built on it.
	//
	// The fence is now at the SINK: the save carries the session's owner_epoch and the store applies
	// only `WHERE owner_epoch <= $k`. A rebase moves state_version and never the epoch, so it cannot
	// reach around that conjunct. An epoch loss is DEFINITIVE — the writer is a zombie, its data is
	// discarded, and the zone is told (saveNotOwnerMsg). The zone probe survives as an optimization
	// for the same-shard re-attach case, nothing more.
	//
	// The pre-existing cross-shard zombie exclusion still holds independently: a final flush only ever
	// runs for a player leaving THIS shard via leave(); a handed-off character is removed by
	// freezeExpire WITHOUT a save, never reaching this path.
	saveFinal
)

// flush reports whether this reason writes the durable Postgres tier (not just the Redis
// checkpoint). Both the cadence flush and the final logout flush do.
func (r saveReason) flush() bool { return r == saveFlush || r == saveFinal }

// saveRequest is one unit of work for the saver: a snapshot produced on the zone goroutine plus
// the zone to post the result back to and why it was requested. id routes the result message to
// the right session in that zone.
type saveRequest struct {
	snap   CharSnapshot
	zone   *Zone
	id     string
	reason saveReason
	// barrier makes this request a SENTINEL rather than a save (#282). The drainer closes it on dequeue
	// and never passes the request to handle. Because one goroutine drains a FIFO queue, dequeuing the
	// sentinel proves every request enqueued before it has already completed its I/O. A dedicated field,
	// not a magic saveReason: reason is switched on in several places and a value outside its iota range
	// would be one forgotten `default` away from writing a zero-valued character row.
	barrier chan struct{}
}

// saver owns the character store + checkpointer and a buffered request channel drained by a
// single background goroutine. It is created once per shard (newSaver) and shared by every hosted
// zone; the buffered channel + a single drainer keep writes ordered per shard without ever
// blocking a zone goroutine. nil store AND nil checkpointer => the saver is a no-op (ephemeral
// characters), which is how a storeless boot keeps today's behavior.
type saver struct {
	store CharacterStore // Postgres tier (nil => no durable record)
	ckpt  Checkpointer   // Redis tier (nil => no checkpoint mirror)
	reqs  chan saveRequest
	log   *slog.Logger
}

// saveQueueDepth bounds the saver's buffered channel. A full queue means the saver is wedged on
// slow I/O; rather than block the zone goroutine (which would stall every player on it) the
// enqueue drops the request and logs at Debug — a dropped checkpoint only widens the crash window
// by one tick, and the next tick re-enqueues a fresh snapshot. The headline correctness (a clean
// quit's flush) takes the same non-blocking path, but is rare and the queue is sized to absorb a
// burst of logouts. Real backpressure is a later concern (matches session.send's drop policy).
const saveQueueDepth = 256

// newSaver builds a saver over the given store and checkpointer. Either may be nil: a nil store
// disables the Postgres tier, a nil checkpointer the Redis tier; both nil makes every save a
// no-op (ephemeral). The drainer goroutine is started by run (called from Shard.Run) so a bare
// test that never runs the shard incurs no goroutine.
func newSaver(store CharacterStore, ckpt Checkpointer) *saver {
	return &saver{
		store: store,
		ckpt:  ckpt,
		reqs:  make(chan saveRequest, saveQueueDepth),
		log:   slog.With("component", "saver"),
	}
}

// enabled reports whether any durable tier is configured. A disabled saver short-circuits every
// enqueue so a storeless shard does zero work and behaves exactly as before slice 4.2.
func (sv *saver) enabled() bool { return sv != nil && (sv.store != nil || sv.ckpt != nil) }

// enqueue hands a save request to the drainer WITHOUT blocking the zone goroutine. If the queue
// is full the request is dropped (logged at Debug) rather than stalling the actor loop — a single
// slow store must never wedge a zone. A disabled saver drops silently. Called only from the zone
// goroutine (leave/quit, the pulse callback), so the snapshot read upstream is race-free.
func (sv *saver) enqueue(req saveRequest) {
	if !sv.enabled() {
		return
	}
	select {
	case sv.reqs <- req:
	default:
		sv.log.Debug("save request dropped: saver queue full", "player", req.id, "reason", req.reason)
	}
}

// run drains the request queue on a single background goroutine until ctx is cancelled. Each
// request does its blocking I/O here, OFF every zone goroutine. Started once by Shard.Run.
func (sv *saver) run(ctx context.Context) {
	if !sv.enabled() {
		return // no durable tier: nothing to drain
	}
	sv.log.Debug("saver loop start", "store", sv.store != nil, "checkpoint", sv.ckpt != nil)
	for {
		select {
		case <-ctx.Done():
			sv.log.Debug("saver loop stop")
			return
		case req := <-sv.reqs:
			if req.barrier != nil {
				// FIFO + single drainer: everything queued before this sentinel is already written.
				close(req.barrier)
				continue
			}
			sv.handle(ctx, req)
		}
	}
}

// enqueueCtx is the BLOCKING, ctx-bounded enqueue used by the drain path. Unlike enqueue, it does not drop
// on a full queue — a dropped drain flush is exactly the data loss #282 exists to prevent, and dropping it
// silently while a barrier reports success would be worse than not having a barrier at all. It reports
// whether the request made it in.
//
// The ctx bound is what keeps this safe. A zone goroutine blocking here while the drainer blocks posting a
// saveConflictMsg into that same zone's full inbox would deadlock; the drain deadline breaks the cycle.
func (sv *saver) enqueueCtx(ctx context.Context, req saveRequest) bool {
	if !sv.enabled() {
		return true // nothing to write; not a loss
	}
	select {
	case sv.reqs <- req:
		return true
	case <-ctx.Done():
		sv.log.Warn("drain save dropped: saver queue full and the drain deadline expired",
			"player", req.id, "reason", req.reason)
		return false
	}
}

// flush blocks until every request already in the queue has been written, or ctx expires, or the drainer is
// gone (dead closed).
//
// It exists because the drainer returns on ctx cancel WITHOUT draining its buffer. On a graceful drain the
// reclaimed stragglers' flush is enqueued LAST, microseconds before shutdown cancels that context — so the
// one cohort whose only durability path is this flush was exactly the cohort most likely to have it thrown
// away. Redirected players are unaffected (their state crosses in the handoff snapshot, not via the saver).
//
// The sentinel is enqueued with a BLOCKING send, not saver.enqueue's drop-on-full: a dropped barrier would
// make the caller wait out its whole timeout for a signal that can never come. `dead` covers the other side
// of that — if the drainer has already exited (a lease fence cancelled the world context mid-drain), the
// send would otherwise block for the caller's whole timeout on a shutdown path with nothing to flush.
//
// RESIDUAL, deliberately not closed (distsys review): a saveFlush whose Postgres CAS MISSES bounces a
// saveConflictMsg back to its zone, which re-dumps and enqueues a NEW request — landing after this sentinel
// and therefore outside the barrier. The only contender at drain time is this shard's own save cadence,
// dumping the same session at the same moment, so whichever CAS wins persists equivalent state before the
// barrier and the loser's re-dump is redundant. Looping the barrier until the queue is quiescent would
// LIVELOCK against that still-ticking cadence, so it stays single-shot.
func (sv *saver) flush(ctx context.Context, dead <-chan struct{}) error {
	if !sv.enabled() {
		return nil // no durable tier: nothing was ever queued
	}
	barrier := make(chan struct{})
	select {
	case sv.reqs <- saveRequest{barrier: barrier}:
	case <-ctx.Done():
		return ctx.Err()
	case <-dead:
		return errors.New("saver drainer already stopped")
	}
	select {
	case <-barrier:
		sv.log.Debug("saver queue drained")
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-dead:
		return errors.New("saver drainer stopped before the barrier completed")
	}
}

// saveIOTimeout bounds one save's blocking I/O so a hung Redis/Postgres can't wedge the drainer
// (which would silently stop every subsequent save). On timeout the write is abandoned; the next
// cadence tick re-enqueues a fresh snapshot.
const saveIOTimeout = 5 * time.Second

// finalFlushRetries bounds how many times a final (logout) flush re-reads + rebases + retries its
// CAS before giving up. A handful is plenty: the only contender is this shard's own cadence flush,
// so each retry strictly advances the row's version and the loop converges in one or two passes.
// The bound stops a pathological churn (e.g. a tight cadence) from spinning the drainer.
const finalFlushRetries = 8

// finalFlushBudget caps the TOTAL wall-clock one logout flush (its first CAS plus every reconcile
// retry + zone presence probe) may consume on the saver drainer goroutine. The drainer is shared by
// every hosted zone, so an unbounded final flush would head-of-line-block all other zones' saves
// under a logout storm. It is deliberately TIGHTER than saveIOTimeout: a single store round-trip
// still gets the full per-call timeout below, but the reconcile loop as a whole yields the drainer
// well before a wedged store could stall the shard. On budget exhaustion the flush logs at Warn
// (the observable durability gap) and the next login's load/checkpoint freshness check recovers.
const finalFlushBudget = 2 * time.Second

// finalFlushIOTimeout bounds ONE store call (a load or a save) inside the reconcile loop, so a
// single hung call can't consume the whole finalFlushBudget in one shot and starve the retries.
const finalFlushIOTimeout = 750 * time.Millisecond

// handle performs one save request's I/O off the zone goroutine: always refresh the Redis
// checkpoint (the cheap tier), and for a flushing reason also run the Postgres state_version CAS.
//
// CAS-miss handling depends on WHO will reconcile:
//   - saveFlush (a still-live player's cadence/drain flush): post a saveConflictMsg back so the
//     ZONE re-dumps the player's current state at the fresh version and re-enqueues. The zone
//     goroutine is the single writer and owns the live entity, so it is the authority.
//   - saveFinal (the logout/leave flush): the session is already being removed, so there is no
//     one to bounce a conflict back to. This snapshot IS the authoritative final state, so the
//     saver re-reads the current version, rebases the snapshot onto it, and retries the CAS in
//     place (bounded). This is what makes a logout-after-move durable even when a cadence flush
//     wins the CAS first (docs/PERSISTENCE.md §6 — logout is a flush point, never silently lost).
//
// The saver NEVER mutates entity state here; on success it posts the bumped state_version back via
// saveOkMsg so a still-present session (the non-final case) stays monotonic for its next save.
func (sv *saver) handle(ctx context.Context, req saveRequest) {
	if req.barrier != nil {
		// Unreachable: run intercepts the sentinel. Defensive, because a barrier that reached here would
		// checkpoint a zero-valued CharSnapshot under an empty name.
		sv.log.Error("saver barrier sentinel reached handle; dropping")
		close(req.barrier)
		return
	}
	ioCtx, cancel := context.WithTimeout(ctx, saveIOTimeout)
	defer cancel()

	// Redis tier (always): a cheap mirror keyed by name so any shard can rehydrate on login.
	if sv.ckpt != nil {
		if err := sv.ckpt.Checkpoint(ioCtx, req.snap); errors.Is(err, ErrCheckpointNotOwner) {
			// The EARLIEST double-own detector (#432). This tier pulses every ~10s while the Postgres
			// fence only fires on the ~60s flush, so routing the refusal to the same eviction shrinks a
			// zombie's window — the span in which it generates unsaveable play and can externalize wealth
			// into another character's row — by roughly a factor of six. It is not a write failure and
			// must not be logged as one.
			//
			// The checkpoint guard reports only THAT it refused, not which epoch beat us, so ownerEpoch
			// is synthesized as "strictly above ours". That is exactly the predicate Zone.ownershipLost
			// needs — it asks whether the session has since caught up to the winner — and it stays correct
			// however far above us the real winner is: a session that has legitimately advanced past our
			// epoch satisfies the guard and is spared, one that has not is the zombie.
			sv.log.Warn("checkpoint refused: another session owns this character; this copy is a zombie",
				"event", "checkpoint_not_owner", "player", req.id, "our_epoch", req.snap.OwnerEpoch)
			req.zone.post(saveNotOwnerMsg{id: req.id, ourEpoch: req.snap.OwnerEpoch, ownerEpoch: req.snap.OwnerEpoch + 1})
			return
		} else if err != nil {
			sv.log.Debug("checkpoint write failed (non-fatal)", "player", req.id, "err", err)
		} else {
			sv.log.Debug("checkpoint written", "player", req.id, "state_version", req.snap.StateVersion)
		}
	}

	if !req.reason.flush() || sv.store == nil {
		return // checkpoint-only tier, or no durable store configured
	}

	// Postgres tier: optimistic CAS on state_version.
	snap := req.snap
	// Durable state-size guard (docs/REMAINING.md §1, symmetric with the handoff carry cap). Off the zone
	// goroutine, so measuring the marshalled state here never slows a tick. Log-only: the state is the
	// player's own, so we still persist it — a loud WARN surfaces unbounded growth without losing the save.
	if b, err := json.Marshal(snap.State); err == nil && len(b) > maxDurableStateBytes {
		sv.log.Warn("durable character state exceeds soft cap; persisting anyway (investigate unbounded growth)",
			"player", req.id, "bytes", len(b), "cap", maxDurableStateBytes)
	}
	res, err := sv.store.SaveCharacter(ioCtx, snap)
	if err != nil {
		sv.log.Debug("postgres flush failed (non-fatal; next cadence retries)", "player", req.id, "err", err)
		return
	}
	switch res.Outcome {
	case SaveApplied:
		sv.log.Debug("postgres flush ok", "player", req.id, "new_state_version", res.NewVersion)
		req.zone.post(saveOkMsg{id: req.id, newVersion: res.NewVersion})
		return

	case SaveNotOwner:
		// OWNERSHIP loss (#432) — definitive, and terminal on BOTH paths. Another session has claimed
		// this character; this writer's state belongs to a zombie.
		//
		// The instinct to reconcile is what has to be resisted here, and on the live path it is not
		// merely wrong but actively harmful: saveConflict re-reads, re-dumps and re-enqueues
		// IMMEDIATELY (not on the cadence), so answering an epoch loss that way is an unbounded
		// read-write loop on the drainer goroutine that every zone on this shard shares — one per
		// double-owned character, and it can never succeed, because a rebase moves state_version and
		// the predicate it is losing on is owner_epoch.
		//
		// So: log the alertable signal and hand the ZONE a verdict it can act on. Only the zone
		// goroutine may touch the session, and only it knows whether this loss is stale (see
		// Zone.ownershipLost).
		sv.log.Warn("save refused: another session owns this character; this copy is a zombie",
			"event", "save_not_owner", "player", req.id, "reason", req.reason,
			"our_epoch", snap.OwnerEpoch, "owner_epoch", res.CurOwnerEpoch)
		req.zone.post(saveNotOwnerMsg{id: req.id, ourEpoch: snap.OwnerEpoch, ownerEpoch: res.CurOwnerEpoch})
		return

	case SaveNoRow:
		// No live row: soft-deleted, or a persist id that never existed. Retrying cannot create one.
		sv.log.Warn("save refused: no live character row", "event", "save_no_row", "player", req.id)
		return

	case SaveStaleVersion:
		if req.reason == saveFinal {
			// Logout flush lost the CAS to a concurrent write. Reconcile under a tight wall-clock
			// budget (off ioCtx so a wedged store can't head-of-line-block the drainer): rebase +
			// retry while the character is gone, or hand off to the live reconcile path if a session
			// re-appeared. ctx (the parent, drainer-lifetime) bounds the budget, not the already-spent
			// ioCtx, so the reconcile gets its full budget even if the first CAS was slow.
			finCtx, finCancel := context.WithTimeout(ctx, finalFlushBudget)
			sv.finalizeFlush(finCtx, req, snap, res.CurVersion)
			finCancel()
			return
		}
		// A live player's flush lost the CAS *at an epoch it still owns* — its own cadence racing its
		// own drain/logout flush. Bounce a conflict back so the zone re-dumps current state at the
		// fresh version (Zone.saveConflict). It never forces the write off-goroutine.
		sv.log.Debug("save conflict: stale state_version, requesting reconcile",
			"player", req.id, "tried_version", snap.StateVersion)
		req.zone.post(saveConflictMsg{id: req.id})
		return

	default:
		// SaveOutcomeUnset. The zero value is not a legitimate verdict — it means a CharacterStore
		// implementation (most likely a test double) returned a bare SaveResult{}. Treating it as
		// success is how a silently-passing test ships a hole in the fence, so it is loud and terminal.
		sv.log.Error("save returned no outcome; treating as failed (a CharacterStore is not setting SaveResult.Outcome)",
			"player", req.id)
		return
	}
}

// finalizeFlush drives a logout flush to durability after its first CAS lost to a concurrent write.
// It runs on the saver drainer goroutine (off every zone goroutine) and never touches entity/session
// state directly — the data was produced on the zone goroutine (dumpCharacter); only persistence I/O
// and a single-writer zone PROBE happen here.
//
// Each pass: PROBE the zone for a live session (z.players, race-free via presenceMsg), then decide.
//   - The character is GONE (no session): the logout snapshot is authoritative for the clean-quit
//     path (its only concurrent writer is this shard's strictly-older cadence), so re-read, rebase
//     StateVersion onto the current row, and retry the CAS. Each retry strictly advances the stored
//     version, so it converges in one or two passes.
//   - A live session RE-APPEARED (a re-attach within the link-death grace): its fresh state is newer
//     than this logout snapshot, so a force-write would REVERT it. Defer to the live reconcile path
//     — post saveConflictMsg so Zone.saveConflict re-dumps the session's CURRENT state — and stop.
//     This is the architect's "z.players is the authority" rule: never clobber a newer legit write.
//
// Bounded by ctx (finalFlushBudget total) so it yields the shared drainer promptly; each store call
// gets its own finalFlushIOTimeout so one hung call can't eat the whole budget. On budget/retry
// exhaustion it logs at Warn (the observable durability gap; the next login's freshness check
// recovers). On success it posts saveOkMsg for symmetry — a guarded no-op if the session is gone.
func (sv *saver) finalizeFlush(ctx context.Context, req saveRequest, snap CharSnapshot, curVersion uint64) {
	for attempt := 0; attempt < finalFlushRetries; attempt++ {
		if ctx.Err() != nil {
			sv.log.Warn("final flush abandoned: budget exhausted", "player", req.id, "attempt", attempt)
			return
		}
		// A live session re-appeared (re-attach within the link-death grace): its state is newer than
		// this logout snapshot. Hand off to the live reconcile path rather than reverting it.
		//
		// The probe reads this process's z.players, so it can only ever answer for THIS process. Treating
		// its "absent" as "safe to force-write" globally is precisely how a stale shard rolled a live
		// owner back (#432) — a live session on another shard is structurally invisible to it. The
		// owner_epoch conjunct at the sink is what holds the line across processes.
		//
		// It is still load-bearing for what it can actually see: the SAME-shard re-attach. A reconnect
		// that lands after this session was reaped seeds its epoch from the directory rather than a
		// fresh mint (server.go skips the claim for a still-resident session, and the reap can race
		// that check), so the two can hold the same epoch and the fence alone would not separate them.
		// The probe does. Same-shard guard, not the cross-shard fence — that distinction is the fix.
		if sv.zonePresent(ctx, req) {
			sv.log.Debug("final flush yielding: live session re-appeared; routing to live reconcile",
				"player", req.id)
			req.zone.post(saveConflictMsg{id: req.id})
			return
		}
		// Rebase onto the version the concurrent write advanced to. The FIRST pass uses the version the
		// refused CAS already observed under its row lock (no extra round-trip inside the flush budget);
		// later passes re-read.
		if attempt > 0 {
			cur, found, err := sv.loadOnce(ctx, snap.Name)
			if err != nil || !found {
				sv.log.Warn("final flush reconcile read failed; logout state may be lost",
					"player", req.id, "found", found, "err", err)
				return
			}
			curVersion = cur.StateVersion
		}
		snap.StateVersion = curVersion
		// The rebase moves state_version and NOTHING ELSE. snap.OwnerEpoch is deliberately never
		// touched here: it is the claim this session was minted at, and a writer that could raise its
		// own epoch on retry would be able to rebase straight through the ownership fence — which is
		// the whole defect (#432). The retry loop is for contention among writers at one epoch.
		res, err := sv.saveOnce(ctx, snap)
		if err != nil {
			sv.log.Warn("final flush retry write failed; logout state may be lost", "player", req.id, "err", err)
			return
		}
		switch res.Outcome {
		case SaveApplied:
			sv.log.Debug("final flush landed after reconcile", "player", req.id,
				"new_state_version", res.NewVersion, "attempts", attempt+1)
			req.zone.post(saveOkMsg{id: req.id, newVersion: res.NewVersion})
			return
		case SaveNotOwner:
			// DEFINITIVE. Another session owns this character, so this logout snapshot is a zombie's and
			// must be discarded — this is the exact write that used to roll a live owner back. Retrying
			// is not merely futile but unsafe-looking: every pass would re-fail on the same conjunct.
			//
			// Warn, because "a double-own happened" is the alertable event this fence exists to surface.
			// The lost data is the zombie's, and the legitimate owner's state is untouched.
			sv.log.Warn("final flush refused: another session owns this character; discarding the zombie's logout state",
				"event", "final_flush_not_owner", "player", req.id,
				"our_epoch", snap.OwnerEpoch, "owner_epoch", res.CurOwnerEpoch)
			req.zone.post(saveNotOwnerMsg{id: req.id, ourEpoch: snap.OwnerEpoch, ownerEpoch: res.CurOwnerEpoch})
			return
		case SaveNoRow:
			sv.log.Warn("final flush refused: no live character row", "event", "final_flush_no_row", "player", req.id)
			return
		case SaveStaleVersion:
			// Lost again to another concurrent write at our own epoch: re-probe + re-read + retry.
		default:
			sv.log.Error("final flush returned no outcome; abandoning", "player", req.id)
			return
		}
	}
	// The one durability gap worth alerting on. A structured "event" key makes it greppable/
	// alertable until a real metrics tier lands (no counter framework exists yet — slog is the
	// observability primitive here). The next login's load/checkpoint freshness check recovers the
	// last DURABLY-flushed state, so this is "logout delta lost," not "character lost."
	sv.log.Warn("final flush exhausted retries; logout state not persisted",
		"event", "final_flush_dropped", "player", req.id)
}

// loadOnce / saveOnce run one store call under a per-call timeout (finalFlushIOTimeout) derived from
// the reconcile budget, so one hung load/save can't consume the whole finalFlushBudget. Each cancels
// its child context promptly (no leak) rather than relying on the parent budget to reclaim it.
func (sv *saver) loadOnce(ctx context.Context, name string) (CharSnapshot, bool, error) {
	c, cancel := context.WithTimeout(ctx, finalFlushIOTimeout)
	defer cancel()
	return sv.store.LoadCharacter(c, name)
}

func (sv *saver) saveOnce(ctx context.Context, snap CharSnapshot) (SaveResult, error) {
	c, cancel := context.WithTimeout(ctx, finalFlushIOTimeout)
	defer cancel()
	return sv.store.SaveCharacter(c, snap)
}

// zonePresent asks the owning zone whether a live session exists for this character, reading
// z.players on the zone goroutine (single-writer) via the same presence probe the persistence tests
// use. It is a blocking round-trip on the SAVER goroutine (never the zone's), bounded by ctx so a
// slow/stopped zone can't wedge the reconcile. On timeout it returns false (treat as gone): the
// reconcile then force-writes the logout state, which is the safe default — a logout flush that
// can't confirm a re-attach should still persist the player's last known state rather than drop it.
func (sv *saver) zonePresent(ctx context.Context, req saveRequest) bool {
	reply := make(chan presence, 1)
	select {
	case req.zone.inbox <- presenceMsg{id: req.id, reply: reply}:
	case <-ctx.Done():
		return false
	}
	select {
	case p := <-reply:
		return p.present
	case <-ctx.Done():
		return false
	}
}
