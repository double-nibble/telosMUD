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
	// the ~60s cadence, on clean leave/quit, and on shard drain.
	saveFlush
)

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

// handle performs one save request's I/O off the zone goroutine: always refresh the Redis
// checkpoint (the cheap tier), and for a flush also run the Postgres state_version CAS. On a CAS
// CONFLICT (the stored version moved — a zombie/duplicated owner saved first) it posts a
// saveConflictMsg back to the zone so the zone can reconcile by re-dumping at the current version;
// it NEVER mutates entity state here. On success it posts the bumped state_version back via
// saveOkMsg so the session stays monotonic for the next save.
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

	if req.reason != saveFlush || sv.store == nil {
		return // checkpoint-only tier, or no durable store configured
	}

	// Postgres tier: optimistic CAS on state_version.
	newVersion, ok, err := sv.store.SaveCharacter(ioCtx, req.snap)
	if err != nil {
		sv.log.Debug("postgres flush failed (non-fatal; next cadence retries)", "player", req.id, "err", err)
		return
	}
	if !ok {
		// Stale writer lost the CAS: the stored row moved past req.snap.StateVersion. Post the
		// conflict back so the zone re-dumps at the current version (it never forces the write).
		sv.log.Debug("save conflict: stale state_version, requesting reconcile",
			"player", req.id, "tried_version", req.snap.StateVersion)
		req.zone.post(saveConflictMsg{id: req.id})
		return
	}
	sv.log.Debug("postgres flush ok", "player", req.id, "new_state_version", newVersion)
	req.zone.post(saveOkMsg{id: req.id, newVersion: newVersion})
}
