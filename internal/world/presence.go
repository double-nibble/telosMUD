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

	// who-read cache (scale hardening): a `who` reads the WHOLE cross-shard roster (a Redis SCAN + an HMGET
	// per online player), which a `who` flood or a large roster makes the first scale pressure point. This
	// caches the last List result for whoCacheTTL so N concurrent `who` collapse to ONE SCAN per window;
	// whoMu (separate from mu) serializes the refresh so only one goroutine hits Redis at the window edge.
	whoMu    sync.Mutex
	whoCache []roster.Entry
	whoAt    time.Time
	whoOK    bool
}

// whoCacheTTL bounds how stale a `who` list may be — small enough that the roster looks live, large enough
// to collapse a spam of `who` into one SCAN per window.
const whoCacheTTL = time.Second

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
func (p *presenceTracker) join(playerID, name string, afk, concealed bool) {
	if !p.enabled() {
		return
	}
	e := roster.Entry{PlayerID: playerID, Name: name, ShardID: p.shardID, AFK: afk, Concealed: concealed}
	p.mu.Lock()
	// join is also the re-write path for an AFK/conceal change (commscmds.go / republishPresenceOnConcealChange),
	// which do NOT recompute the hear-set — so CARRY FORWARD any Channels setResidentChannels already stamped
	// (#90), keeping setResidentChannels the sole writer of Channels and join the sole writer of name/afk/
	// concealed. Without this, an `afk` toggle (or any command while AFK) would silently wipe the player from
	// every channel roster until the next publishCommsConfig, and it would NOT self-heal (the heartbeat reads
	// this wiped map). On a fresh login prev is absent → nil → the following publishCommsConfig stamps it.
	if prev, ok := p.residents[playerID]; ok {
		e.Channels = prev.Channels
	}
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

// setResidentChannels updates a resident's effective hear-set on their roster entry (#90), so the cross-
// shard presence roster ALSO carries the per-channel membership ("who hears X" = List() inverted). Called
// from publishCommsConfig — the SINGLE funnel every hear-set change routes through (login, handoff arrival,
// `channel on|off`, an access change, a channel-def hot reload) — so this one wiring point covers them all.
// A no-op for a non-resident (not ours yet) or a disabled tracker. It eagerly re-writes so a toggle reflects
// in the roster before the next heartbeat; a dropped eager op self-heals on the next heartbeat. Zone-safe.
func (p *presenceTracker) setResidentChannels(playerID string, refs []string) {
	if !p.enabled() {
		return
	}
	p.mu.Lock()
	e, ok := p.residents[playerID]
	if !ok {
		p.mu.Unlock()
		return // not our resident (yet); join seeds the entry and the following publishCommsConfig re-stamps
	}
	e.Channels = append([]string(nil), refs...)
	p.residents[playerID] = e
	p.mu.Unlock()
	p.enqueue(eagerOp{id: playerID, entry: e})
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
	z.shard.presence.join(s.character, s.entity.Name(), playerAFK(s), concealedForRoster(s.entity))
}

// concealedForRoster reports whether entity e should be marked CONCEALED in the cross-shard presence roster
// (#98) — i.e. omitted from an ordinary viewer's `who`. It is the roster counterpart to the zone-local canSee
// filter: a player is concealed if they carry any target-side concealment an ordinary viewer can't pierce —
// magical invisibility, mundane hiding, or staff wizinvis. (A holylight viewer still sees them; renderWho
// takes a seeAll flag for that.) Read on the zone goroutine, where the flags are single-writer-owned; the
// resulting bit rides the roster Entry so the off-goroutine `who` reader never has to touch live entity state.
//
// COARSENING NOTE (wizinvis): local visibleTo hides wizinvis only from STRICTLY-lower ranks, but the roster
// Entry carries neither the bearer's rank nor the reader's, so the cross-shard bit is binary: a wizinvis
// staffer is concealed from everyone-but-holylight in cross-shard `who`. That is the FAIL-SAFE direction — it
// never leaks a hidden builder to a mortal on another shard (the leak this issue closes); the only cost is
// that an equal/higher-rank peer must use holylight to see them cross-shard, which staff already carry.
func concealedForRoster(e *Entity) bool {
	return hasFlag(e, flagInvisible) || hasFlag(e, flagHidden) || hasFlag(e, flagWizinvis)
}

// republishPresenceOnConcealChange refreshes e's cross-shard roster entry after its concealment flags may
// have changed (an effect op set/cleared invisible/hidden, or a staffer toggled wizinvis), so the `who`
// roster reflects the new state without waiting for a re-login. A no-op for a non-player entity or a bare/
// disabled shard. Mirrors republishCommsOnAccessChange (the comms-hearing analog). Zone goroutine only.
//
// INVARIANT (keep the roster bit fresh): every mutation of a concealment flag (isConcealmentFlag) MUST be
// followed by this call. Today the only writers are opSetFlag/opClearFlag (invisible/hidden — reserved
// wizinvis is refused there) and cmdWizinvis, and all three call it. If concealment ever becomes affect-
// native (an affect that grants/strips invisible on apply/expire), those sites must call this too — else a
// stale roster bit would leak or over-hide a player in cross-shard `who` until their next login/heartbeat.
func (z *Zone) republishPresenceOnConcealChange(e *Entity) {
	if e == nil {
		return
	}
	if s, ok := sessionOf(e); ok && s != nil {
		z.presenceJoin(s)
	}
}

// presenceLeave records that this zone no longer hosts the player (clean quit/leave or handed-off orphan
// reap), eagerly removing them from `who`. No-op on a bare zone / disabled tracker.
func (z *Zone) presenceLeave(id string) {
	if z.shard == nil || z.shard.presence == nil {
		return
	}
	z.shard.presence.leave(id)
}

// playerAFK reports the session's AFK flag for presence (Phase 8.6): true iff the player has set AFK
// (the `afk` command). Read from the in-memory comms state; nil/all-default => not AFK. The flag rides
// the presence roster so `who` marks an AFK player.
func playerAFK(s *session) bool {
	return s != nil && s.comms != nil && s.comms.afk
}

// rosterList reads the whole cross-shard presence roster for `who` (off the zone goroutine — see cmdWho).
// Returns nil + false when presence is disabled (no roster) or the read errors, so cmdWho falls back to
// the zone-local list.
func (z *Zone) rosterList(ctx context.Context) ([]roster.Entry, bool) {
	if z.shard == nil || z.shard.presence == nil || !z.shard.presence.enabled() {
		return nil, false
	}
	return z.shard.presence.cachedList(ctx)
}

// rosterConnected reports whether playerID has a LIVE presence entry — i.e. is CURRENTLY connected on some
// shard right now — using the same cross-shard roster (and its ~1s cache) `who` reads. This is the correct
// oracle for a "is this character connected?" decision (#325), distinct from the directory placement, which
// only answers "has ever logged in / where do we route" (a placement persists across logout, and since #320
// is written on every login). Returns false when presence is DISABLED or the roster read fails: the caller
// (the new-mail notify ping) is a best-effort nicety, so when we cannot DEMONSTRATE a live connection we
// send none — the mail is durable and the recipient sees it when they next run `mail` (there is no
// automatic on-login mail notice). Membership is checked against the RAW
// roster (not the visibility-filtered set): the ping goes to the recipient's OWN gate, so a concealed player
// must still be notified of their own mail. Runs OFF the zone goroutine (the mail send I/O goroutine);
// cachedList's whoMu makes that safe, exactly like cmdWho's read.
func (z *Zone) rosterConnected(ctx context.Context, playerID string) bool {
	entries, ok := z.rosterList(ctx)
	if !ok {
		return false
	}
	for i := range entries {
		if entries[i].PlayerID == playerID {
			return true
		}
	}
	return false
}

// cachedList returns the cross-shard roster, serving a sub-whoCacheTTL cached snapshot when one exists so a
// `who` flood collapses to ONE Redis SCAN per window. whoMu serializes the refresh: at the window edge one
// goroutine does the List (holding the lock) and the rest block briefly, then read the just-refreshed cache
// — so N concurrent `who` cost one SCAN, not N. A List error degrades to the zone-local fallback (ok=false),
// the unchanged contract. Runs off the zone goroutine (cmdWho).
func (p *presenceTracker) cachedList(ctx context.Context) ([]roster.Entry, bool) {
	r := p.currentRoster()
	if r == nil {
		return nil, false
	}
	p.whoMu.Lock()
	defer p.whoMu.Unlock()
	if p.whoOK && time.Since(p.whoAt) < whoCacheTTL {
		return p.whoCache, true
	}
	entries, err := r.List(ctx)
	if err != nil {
		return nil, false // a roster read error degrades to the zone-local fallback (unchanged contract)
	}
	p.whoCache, p.whoAt, p.whoOK = entries, time.Now(), true
	return entries, true
}

// renderWho formats the cross-shard presence roster as the player-visible `who` list. Same shape as the
// zone-local who (a "Players online:" header + one indented name per player), extended for 8.4: an AFK
// player is marked. Sorted by name for a stable, readable list across shards.
//
// Concealment (#98): an Entry the hosting shard marked Concealed (invisible/hidden/wizinvis) is OMITTED for
// an ordinary viewer — the cross-shard counterpart to the zone-local canSee filter (whoLocal), which the
// roster path previously couldn't honor. seeAll is the viewer's own see-all capability (holylight), captured
// on the zone goroutine before the async roster read: a holylight staffer still sees concealed players.
//
// This is the built-in FALLBACK shown when a pack defines no `who` display template (#24). It shares the one
// concealment/sort rule (visibleRosterEntries) with the templated path, so the two can never diverge.
func renderWho(entries []roster.Entry, seeAll bool) string {
	var b strings.Builder
	b.WriteString("Players online:")
	for _, e := range visibleRosterEntries(entries, seeAll) {
		b.WriteByte('\n')
		b.WriteByte(' ')
		b.WriteString(e.Name)
		if e.AFK {
			b.WriteString(" (AFK)")
		}
	}
	return b.String()
}

// visibleRosterEntries applies the #98 concealment filter and sorts by display name — the exact rule
// renderWho renders with, lifted out so the CONTENT `who` template consumes the same already-filtered set.
// A concealed entry never reaches a template: the filter runs in Go, before any binding (a template that
// could see a concealed row could leak it, however it chose to render).
func visibleRosterEntries(entries []roster.Entry, seeAll bool) []roster.Entry {
	out := make([]roster.Entry, 0, len(entries))
	for _, e := range entries {
		if e.Concealed && !seeAll {
			continue // concealed from an ordinary cross-shard viewer
		}
		if e.Name == "" {
			e.Name = e.PlayerID // the display name, as renderWho computes it
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// renderWhoSheet renders the CONTENT `who` template (surface "who") for viewer, binding `self` to the viewer's
// handle and `list` to the visible roster rows as plain-data records. Returns ("", false) when the pack defines
// no `who` template or the body fails — cmdWho then falls back to the built-in renderWho.
//
// Roster rows are REMOTE, cross-shard data (a player hosted on another shard has no *Entity here), so each row
// binds as a snapshot record — {name, shard, afk, concealed} — never as an entity handle (T7).
//
// SINGLE-WRITER: this enters the zone's Lua VM, so it MUST run on the zone goroutine. Its one caller is the
// whoRenderMsg inbox handler, which is exactly where the async roster read posts its result back to.
func (z *Zone) renderWhoSheet(viewer *Entity, entries []roster.Entry, seeAll bool) (string, bool) {
	visible := visibleRosterEntries(entries, seeAll)
	records := make([]*displayRecord, 0, len(visible))
	for _, e := range visible {
		records = append(records, newDisplayRecord().
			str("name", e.Name).
			str("shard", e.ShardID).
			boolean("afk", e.AFK).
			// `concealed` is always false for an ordinary viewer (those rows are filtered out above); a
			// holylight staffer sees the bit so a staff template can mark who is hiding.
			boolean("concealed", e.Concealed))
	}
	return z.renderDisplayList("who", viewer, records)
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
