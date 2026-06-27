package world

import (
	"context"
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
	// This force-rebase is authoritative ONLY for the clean-quit path, and only because its sole
	// concurrent writer is this shard's STRICTLY-OLDER cadence flush (the logout state dominates a
	// flush taken before it). It is NOT globally newest, and the saver cannot prove that off
	// goroutine — so before each force-write finalizeFlush PROBES the zone (z.players, single-
	// writer): if the character is truly gone the rebase is safe; if a live session re-appeared
	// (a re-attach within the link-death grace, whose fresh state would otherwise be reverted) it
	// hands off to the LIVE reconcile path (saveConflictMsg -> Zone.saveConflict re-dumps current)
	// instead of clobbering. Cross-shard zombie fence (saver.handle) is intact regardless: a final
	// flush only ever runs for a player leaving THIS shard via leave(); a handed-off character is
	// removed by freezeExpire WITHOUT a save, never reaching this path.
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
			sv.handle(ctx, req)
		}
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
	ioCtx, cancel := context.WithTimeout(ctx, saveIOTimeout)
	defer cancel()

	// Redis tier (always): a cheap mirror keyed by name so any shard can rehydrate on login.
	if sv.ckpt != nil {
		if err := sv.ckpt.Checkpoint(ioCtx, req.snap); err != nil {
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
	newVersion, ok, err := sv.store.SaveCharacter(ioCtx, snap)
	if err != nil {
		sv.log.Debug("postgres flush failed (non-fatal; next cadence retries)", "player", req.id, "err", err)
		return
	}
	if !ok {
		if req.reason == saveFinal {
			// Logout flush lost the CAS to a concurrent write. Reconcile under a tight wall-clock
			// budget (off ioCtx so a wedged store can't head-of-line-block the drainer): rebase +
			// retry while the character is gone, or hand off to the live reconcile path if a session
			// re-appeared. ctx (the parent, drainer-lifetime) bounds the budget, not the already-spent
			// ioCtx, so the reconcile gets its full budget even if the first CAS was slow.
			finCtx, finCancel := context.WithTimeout(ctx, finalFlushBudget)
			sv.finalizeFlush(finCtx, req, snap)
			finCancel()
			return
		}
		// A live player's flush lost the CAS: bounce a conflict back so the zone re-dumps current
		// state at the fresh version (Zone.saveConflict). It never forces the write off-goroutine.
		sv.log.Debug("save conflict: stale state_version, requesting reconcile",
			"player", req.id, "tried_version", snap.StateVersion)
		req.zone.post(saveConflictMsg{id: req.id})
		return
	}
	sv.log.Debug("postgres flush ok", "player", req.id, "new_state_version", newVersion)
	req.zone.post(saveOkMsg{id: req.id, newVersion: newVersion})
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
func (sv *saver) finalizeFlush(ctx context.Context, req saveRequest, snap CharSnapshot) {
	for attempt := 0; attempt < finalFlushRetries; attempt++ {
		if ctx.Err() != nil {
			sv.log.Warn("final flush abandoned: budget exhausted", "player", req.id, "attempt", attempt)
			return
		}
		// A live session re-appeared (re-attach within the link-death grace): its state is newer than
		// this logout snapshot. Hand off to the live reconcile path rather than reverting it.
		if sv.zonePresent(ctx, req) {
			sv.log.Debug("final flush yielding: live session re-appeared; routing to live reconcile",
				"player", req.id)
			req.zone.post(saveConflictMsg{id: req.id})
			return
		}
		cur, found, err := sv.loadOnce(ctx, snap.Name)
		if err != nil || !found {
			sv.log.Warn("final flush reconcile read failed; logout state may be lost",
				"player", req.id, "found", found, "err", err)
			return
		}
		snap.StateVersion = cur.StateVersion // rebase onto the version the cadence advanced to
		newVersion, ok, err := sv.saveOnce(ctx, snap)
		if err != nil {
			sv.log.Warn("final flush retry write failed; logout state may be lost", "player", req.id, "err", err)
			return
		}
		if ok {
			sv.log.Debug("final flush landed after reconcile", "player", req.id,
				"new_state_version", newVersion, "attempts", attempt+1)
			req.zone.post(saveOkMsg{id: req.id, newVersion: newVersion})
			return
		}
		// Lost again to another concurrent write: re-probe + re-read + retry until the bound.
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

func (sv *saver) saveOnce(ctx context.Context, snap CharSnapshot) (uint64, bool, error) {
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
