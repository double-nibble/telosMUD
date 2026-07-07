package world

import (
	"reflect"
	"sync"
	"sync/atomic"
)

// Prototypes & instancing — flyweight + copy-on-write (docs/MUDLIB.md §5, §8 D1).
//
// Content authors define PROTOTYPES (immutable templates keyed by ProtoRef); the world
// spawns INSTANCES. An instance is a lightweight DELTA over its prototype: the shared
// immutable fields (keywords, base short/long, base component data) are REFERENCED from
// the prototype, and only fields that actually change on the instance are stored locally.
// The first mutation of a shared field copies it onto the instance (copy-on-write), so a
// write to one instance never touches the prototype or a sibling. This keeps a room of
// 40 identical kobolds cheap: 40 thin Entity headers over one shared *Prototype.
//
// # Shared-immutable vs instance-local (the single-writer / no-aliasing contract)
//
// SHARED-IMMUTABLE (lives on the *Prototype, read by many instances across MANY zone
// goroutines on the same shard, NEVER mutated after construction):
//   - keywords []string, short, long
//   - the component template (each *Room/*Physical/... and every slice/map reachable
//     from it)
//
// INSTANCE-LOCAL (lives on the Entity, written only by the owning zone goroutine):
//   - rid, pid, zone (identity/ownership)
//   - location, contents (containment is NEVER shared — see spawn)
//   - any field or component the instance has COW'd
//
// The prototype cache is built once at shard construction (before any zone goroutine
// runs) and is read-only thereafter, so the cross-goroutine sharing of *Prototype needs
// no lock: it is publication-then-immutable. Any code path that mutated a value reachable
// from a *Prototype would be a cross-zone data race; the COW helpers below exist precisely
// so no such path exists.
//
// # Hot reload and the atomic table swap (docs/PHASE4-PLAN.md §5)
//
// Slice 4.3 makes the cache reloadable AT RUNTIME: an operator edits a definition row and
// publishes an invalidation, and every shard rebuilds JUST that *Prototype and swaps it in,
// so the NEXT spawn uses the new data. The hot path (spawn -> get) reads the cache locklessly
// on EVERY zone goroutine, so a runtime swap of a shared map would be a cross-goroutine data
// race. The fix is NOT to mutate the shared map in place; it is to publish a NEW immutable map.
//
// Mechanism (chosen of the two in §5): a single per-shard atomic.Pointer to the map TABLE
// (protoCache.live). spawn/get do one atomic Load — lock-free, one indirection, the hot path
// stays cheap. A reload (or a build-time define) builds a NEW table = a copy of the current
// one with the one entry replaced, then atomically Stores it. Readers either see the whole old
// table or the whole new one, never a half-written map. The OLD *Prototype stays alive as long
// as any live instance still aliases it (Go GC), so a reload NEVER touches in-flight COW deltas
// — live instances keep the prototype they spawned from; only later spawns see the edit.
//
// Why the whole-table swap (§5 option 1) and not a per-zone reload message (§5 option 2): the
// cache is per-SHARD, shared by every hosted zone, so "apply the swap on each zone goroutine"
// would still have N zone goroutines mutating ONE shared map — the very race we are removing —
// unless each zone owned a private copy, which would defeat the per-shard flyweight. A shared
// structure read across goroutines wants a shared-atomic swap: it is the simplest CORRECT fit,
// keeps spawn lock-free, and confines the only writer to whoever calls reload (serialized by
// the shard's single reload applier). The map copy is O(#refs) and a reload is rare, so the
// per-reload allocation is negligible against keeping every spawn lock-free.

// Prototype is an immutable template (MUDLIB §5). After build() returns it, nothing
// mutates it or anything reachable from it; many instances on many zone goroutines read
// it concurrently and safely. Its component template (comps) holds canonical component
// pointers that instances share by reference until they COW.
type Prototype struct {
	ref      ProtoRef     // stable content key (identity.go); also each instance's e.proto
	keywords []string     // base targeting tokens, shared by reference until an instance COWs
	short    string       // base inline name
	long     string       // base room/ground line
	comps    componentSet // component template: canonical pointers shared until COW
}

// newPrototype is the single *Prototype constructor (the build entry point both define and the
// hot-reload swap use, so a reloaded prototype is built identically to a booted one). It
// normalizes a nil component set to empty and takes ownership of the immutable template fields.
func newPrototype(ref ProtoRef, keywords []string, short, long string, comps componentSet) *Prototype {
	if comps == nil {
		comps = componentSet{}
	}
	return &Prototype{ref: ref, keywords: keywords, short: short, long: long, comps: comps}
}

// protoCache is the per-shard prototype registry (MUDLIB §5: "cached per shard"). It maps
// ProtoRef -> *Prototype and is shared READ-ONLY across the shard's zone goroutines. It is
// populated entirely during shard construction (newShard, before Run), then only read — so
// it needs no lock. It is NOT per-zone: a shard's zones share one cache, which is what makes
// the flyweight pay off across the whole process.
type protoCache struct {
	// live is the published table of prototypes, swapped atomically. Every read (get/spawn,
	// from any zone goroutine) Loads it; every write (define at build time, reload at runtime)
	// Stores a fresh copy with the one changed entry. Holding a pointer to the map (not the map
	// directly) is what makes the swap a single atomic operation. NEVER index a Loaded table for
	// WRITE — that would mutate a table other goroutines are reading.
	live atomic.Pointer[protoTable]

	// writeMu serializes the WRITE path only (define/reload's read-copy-store), so two writers
	// can't both copy the same base table and clobber each other's update (the atomic Store is
	// last-writer-wins on a stale copy otherwise). Readers never take it — get/spawn are a pure
	// atomic.Load. There are TWO runtime writers — the reload applier (ADD/UPDATE, subscriber
	// goroutine) and a zone goroutine (ref DELETION via reconcileZone → removeRoom) — which
	// never target the same ref concurrently (see reload's doc comment); writeMu makes even that
	// partitioned pair memory-safe, and guards any future writer from silently losing an update.
	writeMu sync.Mutex
}

// protoTable is the immutable snapshot the atomic pointer publishes: a ref->prototype map that,
// once Stored into protoCache.live, is never mutated again. A define/reload builds a new one.
type protoTable map[ProtoRef]*Prototype

// newProtoCache builds an empty cache with an empty published table. Authoring (define) happens
// immediately after, on the construction goroutine, before any zone Run starts.
func newProtoCache() *protoCache {
	c := &protoCache{}
	c.live.Store(&protoTable{})
	return c
}

// table returns the currently published table (always non-nil after newProtoCache).
func (c *protoCache) table() protoTable { return *c.live.Load() }

// swap publishes next as the new live table by an atomic Store. The caller must have built next
// as a FRESH map (never the Loaded table mutated in place), so a reader holding the old table is
// undisturbed. Build-time define and runtime reload share this one publish point.
func (c *protoCache) swap(next protoTable) { c.live.Store(&next) }

// define registers a prototype under its ref. Build-time path: called from newShard while the
// cache is still private to the construction goroutine (single-threaded), so the copy-then-swap
// here is uncontended, and it leaves the cache already PUBLISHED (the atomic table holds it) so
// the runtime read path (get) is identical whether a ref was defined at boot or hot-reloaded. It
// takes ownership of keywords/short/long/comps as the canonical immutable template — the caller
// must not retain and mutate them afterward (they become shared-immutable).
func (c *protoCache) define(ref ProtoRef, keywords []string, short, long string, comps componentSet) *Prototype {
	p := newPrototype(ref, keywords, short, long, comps)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	cur := c.table()
	next := make(protoTable, len(cur)+1)
	for k, v := range cur {
		next[k] = v
	}
	next[ref] = p
	c.swap(next)
	return p
}

// reload atomically replaces (or, with p==nil, removes) the prototype for ref AT RUNTIME — the
// hot-reload swap (§5). It builds a FRESH table copy with the one entry changed and Stores it, so
// every concurrent spawn on every zone goroutine sees either the whole old table or the whole new
// one; the swap is never observed half-applied. The OLD *Prototype is left untouched and stays
// alive while any live instance aliases it (Go GC) — so a reload corrupts no in-flight COW delta;
// instances spawned BEFORE the reload keep the old prototype, spawns AFTER use the new one. A nil
// p (the ref's row was deleted) removes the entry: subsequent spawns of that ref return nil and
// are logged, exactly like an unknown ref, rather than serving a stale prototype.
//
// Concurrency: this copy-and-swap is writeMu-guarded, so ANY set of concurrent writers is
// memory-safe (each takes the lock, copies, and Stores; spawn never writes the table, only Loads).
// There are TWO runtime writers, and they never target the same ref concurrently:
//   - the shard's single reload applier (the contentbus subscriber goroutine) writes ADDs/UPDATEs of a
//     ref, serialized per subscription (reload.go onInvalidation);
//   - a ZONE goroutine writes a ref DELETION (nil p) when reconcileZone → removeRoom tears down a
//     room the content dropped (world.go), because a deletion emits no per-ref invalidation so the
//     applier never learns of it — the zone is the sole knower.
//
// These two never collide on one ref: a given pack read has a ref either present (applier ADD/UPDATE) or
// absent (zone DELETE), never both, so the writers partition the ref space. A future edit that adds a
// SUBSCRIBER-side delete path would reintroduce a same-ref writer race — keep deletions zone-driven.
func (c *protoCache) reload(ref ProtoRef, p *Prototype) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	cur := c.table()
	next := make(protoTable, len(cur))
	for k, v := range cur {
		if k == ref {
			continue // dropped (then re-added below if p != nil)
		}
		next[k] = v
	}
	if p != nil {
		next[ref] = p
	}
	c.swap(next)
}

// get returns the prototype for ref, or nil. Read-only and safe from any zone goroutine: it
// Loads the published table atomically, so it never races a concurrent reload's table swap.
func (c *protoCache) get(ref ProtoRef) *Prototype { return c.table()[ref] }

// exits returns the Room template's exits map for AUTHORING-TIME wiring only (newDemoZone
// populates it before any instance is spawned or any zone runs). After construction this
// map is shared-immutable — an instance that re-routes an exit must COW via mutableRoom, it
// must NEVER reach back into the prototype's map. Panics if the prototype has no Room
// template (called only on room prototypes).
func (p *Prototype) exits() map[string]ProtoRef {
	r, ok := p.comps[reflect.TypeFor[*Room]()]
	if !ok {
		panic("world: Prototype.exits on a non-room prototype " + string(p.ref))
	}
	return r.(*Room).exits
}

// protoIsPlayerControlled reports whether the prototype named by ref carries a PlayerControlled
// component template (luamud.go ISSUE-A — mud.spawn must reject spawning a player-controlled
// proto). An unknown ref reports false: the spawn then takes the unknown-proto nil path, not a
// rejection. Read-only; zone goroutine.
func (z *Zone) protoIsPlayerControlled(ref ProtoRef) bool {
	p := z.protos.get(ref)
	if p == nil {
		return false
	}
	_, ok := p.comps[reflect.TypeFor[*PlayerControlled]()]
	return ok
}

// protoScriptSource returns the CURRENT (post-reload) Lua trigger-block source of the prototype
// named by ref from the shared cache, or "" if the prototype is unknown or carries no Scripted
// component. Used by the hot-reload path (slice 7.7) to re-register live instances' handlers from
// the NEW source — a live instance still aliases its OLD prototype's *Scripted component (the
// flyweight is kept alive by the instance), so the reload reads the swapped prototype here, not
// the instance's stale component. Read-only; zone goroutine.
func (z *Zone) protoScriptSource(ref ProtoRef) string {
	p := z.protos.get(ref)
	if p == nil {
		return ""
	}
	if c, ok := p.comps[reflect.TypeFor[*Scripted]()]; ok {
		if s, ok := c.(*Scripted); ok {
			return s.source
		}
	}
	return ""
}

// spawn instantiates the prototype named by ref into zone z as a fresh Entity that is a
// DELTA over its prototype (MUDLIB §5). The instance:
//
//   - gets a fresh per-zone RuntimeID and this zone as its single-writer owner;
//   - REFERENCES the prototype's immutable fields — keywords/short/long are shared by
//     reference (the slice header and strings), and each component pointer in the template
//     is shared by reference via the instance's own comps map and the *Room/*Living hot
//     pointers;
//   - has its OWN, empty containment (location nil, contents nil): containment is never
//     shared, so two instances of one prototype never alias each other's contents.
//
// No field is copied here — sharing is the whole point. The first mutation of any shared
// field goes through a COW helper (mutableKeywords / setShort / setLong / mutableRoom /
// mutableComponent) which copies that one field onto the instance before writing, leaving
// the prototype and every sibling untouched. Returns nil if ref names no prototype.
//
// Single-writer: spawn runs on the zone goroutine that will own the instance; the only
// cross-goroutine thing it touches is the read-only prototype.
func (z *Zone) spawn(ref ProtoRef) *Entity {
	p := z.protos.get(ref)
	if p == nil {
		z.log.Warn("spawn: unknown prototype", "ref", ref)
		return nil
	}
	e := &Entity{
		rid:       z.rids.alloc(),
		proto:     p.ref,
		prototype: p,
		// Shared-immutable fields referenced from the prototype. These stay aliased to the
		// prototype until the instance COWs them; reads fall through transparently via the
		// Entity accessors (Name/Long/keywordList).
		keywords: p.keywords,
		short:    p.short,
		long:     p.long,
		zone:     z,
		// Component template: start the instance's comps map as a SHALLOW copy of the
		// template — same *Component pointers, so the component DATA (including its slices/
		// maps) is shared with the prototype. The map itself is instance-local so a later
		// mutableComponent swap (COW) replaces only this instance's entry, never the
		// template's. location/contents are left zero: containment is always instance-local.
		comps: make(componentSet, len(p.comps)),
	}
	for t, c := range p.comps {
		e.comps[t] = c
	}
	// Promote the two hot components to their direct pointers, still SHARED with the
	// prototype (mutableRoom/COW will replace the pointer on first write).
	if r, ok := e.comps[reflect.TypeFor[*Room]()]; ok {
		e.room = r.(*Room)
	}
	if l, ok := e.comps[reflect.TypeFor[*Living]()]; ok {
		e.living = l.(*Living)
	}
	// Debug events are emitted unconditionally; the slog handler filters them out unless
	// DEBUG is set (internal/obs), matching every other z.log.Debug call in this package.
	// A stackable material instance gets its own Stack{1} (Phase 13.2) — per-instance, never aliased to
	// the proto, so two material drops never share a count.
	ensureStack(e)
	z.log.Debug("spawn", "ref", ref, "rid", e.rid)
	return e
}
