package world

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	roster "github.com/double-nibble/telosmud/internal/presence"
)

// presence.go is the shard-level cross-shard `who` plumbing (docs/PHASE8-PLAN.md slice 8.4, P8-D4). It
// owns the set of players THIS shard currently hosts and publishes them to the shared presence roster
// (Redis in prod, an in-process Mem roster in the cross-shard tests) so `who` can read every shard's
// players. It mirrors the saver's discipline: zone goroutines never touch the roster I/O directly — they
// hand a small, concurrency-safe in-memory update to this tracker, and a single background loop does the
// blocking Redis writes (a batched heartbeat + eager add/remove) OFF every zone goroutine.
//
// # Why a shard-level tracker (like commSource / saver), not per-zone
//
// Presence is player-scoped and shard-scoped: `who` lists "players on this shard", regardless of which
// hosted zone holds each one, and a player walking zone->zone WITHIN this shard must not flicker out of
// who. So the resident set lives once on the Shard, keyed by player id, and every hosted zone updates the
// SAME set. It holds no zone state (only {name, afk} per player id), so guarding it with a mutex does not
// weaken single-writer — a zone goroutine recording "I now host Alice" is not a read/write of another
// zone's world data.
//
// # Write authority (P8-A4)
//
// Every write names this shard's id; the roster store refuses a write to a key a DIFFERENT live shard
// owns. So this tracker can only ever assert/refresh/remove the players it actually hosts — it cannot mark
// an arbitrary player online or evict a real one elsewhere. Presence is NEVER a routing source (tells read
// the epoch-authoritative directory); it is the best-effort who roster only.

// presenceTracker is the shard's presence SOURCE state. Always non-nil on a constructed shard; its roster
// is nil until WithPresence wires one — a nil roster makes every method a no-op, so a no-Redis / single-
// shard run keeps the pre-8.4 behavior (cmdWho falls back to the zone-local list) and is byte-identical.
type presenceTracker struct {
	roster    roster.Roster
	shardID   string
	ttl       time.Duration
	heartbeat time.Duration

	mu        sync.Mutex
	residents map[string]roster.Entry // player id -> {name, afk}; the set THIS shard hosts
	eager     chan eagerOp            // immediate add/remove I/O, drained off the zone goroutine
}

// eagerOp is a single immediate presence write enqueued by a zone goroutine: a join SET (one key) or a
// clean-quit REMOVE. It is drained by the background loop so the blocking Redis call never runs on a zone
// goroutine. A full queue drops the eager write (the next heartbeat reconciles it) rather than stalling
// the actor loop — the saver's exact backpressure discipline.
type eagerOp struct {
	id     string
	entry  roster.Entry // for an add
	remove bool
}

// presenceEagerQueue bounds the eager-op channel. A drop is self-healing: a missed join SET is picked up
// by the next heartbeat (the player is in `residents`); a missed REMOVE is covered by the TTL age-out.
const presenceEagerQueue = 256

func newPresenceTracker() *presenceTracker {
	return &presenceTracker{
		ttl:       roster.DefaultTTL,
		heartbeat: roster.DefaultHeartbeat,
		residents: map[string]roster.Entry{},
		eager:     make(chan eagerOp, presenceEagerQueue),
	}
}

// enabled reports whether a roster is wired. A disabled tracker short-circuits every method so a no-Redis
// shard does zero presence work and `who` cleanly falls back to the zone-local list.
func (p *presenceTracker) enabled() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.roster != nil
}

// join records that this shard now hosts playerID (a fresh login or a cross-shard handoff arrival) and
// eagerly writes its presence so the player appears in `who` immediately, not only on the next heartbeat.
// Concurrency-safe; called from a zone goroutine. The blocking write happens on the background loop.
func (p *presenceTracker) join(playerID, name string, afk bool) {
	if !p.enabled() {
		return
	}
	e := roster.Entry{PlayerID: playerID, Name: name, ShardID: p.shardID, AFK: afk}
	p.mu.Lock()
	p.residents[playerID] = e
	p.mu.Unlock()
	p.enqueue(eagerOp{id: playerID, entry: e})
}

// leave records that this shard no longer hosts playerID (a clean quit/leave or a handed-off orphan reap)
// and eagerly REMOVEs its presence so the player drops out of `who` at once — before the TTL. A handoff
// AWAY removes the source-side resident here; the destination's join re-asserts it (and the roster's
// owner-guard means our late remove can't evict the destination's fresh entry). Concurrency-safe.
func (p *presenceTracker) leave(playerID string) {
	if !p.enabled() {
		return
	}
	p.mu.Lock()
	_, had := p.residents[playerID]
	delete(p.residents, playerID)
	p.mu.Unlock()
	if !had {
		return // not ours (or already gone): nothing to remove
	}
	p.enqueue(eagerOp{id: playerID, remove: true})
}

// enqueue hands an eager op to the background loop WITHOUT blocking the zone goroutine; a full queue drops
// it (self-healing: the heartbeat reconciles an add, the TTL covers a remove). Mirrors saver.enqueue.
func (p *presenceTracker) enqueue(op eagerOp) {
	select {
	case p.eager <- op:
	default:
		// queue full — drop; the heartbeat / TTL reconciles.
	}
}

// snapshot copies the current resident set for the batched heartbeat write. Concurrency-safe.
func (p *presenceTracker) snapshot() []roster.Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]roster.Entry, 0, len(p.residents))
	for _, e := range p.residents {
		out = append(out, e)
	}
	return out
}

// run is the single background loop (started once by Shard.Run) that does ALL presence I/O off every zone
// goroutine: a periodic BATCHED heartbeat (one pipelined SET for every resident — write rate O(1) per
// shard per beat, never O(players)) and the eager add/remove drain. A nil/disabled tracker returns at once
// (no goroutine cost). On shutdown it does NOT proactively delete this shard's presence — a clean drain is
// the placement controller's job (Phase 10); the TTL ages the entries out, the same self-healing path a
// crash uses.
func (p *presenceTracker) run(ctx context.Context) {
	if !p.enabled() {
		return
	}
	ticker := time.NewTicker(p.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refresh(ctx)
		case op := <-p.eager:
			p.applyEager(ctx, op)
		}
	}
}

// presenceIOTimeout bounds one presence write/read so a hung Redis can't wedge the loop (which would stop
// every subsequent heartbeat, silently freezing the roster). On timeout the write is abandoned; the next
// beat retries from the in-memory resident set. Mirrors saveIOTimeout.
const presenceIOTimeout = 5 * time.Second

// refresh writes the whole resident set in one batched, owner-guarded SET — the heartbeat that keeps every
// live resident's TTL from lapsing. ErrNotOwner (an entry a different shard took over) is benign and
// ignored: the roster applied the entries we legitimately own.
func (p *presenceTracker) refresh(ctx context.Context) {
	entries := p.snapshot()
	if len(entries) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, presenceIOTimeout)
	defer cancel()
	r := p.currentRoster()
	if r == nil {
		return
	}
	_ = r.Set(cctx, p.shardID, entries, p.ttl)
}

// applyEager performs one immediate add/remove off the zone goroutine.
func (p *presenceTracker) applyEager(ctx context.Context, op eagerOp) {
	cctx, cancel := context.WithTimeout(ctx, presenceIOTimeout)
	defer cancel()
	r := p.currentRoster()
	if r == nil {
		return
	}
	if op.remove {
		_ = r.Remove(cctx, p.shardID, op.id)
		return
	}
	_ = r.Set(cctx, p.shardID, []roster.Entry{op.entry}, p.ttl)
}

// currentRoster reads the roster handle under the lock (a test can swap it to nil to simulate a crash).
func (p *presenceTracker) currentRoster() roster.Roster {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.roster
}

// --- Zone-goroutine accessors into the shard's presence tracker ------------------------------

// presenceJoin records (on the shard's tracker) that this zone now hosts a resident player, so they appear
// in cross-shard `who`. Called from the zone goroutine on a fresh login / handoff arrival. A bare zone (no
// shard) or a disabled tracker is a no-op. Pulls the AFK flag from the player's comms state if present.
func (z *Zone) presenceJoin(s *session) {
	if z.shard == nil || z.shard.presence == nil || s == nil || s.entity == nil {
		return
	}
	z.shard.presence.join(s.character, s.entity.Name(), playerAFK(s))
}

// presenceLeave records that this zone no longer hosts the player (clean quit/leave or handed-off orphan
// reap), eagerly removing them from `who`. No-op on a bare zone / disabled tracker.
func (z *Zone) presenceLeave(id string) {
	if z.shard == nil || z.shard.presence == nil {
		return
	}
	z.shard.presence.leave(id)
}

// playerAFK reports the session's AFK flag for presence. 8.4 carries the field; the `afk` command that
// sets it lands in 8.6, so this is false today (a hook point — no AFK state subtree exists yet).
func playerAFK(_ *session) bool { return false }

// rosterList reads the whole cross-shard presence roster for `who` (off the zone goroutine — see cmdWho).
// Returns nil + false when presence is disabled (no roster) or the read errors, so cmdWho falls back to
// the zone-local list.
func (z *Zone) rosterList(ctx context.Context) ([]roster.Entry, bool) {
	if z.shard == nil || z.shard.presence == nil || !z.shard.presence.enabled() {
		return nil, false
	}
	r := z.shard.presence.currentRoster()
	if r == nil {
		return nil, false
	}
	entries, err := r.List(ctx)
	if err != nil {
		return nil, false // a roster read error degrades to the zone-local fallback, never an error to the player
	}
	return entries, true
}

// renderWho formats the cross-shard presence roster as the player-visible `who` list. Same shape as the
// zone-local who (a "Players online:" header + one indented name per player), extended for 8.4: an AFK
// player is marked. Sorted by name for a stable, readable list across shards.
func renderWho(entries []roster.Entry) string {
	names := make([]string, 0, len(entries))
	afk := map[string]bool{}
	for _, e := range entries {
		display := e.Name
		if display == "" {
			display = e.PlayerID
		}
		names = append(names, display)
		afk[display] = e.AFK
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("Players online:")
	for _, n := range names {
		b.WriteByte('\n')
		b.WriteByte(' ')
		b.WriteString(n)
		if afk[n] {
			b.WriteString(" (AFK)")
		}
	}
	return b.String()
}

// writeFrameTo writes a frame to a session out channel from a NON-zone goroutine (the async `who` read).
// It uses the same non-blocking select as session.send but does NOT touch the zone-owned appliedSeq — the
// frame carries ack 0 (like a comms frame), so this is race-free off the zone goroutine.
func writeFrameTo(out chan *playv1.ServerFrame, f *playv1.ServerFrame) {
	if out == nil {
		return
	}
	select {
	case out <- f:
	default:
	}
}
