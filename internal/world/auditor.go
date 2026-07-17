package world

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// auditor.go is the async write path for the #350 audit trail — the world-side twin of saver.go. An
// emit site on the zone goroutine PRODUCES an AuditEvent (race-free, reading the live entity) and hands
// it to this auditor over a buffered channel; a single background goroutine drains the channel and calls
// sink.AppendAudit OFF every zone goroutine, so an audit write NEVER blocks a tick. It is created once
// per shard and shared by every hosted zone (like the saver).
//
// A nil sink makes every enqueue a no-op — the bare-engine invariant: a storeless shard audits nothing.
// A full queue DROPS the event with a Warn (the saver's posture): the queue only fills under a wedged
// DB, and the durable unique index still guarantees no double-record, so a dropped audit is a lost
// observability row, never a correctness bug. Death/tier/create — the events that most matter — also
// pass through here, but the queue is sized to absorb a burst and the DB is the correctness authority.

// auditQueueDepth bounds the auditor's buffered channel. A full queue means the sink is wedged on slow
// I/O; rather than block the zone goroutine (stalling every player on it) the enqueue drops and logs at
// Warn. Mirrors saveQueueDepth.
const auditQueueDepth = 256

// auditIOTimeout bounds one AppendAudit call so a hung Postgres can't wedge the drainer (which would
// silently stop every subsequent audit write). On timeout the write is abandoned; the event is lost
// (an observability gap, not a correctness one). Mirrors saveIOTimeout.
const auditIOTimeout = 5 * time.Second

// auditDrainDeadline bounds the shutdown flush (drainRemaining): the total time the auditor will spend
// writing the events still buffered when its context is cancelled, before it gives up and exits. Keeps a
// wedged DB from stalling a graceful shard drain indefinitely while still flushing a healthy queue.
const auditDrainDeadline = 5 * time.Second

// auditor owns the sink + a buffered request channel drained by a single background goroutine. Created
// once per shard (newAuditor) and shared by every hosted zone; the buffered channel + single drainer
// keep writes off every zone goroutine. A nil sink makes it a no-op, which is how a storeless boot keeps
// today's behavior.
type auditor struct {
	sink AuditSink
	reqs chan AuditEvent
	log  *slog.Logger
}

// newAuditor builds an auditor over the given sink. A nil sink disables it (every enqueue a no-op). The
// drainer goroutine is started by run (called from Shard.Run) so a bare test that never runs the shard
// incurs no goroutine.
func newAuditor(sink AuditSink) *auditor {
	return &auditor{
		sink: sink,
		reqs: make(chan AuditEvent, auditQueueDepth),
		log:  slog.With("component", "auditor"),
	}
}

// enabled reports whether a sink is configured. A disabled auditor short-circuits every enqueue so a
// storeless shard does zero work and is byte-identical to a pre-#350 shard.
func (a *auditor) enabled() bool { return a != nil && a.sink != nil }

// enqueue hands an event to the drainer WITHOUT blocking the zone goroutine. A full queue drops the
// event (logged at Warn) rather than stalling the actor loop — a single slow sink must never wedge a
// zone. A disabled auditor drops silently. Called only from the zone goroutine, so the event's read of
// the live entity upstream is race-free.
func (a *auditor) enqueue(ev AuditEvent) {
	if !a.enabled() {
		return
	}
	select {
	case a.reqs <- ev:
	default:
		a.log.Warn("audit event dropped: auditor queue full (DB wedged?)",
			"subject", ev.SubjectID, "kind", ev.EventKind)
	}
}

// run drains the queue on a single background goroutine until ctx is cancelled. Each AppendAudit does
// its blocking I/O here, OFF every zone goroutine. Started once by Shard.Run.
//
// On ctx cancel it FLUSHES the events already queued before returning (drainRemaining), symmetric to the
// saver's drain discipline: a dropped audit row has no other durability path (unlike a checkpoint the next
// cadence re-enqueues), so abandoning the buffer on every graceful shard drain would silently lose the
// events queued in that instant. The queue-full drop in enqueue still stands (only reachable under a
// wedged DB), so the async half remains BEST-EFFORT, not a durability guarantee — the DB unique index
// prevents doubles, not drops.
func (a *auditor) run(ctx context.Context) {
	if !a.enabled() {
		return // no sink: nothing to drain
	}
	a.log.Debug("auditor loop start")
	for {
		select {
		case <-ctx.Done():
			a.drainRemaining()
			a.log.Debug("auditor loop stop")
			return
		case ev := <-a.reqs:
			a.handle(ctx, ev)
		}
	}
}

// drainRemaining writes every event still buffered at shutdown, under a FRESH bounded context (the run
// ctx is already cancelled, so reusing it would abort each write immediately). It drains only what is
// already in the channel — a non-blocking receive loop — so it can never wait on a live producer; every
// producer is a zone goroutine that has stopped by the time the shard's context is cancelled.
func (a *auditor) drainRemaining() {
	ctx, cancel := context.WithTimeout(context.Background(), auditDrainDeadline)
	defer cancel()
	for {
		select {
		case ev := <-a.reqs:
			a.handle(ctx, ev)
		default:
			return
		}
	}
}

// handle performs one AppendAudit off the zone goroutine, under a bounded timeout. A failure logs at
// Debug (non-fatal — the trail is observability, not a durability guarantee for gameplay); recorded=false
// (a benign idempotent no-op) is unremarkable and silent.
func (a *auditor) handle(ctx context.Context, ev AuditEvent) {
	ioCtx, cancel := context.WithTimeout(ctx, auditIOTimeout)
	defer cancel()
	if _, err := a.sink.AppendAudit(ioCtx, ev); err != nil {
		a.log.Debug("audit append failed (non-fatal)", "subject", ev.SubjectID, "kind", ev.EventKind, "err", err)
	}
}

// --- zone-goroutine emit helpers ---------------------------------------------------------------
//
// Each helper runs ON the zone goroutine (the single writer), reads the live entity race-free to build
// an AuditEvent, and ENQUEUES it (never blocks). Every one guards the sink being present AND the subject
// being a valid saved player (isPlayer + a non-nil pid), so a storeless shard, a mob, or a player in the
// async-create window (pid still nil) all emit nothing.

// auditSink returns the shard's audit sink, or nil on a bare zone / a storeless shard (auditing
// disabled). Mirrors mailStore's accessor. Read-only; safe from any zone goroutine.
func (z *Zone) auditSink() AuditSink {
	if z.shard == nil || z.shard.auditor == nil {
		return nil
	}
	return z.shard.auditor.sink
}

// auditEnabled reports whether this zone can emit audit events (a non-nil sink behind a live auditor).
// The emit helpers short-circuit on it so a storeless zone does zero work.
func (z *Zone) auditEnabled() bool {
	return z.shard != nil && z.shard.auditor.enabled()
}

// auditPlayerDeath records a player's permanent death. The dedup_key is a FRESH per-death UUID, not the
// deaths counter: die()'s l.dying latch already guarantees this helper is reached at most once per death
// (a re-entrant / same-round DoT die() early-returns before l.deaths++), and the auditor never retries an
// enqueue, so there is no in-process duplicate for the key to defend against. The deaths counter is
// TRANSIENT (Living.deaths resets to 0 in a fresh process, components.go), so keying on it would make
// (pid, "died", "1") collide across relogs/handoffs and silently drop every death after the first per
// generation-number (the durable index spans the character's whole life). The counter is carried in the
// payload for context instead. A mob death (isPlayer false) or a not-yet-saved player (pid nil) emits
// nothing. The killer, if a saved player, is the actor; otherwise the death is attributed to the system.
func (z *Zone) auditPlayerDeath(victim, killer *Entity, deaths uint64) {
	if !z.auditEnabled() || !isPlayer(victim) || victim.pid == nil {
		return
	}
	actorType := AuditActorSystem
	actorID := ""
	killerName := ""
	if killer != nil {
		killerName = killer.Name()
		if isPlayer(killer) && killer.pid != nil {
			actorType = AuditActorCharacter
			actorID = string(*killer.pid)
		}
	}
	roomRef := ""
	if victim.location != nil {
		roomRef = string(victim.location.proto)
	}
	z.shard.auditor.enqueue(AuditEvent{
		SubjectType: AuditSubjectCharacter,
		SubjectID:   string(*victim.pid),
		SubjectName: victim.Name(),
		ActorType:   actorType,
		ActorID:     actorID,
		EventKind:   AuditKindDied,
		DedupKey:    uuid.NewString(),
		Payload:     AuditPayload(map[string]any{"killer_name": killerName, "room_ref": roomRef, "generation": deaths}),
	})
}

// auditAttributeBase records a permanent attribute-base grant on a player. A discrete grant has no
// natural idempotency key (two +1 STR grants are two distinct events), so dedup_key is a fresh UUID —
// each call records its own row, and a rare retry is harmlessly a second row rather than a swallowed one.
// A mob target or a not-yet-saved player emits nothing.
func (z *Zone) auditAttributeBase(target *Entity, attr string, oldV, newV float64) {
	if !z.auditEnabled() || !isPlayer(target) || target.pid == nil {
		return
	}
	z.shard.auditor.enqueue(AuditEvent{
		SubjectType: AuditSubjectCharacter,
		SubjectID:   string(*target.pid),
		SubjectName: target.Name(),
		ActorType:   AuditActorSystem,
		EventKind:   AuditKindAttributeBase,
		DedupKey:    uuid.NewString(),
		Payload: AuditPayload(map[string]any{
			"attr": attr, "old": oldV, "new": newV, "delta": newV - oldV,
		}),
	})
}

// auditTrackStep records one newly-crossed advancement-track step on a player. dedup_key is
// "<track>\x1f<step>" — a unit-separator delimiter (not ":") so a track ref containing a colon can't make
// two distinct (track, step) pairs alias to one key. The stored step is a high-water mark, so a re-advance
// crossing no new step calls this for no step (the caller's loop doesn't run) and a replay of an
// already-crossed step is deduped by the unique index. A mob subject or a not-yet-saved player emits
// nothing. (A track that can be RESET and re-crossed would re-collide on the old key — noted as a
// follow-up; tracks are monotonic today.)
func (z *Zone) auditTrackStep(subject *Entity, trackRef string, step int, threshold any) {
	if !z.auditEnabled() || !isPlayer(subject) || subject.pid == nil {
		return
	}
	z.shard.auditor.enqueue(AuditEvent{
		SubjectType: AuditSubjectCharacter,
		SubjectID:   string(*subject.pid),
		SubjectName: subject.Name(),
		ActorType:   AuditActorSystem,
		EventKind:   AuditKindTrackAdvanced,
		DedupKey:    trackRef + "\x1f" + strconv.Itoa(step),
		Payload:     AuditPayload(map[string]any{"track": trackRef, "step": step, "threshold": threshold}),
	})
}
