package world

import (
	"context"
	"log/slog"
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
	log   *slog.Logger
}

// newReloader wires a reloader over src/cache/bus for the given enabled packs and SUBSCRIBES. A
// nil bus or nil src yields a nil reloader (hot reload disabled). A subscribe failure logs and
// returns nil — never fatal, so an unreachable/closed bus simply disables hot reload.
func newReloader(src content.DefinitionSource, cache *protoCache, bus contentbus.Bus, enabledPacks []string) *reloader {
	if bus == nil || src == nil || cache == nil {
		return nil
	}
	r := &reloader{
		src:   src,
		cache: cache,
		bus:   bus,
		packs: map[string]bool{},
		log:   slog.With("component", "contentreload"),
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
