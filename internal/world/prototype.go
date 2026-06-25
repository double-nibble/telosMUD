package world

import "reflect"

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

// protoCache is the per-shard prototype registry (MUDLIB §5: "cached per shard"). It maps
// ProtoRef -> *Prototype and is shared READ-ONLY across the shard's zone goroutines. It is
// populated entirely during shard construction (newShard, before Run), then only read — so
// it needs no lock. It is NOT per-zone: a shard's zones share one cache, which is what makes
// the flyweight pay off across the whole process.
type protoCache struct {
	protos map[ProtoRef]*Prototype
}

// newProtoCache builds an empty cache. Authoring (define) happens immediately after, on
// the construction goroutine, before any zone Run starts.
func newProtoCache() *protoCache {
	return &protoCache{protos: map[ProtoRef]*Prototype{}}
}

// define registers a prototype under its ref. Build-time only: called from newShard while
// the cache is still private to the construction goroutine, never after a zone is running.
// It takes ownership of keywords/short/long/comps as the canonical immutable template — the
// caller must not retain and mutate them afterward (they become shared-immutable).
func (c *protoCache) define(ref ProtoRef, keywords []string, short, long string, comps componentSet) *Prototype {
	if comps == nil {
		comps = componentSet{}
	}
	p := &Prototype{ref: ref, keywords: keywords, short: short, long: long, comps: comps}
	c.protos[ref] = p
	return p
}

// get returns the prototype for ref, or nil. Read-only; safe from any zone goroutine
// because the cache is immutable after construction.
func (c *protoCache) get(ref ProtoRef) *Prototype { return c.protos[ref] }

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
	z.log.Debug("spawn", "ref", ref, "rid", e.rid)
	return e
}
