package world

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// reload.go is the shard-side hot-reload applier (docs/PHASE4-PLAN.md §5). It subscribes to the
// content bus; on an invalidation it RE-READS just the named (kind, ref) definition from the
// content source, REBUILDS the one *Prototype via the same DTO->component mapper the boot loader
// uses (content_map.go), and SWAPS it into the per-shard prototype cache race-safely
// (protoCache.reload — the atomic table swap, prototype.go). The NEXT spawn of that ref uses the
// new data; live instances spawned earlier keep the old prototype (it stays alive via GC), which
// is the documented MUD semantics: an existing mob keeps its stats, the next repop uses the edit.
//
// # Optional, never fatal
//
// Hot reload is OPTIONAL: a shard with no bus OR no source has a nil reloader and behaves exactly
// as a pre-4.3 shard (boot-load still works, every storeless/busless test stays green). A bus
// subscribe failure is logged, never fatal. Mirrors WithPersistence's disabled fallback.
//
// # Single-writer of the cache swap
//
// The bus delivers invalidations SERIALLY per subscription, so the applier runs reloads one at a
// time on the subscription's own goroutine — it is the sole RUNTIME writer of the cache table.
// spawn never writes the table (only get -> atomic Load), so the swap races neither a reader nor
// another reload. This is the per-shard concurrency contract the distributed-systems-architect
// scrutinizes: the cache is a shared-read structure mutated only by atomic whole-table swap.

// reloadIOTimeout bounds the single-ref re-read so a slow/hung content source cannot wedge the
// subscriber goroutine (which would silently stop every later reload). On timeout the reload is
// abandoned; the last-known prototype stays in the cache and the next invalidation retries.
const reloadIOTimeout = 5 * time.Second

// reloader holds the hot-reload wiring for one shard: the content source to re-read a single
// definition from, the shared prototype cache to swap into, and the live bus subscription. It is
// nil on a shard with no bus/source (hot reload disabled).
type reloader struct {
	src   content.DefinitionSource // single-ref re-read (Postgres in prod, embedded/mem in tests)
	cache *protoCache              // the per-shard cache whose entries this swaps (shared read)
	bus   contentbus.Bus           // the invalidation bus
	sub   contentbus.Subscription  // live subscription; Unsubscribe on stop
	packs map[string]bool          // packs this shard loads; an edit to another pack is ignored
	// enabled is the same pack set as packs, kept as the ORDERED boot list so the `reload` staff command
	// (reloadcmd.go) can iterate + display them deterministically. Set once at construction, read-only.
	enabled []string
	shard   *Shard // the hosting shard — to post a reloadLuaMsg to each zone (7.7); nil on a bare test
	log     *slog.Logger
}

// newReloader wires a reloader over src/cache/bus for the given enabled packs and SUBSCRIBES. A
// nil bus or nil src yields a nil reloader (hot reload disabled). A subscribe failure logs and
// returns nil — never fatal, so an unreachable/closed bus simply disables hot reload.
func newReloader(src content.DefinitionSource, cache *protoCache, bus contentbus.Bus, enabledPacks []string, shard *Shard) *reloader {
	if bus == nil || src == nil || cache == nil {
		return nil
	}
	r := &reloader{
		src:     src,
		cache:   cache,
		bus:     bus,
		packs:   map[string]bool{},
		enabled: append([]string(nil), enabledPacks...),
		shard:   shard,
		log:     slog.With("component", "contentreload"),
	}
	for _, p := range enabledPacks {
		r.packs[p] = true
	}
	sub, err := bus.Subscribe(r.onInvalidation)
	if err != nil {
		r.log.Warn("content invalidation subscribe failed; hot reload disabled", "err", err)
		return nil
	}
	r.sub = sub
	r.log.Debug("hot reload enabled", "packs", enabledPacks)
	return r
}

// onInvalidation is the bus handler: it runs OFF every zone goroutine (the bus's subscription
// goroutine), serially per subscription. It filters by pack, re-reads the single definition,
// rebuilds the prototype, and swaps it into the cache. Every failure is non-fatal (logged):
// hot reload is best-effort and never disturbs the running world beyond the one ref it targets.
func (r *reloader) onInvalidation(inv contentbus.Invalidation) {
	// Ignore an edit to a pack this shard does not load (an empty pack matches nothing). A shard
	// only caches prototypes from its enabled packs, so a foreign-pack invalidation is a no-op.
	if inv.Pack != "" && !r.packs[inv.Pack] {
		r.log.Debug("invalidation ignored: pack not loaded here", "kind", inv.Kind, "ref", inv.Ref, "pack", inv.Pack)
		return
	}
	r.log.Debug("invalidation received", "kind", inv.Kind, "ref", inv.Ref, "pack", inv.Pack)

	// Phase 8.3: a `channel` invalidation reloads a pack-GLOBAL channel_def into the per-shard channel
	// REGISTRY (the atomic-swap defRegistry), not the prototype cache. It is a different swap target
	// (a channelDef, not a *Prototype) and a different table, so it forks off here before the
	// prototype path. Channel verbs are derived from the registry on each dispatch (channelForVerb), so
	// a verb added/removed by the edit takes effect with no second swap.
	if inv.Kind == content.KindChannel {
		r.reloadChannel(inv)
		return
	}

	// A `zone` invalidation carries no spawnable prototype — it drives the live-room-SHAPE reconcile
	// (#191). Re-read the zone's room set and post a reconcile to the hosting zone so it TEARS DOWN any
	// room the edit deleted (a deletion emits no per-ref invalidation, so this is the only signal). It
	// forks off before the prototype path, like the channel case.
	if inv.Kind == content.KindZone {
		r.reloadZoneShape(inv)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), reloadIOTimeout)
	defer cancel()
	def, err := r.src.LoadDefinition(ctx, inv.Kind, inv.Ref, inv.Pack)
	if err != nil {
		// Infrastructure failure on the re-read: keep the last-known prototype (do NOT drop it on
		// a transient read error — that would empty the ref on a Postgres blip). The next
		// invalidation retries.
		r.log.Warn("hot reload re-read failed; keeping last-known prototype",
			"kind", inv.Kind, "ref", inv.Ref, "err", err)
		return
	}

	ref := ProtoRef(inv.Ref)
	if !def.Found {
		// The row was deleted/renamed: remove the entry so a later spawn of this ref returns nil
		// (logged as unknown) rather than serving a now-orphaned prototype. Live instances already
		// spawned keep their aliased prototype (GC holds it) — they are unaffected, by design.
		r.cache.reload(ref, nil)
		r.log.Debug("hot reload: prototype removed (definition deleted)", "kind", inv.Kind, "ref", inv.Ref)
		r.notifyZones(inv.Kind, inv.Ref) // a deleted scripted proto → its live instances go scriptless
		return
	}

	p := buildPrototype(def)
	if p == nil {
		r.log.Warn("hot reload: unbuildable definition, skipped", "kind", inv.Kind, "ref", inv.Ref)
		return
	}
	// The atomic whole-table swap (prototype.go reload): the new prototype is published in one
	// step; concurrent spawns see the old or the new table, never a half-applied map. The old
	// prototype stays alive while any live instance aliases it.
	r.cache.reload(ref, p)
	r.log.Debug("hot reload: prototype swapped", "kind", inv.Kind, "ref", inv.Ref)
	// Notify each hosted zone to apply the LUA reload on its own goroutine (7.7): recompile the
	// chunk, re-register live instances' handlers (keeping self.state), bump the timer generation,
	// reset the breaker. The shared-cache swap above is the cross-goroutine-safe publish; the
	// per-zone Lua state is updated single-writer via the inbox post.
	r.notifyZones(inv.Kind, inv.Ref)
}

// reloadChannel applies a `channel` content hot reload (Phase 8.3): it re-reads the single edited
// channel_def, rebuilds the runtime channelDef via the SAME mapper the boot loader uses
// (buildChannelDef), and SWAPS it into the per-shard channel registry race-safely (defRegistry.reload
// — the atomic whole-table swap). A deleted channel (Found=false) is REMOVED from the registry, so its
// verb stops resolving and a speak attempt falls through to "Huh?". Every failure is non-fatal
// (logged): hot reload is best-effort and never disturbs the running world beyond the one channel.
//
// It runs OFF every zone goroutine (the content-bus subscription goroutine), serially per
// subscription, and the registry swap is the cross-goroutine-safe publish (a zone goroutine reading
// channelForVerb sees the old or the new table whole). Channel verbs are DERIVED from the registry on
// each dispatch, so a verb the edit added/removed/renamed takes effect with no further notification —
// nothing per-zone to re-register (unlike a scripted prototype's Lua handlers).
func (r *reloader) reloadChannel(inv contentbus.Invalidation) {
	if r.shard == nil || r.shard.defs == nil {
		// A bare reloader (a test without a shard) has no channel registry to swap; nothing to do.
		return
	}
	reg := r.shard.defs.channel
	if reg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reloadIOTimeout)
	defer cancel()
	def, err := r.src.LoadDefinition(ctx, inv.Kind, inv.Ref, inv.Pack)
	if err != nil {
		// Infrastructure failure on the re-read: keep the last-known channel_def (do NOT drop it on a
		// transient read error). The next invalidation retries.
		r.log.Warn("hot reload re-read failed; keeping last-known channel", "ref", inv.Ref, "err", err)
		return
	}
	if !def.Found {
		// The channel was deleted/renamed: remove it so its verb stops resolving.
		reg.reload(inv.Ref, nil, true)
		r.log.Debug("hot reload: channel removed (definition deleted)", "ref", inv.Ref)
		return
	}
	reg.reload(inv.Ref, buildChannelDef(def.Channel), false)
	r.log.Debug("hot reload: channel swapped", "ref", inv.Ref)
}

// reloadZoneShape applies a `zone` content hot reload (#191): it re-reads the whole zone definition and
// posts a reconcile to the hosting zone so the zone goroutine diffs its live room set against the
// reloaded content and TEARS DOWN any room the edit DELETED (removeRoom — re-place occupants, despawn
// ephemera). ADD/UPDATE stay on the per-ref applier (resyncRoom); this closes ONLY the deletion gap the
// per-ref path structurally cannot see — a deleted room's ref is absent from the content, so PublishPack
// never names it. Runs OFF every zone goroutine (the content-bus subscription goroutine); the reconcile
// itself runs single-writer on the zone goroutine via the inbox post.
//
// It re-reads the WHOLE pack (LoadPacks) rather than a single ref, because the reconcile needs the zone's
// full room set (which rooms SHOULD be live), not one prototype — so it requires a source that implements
// content.Source (all production + embedded sources do). A shard only re-reads for a zone it HOSTS (the
// host check precedes the re-read), so a fleet of shards each pays only for its own zones. Every failure
// is non-fatal (logged): the live room set simply stays as-is until a re-run.
//
// A zone that VANISHED from content entirely is deliberately NOT reconciled to an empty room set —
// tearing down every room (including the start/login room) out from under players is a rolling-reboot
// operation, not a hot reload — so it is skipped with a warning.
func (r *reloader) reloadZoneShape(inv contentbus.Invalidation) {
	if r == nil || r.shard == nil {
		return
	}
	src, ok := r.src.(content.Source)
	if !ok {
		// A DefinitionSource that cannot re-read whole packs (a bare test double): the per-ref ADD/UPDATE
		// path still works; only the deletion reconcile is unavailable here.
		return
	}
	// Only a shard HOSTING this zone has a live room set to reconcile; skip (and skip the re-read) otherwise.
	var target *Zone
	for _, z := range r.shard.zonesList() { // mu-guarded against a runtime HostZone (16.4a)
		if z.id == inv.Ref {
			target = z
			break
		}
	}
	if target == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reloadIOTimeout)
	defer cancel()
	// FOLLOW-UP (scaling, #207): this re-reads the WHOLE pack per KindZone
	// invalidation, and PublishPack emits one per zone — so a shard hosting M zones of a pack does M full-pack LoadPacks per
	// reload, serially on this one subscriber goroutine (each discarding the other zones via findZoneDTO).
	// Gated to hosted zones (the target check above), so a shard pays only for its own zones, but it is
	// still O(M × full-pack-cost) of redundant Postgres I/O that stacks against every later invalidation.
	// Fine for v1 (reloads are rare, correctness is unaffected); coalesce KindZones of one pack into a
	// single read + reconcile-all-hosted-zones before many-zone content-heavy shards.
	packs, err := src.LoadPacks(ctx, []string{inv.Pack})
	if err != nil {
		r.log.Warn("hot reload: zone-shape re-read failed; live room set unchanged", "zone", inv.Ref, "err", err)
		return
	}
	zd := findZoneDTO(packs, inv.Ref)
	if zd == nil {
		r.log.Warn("hot reload: zone no longer in content; skipping shape reconcile "+
			"(removing a whole zone is a rolling-reboot operation, not a hot reload)", "zone", inv.Ref)
		return
	}
	want := make(map[ProtoRef]bool, len(zd.Rooms))
	for i := range zd.Rooms {
		want[ProtoRef(zd.Rooms[i].Ref)] = true
	}
	msg := reloadZoneMsg{zoneRef: inv.Ref, wantRooms: want, startRoom: ProtoRef(zd.StartRoom)}
	// NON-BLOCKING post (same drop-vs-block tradeoff as notifyZones): a full zone inbox DROPS the reconcile
	// rather than head-of-line-stalling every later invalidation. A dropped reconcile LOSES a room DELETION
	// until `reload` re-runs — so it warns loudly. Bounded-reliable delivery is the tracked follow-up (#194).
	if !target.postOrDrop(msg) {
		slog.Warn("hot-reload zone-shape reconcile dropped (zone inbox full); a room DELETION is lost until re-run",
			"zone", inv.Ref)
	}
}

// findZoneDTO returns the zone DTO with ref within packs, or nil if none carries it. A pointer into the
// re-read slice — the caller only reads it (the room set + start room), never retains it past the post.
func findZoneDTO(packs []content.Pack, ref string) *content.ZoneDTO {
	for pi := range packs {
		for zi := range packs[pi].Zones {
			if packs[pi].Zones[zi].Ref == ref {
				return &packs[pi].Zones[zi]
			}
		}
	}
	return nil
}

// reloadLua applies a content Lua hot reload for (kind, ref) ON THE ZONE GOROUTINE (slice 7.7,
// P7-D7 / §1.1). The shard reloader already swapped the new prototype/def into the shared cache;
// this re-runs on each hosted zone so the per-zone Lua state (the chunk cache, the LState, the
// per-instance entityScripts) — all zone-owned — is updated single-writer. It:
//
//  1. BUMPS the chunk generation so pending old-gen mud.after timers DROP at fire (don't run old
//     code against new state; a durable=true finalizer still completes).
//  2. INVALIDATES the per-(kind,ref) shared chunk cache entry (an ability on_resolve / affect hook /
//     formula / pvp policy) so the NEXT invocation recompiles from the new source. The source-aware
//     chunkFor would recompile anyway on a changed source, but a SHARED def's source lives on the
//     swapped shared registry, not passed here — so we drop the cache entry and let the next entry-
//     point invocation re-read + recompile (the security-critical pvp_allowed case takes effect on
//     its next consult, never the stale permissive policy).
//  3. RE-REGISTERS the handlers of every LIVE instance of a scripted prototype (a mob/room with a
//     `lua` block) from the NEW source, PRESERVING each instance's self.state (the DATA survives).
//  4. RESETS the circuit breaker for that def so a script a bug had quarantined is re-enabled by the
//     fix reload.
func (z *Zone) reloadLua(kind, ref string) {
	if z.lua == nil {
		return
	}
	rt := z.lua
	rt.chunkGen++ // (1) old-gen mud.after timers drop at fire (P7-D7)

	// (2) Invalidate the shared chunk cache entries that key off this ref, so the next invocation
	// recompiles from the swapped registry source. Keys are "<area>:<ref>:<hook>" / "<area>:<ref>" /
	// "formula:<name>" / "pvp_allowed". We drop every cache entry whose key contains the ref (cheap;
	// the chunk cache is small) plus the global policy/formula keys, and reset their breakers.
	for key := range rt.chunks {
		if strings.Contains(key, ref) || key == "pvp_allowed" || strings.HasPrefix(key, "formula:") {
			delete(rt.chunks, key)
			rt.breakerReset(breakerKeyShared(key)) // (4) re-enable a quarantined shared def
		}
	}

	// (3) Re-register live instances of the reloaded scripted prototype from the new source, keeping
	// their self.state. A non-prototype (ability/affect/formula) reload has no instances to walk.
	rt.reloadEntityScriptsForProto(ProtoRef(ref))

	z.log.Debug("lua hot reload applied", "kind", kind, "ref", ref, "gen", rt.chunkGen)
}

// notifyZones posts a reloadLuaMsg to every hosted zone's inbox so each applies the Lua reload on
// its own goroutine (the per-zone Lua state is zone-owned). Called by the subscriber goroutine
// AFTER the shared prototype cache swap; the post is the cross-goroutine-safe hand-off (the only
// sanctioned way to reach zone state). A reloader with no shard (a bare test) skips it.
func (r *reloader) notifyZones(kind, ref string) {
	if r == nil || r.shard == nil {
		return
	}
	for _, z := range r.shard.zonesList() { // mu-guarded: safe against a runtime HostZone (16.4a)
		// NON-BLOCKING fan-out: a blocking post here would let ONE saturated (or wedged) zone inbox
		// head-of-line-stall every LATER zone's invalidation shard-wide, and a wedged zone would halt hot
		// reload entirely (distsys review) — so a full inbox DROPS the message. For a Lua/def reload that
		// self-heals (the next invocation recompiles from the swapped source). For a ROOM it does NOT: a room
		// is a singleton that never re-spawns, so a dropped room invalidation leaves the live room's resync
		// (or a new-room add, resyncRoom) UN-applied until another reload of that ref lands — hence the loud
		// warn, and the operator remedy is to re-run `reload`. Reliable room-reload delivery (bounded retry /
		// a reconciliation sweep) is a tracked follow-up, #194.
		if !z.postOrDrop(reloadLuaMsg{kind: kind, ref: ref}) {
			slog.Warn("hot-reload invalidation dropped (zone inbox full); a Lua/def reload self-heals on next "+
				"access, but a ROOM reload is lost until re-run",
				"zone", z.id, "kind", kind, "ref", ref)
		}
	}
}

// stop unsubscribes the reloader from the bus. Idempotent; safe on a nil reloader.
func (r *reloader) stop() {
	if r == nil || r.sub == nil {
		return
	}
	_ = r.sub.Unsubscribe()
}

// buildPrototype turns a single re-read Definition into a fresh *Prototype using the SAME
// DTO->component mapper the boot loader uses (content_map.go), so a hot-reloaded prototype is
// byte-identical to one built at boot. It does NOT touch the cache — the caller swaps it in. A
// nil result means the kind carries no spawnable prototype (e.g. a zone definition). Note: a room
// prototype's display name lives in RoomDTO.Name (short) and its long in RoomDTO.Long, matching
// defineContent's room define call.
func buildPrototype(def content.Definition) *Prototype {
	switch def.Kind {
	case content.KindRoom:
		r := def.Room
		return newPrototype(ProtoRef(r.Ref), nil, r.Name, r.Long, roomComponents(r))
	case content.KindItem, content.KindMob:
		d := def.Proto
		return newPrototype(ProtoRef(d.Ref), d.Keywords, d.Short, d.Long, protoComponents(d))
	default:
		return nil
	}
}
