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

// auditBatchMax caps how many events one drain flush coalesces into a single AppendAuditBatch round-trip
// (#399). The drainer takes one event, then greedily pulls up to auditBatchMax-1 more already-queued
// events and writes them in ONE pipelined transaction — so a death-storm (or any burst) costs one
// round-trip per batch instead of one per event, raising the throughput ceiling under a momentarily-slow
// Postgres. Sized to the queue depth: a full queue drains in a couple of batches, and a single steady-state
// event still flushes immediately (the coalesce loop is non-blocking, so it never waits to fill a batch).
const auditBatchMax = 64

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

// auditLowPriorityWatermark reserves queue headroom for the rare, security-critical kinds (died / tier_changed
// / character_created / attribute_base_changed / track_advanced). A HIGH-FREQUENCY, LOW-VALUE kind
// (item_transferred, #443) must not be able to fill the shared queue and raise THEIR drop probability — an
// attacker-influenceable degradation (two confederates looping drop/get), and a way to mask another dupe row
// (security review, Finding B). Sized to HALF the depth: transfers may use up to the first half, but the
// second half is always available to the critical kinds regardless of transfer volume.
const auditLowPriorityWatermark = auditQueueDepth / 2

// enqueueLowPriority enqueues a high-frequency, low-value event but SHEDS it once the queue is already past
// auditLowPriorityWatermark, reserving the rest of the depth for the security-critical kinds (see the
// watermark doc). Below the watermark it behaves exactly like enqueue. The len() read is a cheap heuristic —
// a momentary race just means one extra low-priority event slips in or is shed, never a critical-kind drop.
func (a *auditor) enqueueLowPriority(ev AuditEvent) {
	if !a.enabled() {
		return
	}
	if len(a.reqs) >= auditLowPriorityWatermark {
		a.log.Debug("low-priority audit event shed to reserve queue headroom for critical kinds",
			"subject", ev.SubjectID, "kind", ev.EventKind)
		return
	}
	a.enqueue(ev)
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
			// Coalesce this event with any others already queued into ONE batch write (#399), so a burst
			// (a death-storm) drains in a couple of round-trips instead of one per event.
			a.handleBatch(ctx, a.coalesce(ev))
		}
	}
}

// coalesce greedily pulls up to auditBatchMax-1 more already-queued events onto `first`, WITHOUT blocking
// (the default case stops the moment the queue is momentarily empty). So a single steady-state event
// flushes immediately as a batch of one, while a burst packs a full batch — the coalesce never waits to
// fill a batch, which would add latency to a lone event. Called only from the drainer goroutine.
func (a *auditor) coalesce(first AuditEvent) []AuditEvent {
	batch := make([]AuditEvent, 1, auditBatchMax)
	batch[0] = first
	for len(batch) < auditBatchMax {
		select {
		case ev := <-a.reqs:
			batch = append(batch, ev)
		default:
			return batch
		}
	}
	return batch
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
			a.handleBatch(ctx, a.coalesce(ev)) // batch the shutdown flush too
		default:
			return
		}
	}
}

// handleBatch writes one coalesced batch off the zone goroutine, under a bounded timeout. One
// AppendAuditBatch is one round-trip regardless of batch size, so the timeout bounds the whole flush.
//
// On a batch error it FALLS BACK to per-row appends under the SAME bounded ctx. A failed batch is one
// implicit transaction that committed NOTHING (all-or-nothing), so re-inserting per row is safe and, being
// ON CONFLICT idempotent, harmless — this restores the blast radius of a single POISON event (a row the DB
// rejects) from the whole batch (up to auditBatchMax) back to just that one row, so one bad event no longer
// takes 63 good ones down with it. It does NOT reintroduce slow-drain under a wedged DB: the fallback
// shares the one already-bounded ioCtx, so once that is spent (the DB-down case, where the batch and every
// row fail alike) the remaining per-row calls fail instantly rather than each waiting a fresh timeout.
// Everything stays best-effort and non-fatal (Debug logs only) — the trail is observability, not a
// gameplay-durability guarantee.
func (a *auditor) handleBatch(ctx context.Context, evs []AuditEvent) {
	if len(evs) == 0 {
		return
	}
	ioCtx, cancel := context.WithTimeout(ctx, auditIOTimeout)
	defer cancel()
	if _, err := a.sink.AppendAuditBatch(ioCtx, evs); err != nil {
		a.log.Debug("audit batch append failed; falling back to per-row", "count", len(evs), "err", err)
		for _, ev := range evs {
			if _, err := a.sink.AppendAudit(ioCtx, ev); err != nil {
				a.log.Debug("audit append failed (non-fatal)", "subject", ev.SubjectID, "kind", ev.EventKind, "err", err)
			}
		}
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

// auditItemTransfer records an unbound item crossing a CHARACTER boundary — an item one player DROPPED or PUT
// that a DIFFERENT player picked up (#443). SUBJECT is the ACQUIRER (the item's new durable home — the
// surviving externalized copy the #432 residual is about), ACTOR is the RELEASER (the source, keyed so a
// reconciler can spot an externalization during a double-own window). dedup_key is a FRESH UUID: each
// transfer is a distinct event with no natural per-lifetime key (two legitimate trades of the same ref are
// two rows). A storeless shard, an unsaved acquirer, or a mob acquirer emits nothing. The releaser fields
// come from the Released marker (recordCrossCharTransfer already gated releaser != acquirer). This is
// DETECTION only — it neither prevents the transfer nor asserts a conservation invariant.
func (z *Zone) auditItemTransfer(acquirer *Entity, rel *Released, item *Entity) {
	if !z.auditEnabled() || !isPlayer(acquirer) || acquirer.pid == nil {
		return
	}
	roomRef := ""
	if acquirer.location != nil {
		roomRef = string(acquirer.location.proto)
	}
	stack := 1 // a non-material item is a single instance; a material carries its stack count
	if s, ok := Get[*Stack](item); ok {
		stack = s.count
	}
	z.shard.auditor.enqueueLowPriority(AuditEvent{ // #443: high-frequency kind — must not starve died/tier
		SubjectType: AuditSubjectCharacter,
		SubjectID:   string(*acquirer.pid),
		SubjectName: acquirer.Name(),
		ActorType:   AuditActorCharacter,
		ActorID:     rel.pid,
		EventKind:   AuditKindItemTransferred,
		DedupKey:    uuid.NewString(),
		Payload: AuditPayload(map[string]any{
			"item_ref": string(item.proto), "stack": stack,
			"from_name": rel.name, "to_name": acquirer.Name(), "room_ref": roomRef,
		}),
	})
}
