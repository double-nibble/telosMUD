package world

import (
	"sync"
	"sync/atomic"
)

// defs.go holds the per-shard registries for the pack-GLOBAL definition kinds (attributes,
// resources, damage types — Phase 5.1; affects/abilities arrive in 5.2/5.3). These are NOT the
// prototype cache: a prototype is an instancing template the spawner clones, whereas these are
// flat content DEFINITIONS the runtime reads by ref (attr() looks up an attributeDef, the resource
// model looks up a resourceDef). They are content (docs/ABILITIES.md §1) — the engine knows the
// KIND, content supplies the instances.
//
// # The atomic-swap shape (mirrors protoCache, prototype.go)
//
// Each registry is a defRegistry[T]: a single per-shard atomic.Pointer to an immutable ref->def
// table, swapped wholesale. Reads (the hot attr()/resource paths, on ANY zone goroutine) are a
// lock-free atomic.Load; writes (build-time register, the 4.3-style hot reload) copy-then-Store a
// fresh table under writeMu. This is the EXACT pattern protoCache uses, so a later slice can
// hot-reload a damage_type/attribute without restart by dropping a reload() in — the shape is
// already here even though invalidation isn't published this slice.
//
// Like protoCache, the registries are built once at shard construction (before any zone goroutine
// runs) and then only read, so the cross-goroutine sharing needs no lock beyond the publish.

// defRegistry is the generic atomic-swap registry for one definition kind. T is a pointer to the
// def struct (so a Load returns nil for an absent ref, never a zero-struct false positive).
type defRegistry[T any] struct {
	// live is the published table, swapped atomically. Every read (get) Loads it; every write
	// (register/reload) Stores a fresh copy with the one entry changed. Holding a pointer to the
	// map makes the swap a single atomic op. NEVER index a Loaded table for WRITE.
	live atomic.Pointer[map[string]T]
	// writeMu serializes the WRITE path (register/reload's read-copy-store) so two writers can't
	// both copy the same base and clobber each other. Readers never take it.
	writeMu sync.Mutex
}

// newDefRegistry builds an empty registry with an empty published table.
func newDefRegistry[T any]() *defRegistry[T] {
	r := &defRegistry[T]{}
	empty := map[string]T{}
	r.live.Store(&empty)
	return r
}

// table returns the currently published table (always non-nil after newDefRegistry).
func (r *defRegistry[T]) table() map[string]T { return *r.live.Load() }

// get returns the def for ref, or the zero value of T (nil for a pointer T) when absent. Read-only
// and safe from any zone goroutine: a pure atomic.Load, never racing a concurrent reload swap.
func (r *defRegistry[T]) get(ref string) T {
	var zero T
	if d, ok := r.table()[ref]; ok {
		return d
	}
	return zero
}

// has reports whether ref is registered.
func (r *defRegistry[T]) has(ref string) bool {
	_, ok := r.table()[ref]
	return ok
}

// len reports how many defs are registered (used by the bare-engine assertion: 0 with no pack).
func (r *defRegistry[T]) len() int { return len(r.table()) }

// register publishes def under ref, copy-then-swap under writeMu (build-time path: uncontended,
// the cache is still private to the construction goroutine). It leaves the registry PUBLISHED so
// the runtime read path is identical whether a ref was registered at boot or hot-reloaded.
func (r *defRegistry[T]) register(ref string, def T) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur := r.table()
	next := make(map[string]T, len(cur)+1)
	for k, v := range cur {
		next[k] = v
	}
	next[ref] = def
	r.live.Store(&next)
}

// reload atomically replaces (or, with a zero def, the caller uses remove) the def for ref AT
// RUNTIME — the 4.3-style hot-reload swap. It builds a FRESH table copy with the one entry changed
// and Stores it, so every concurrent reader sees the whole old or whole new table, never a partial.
// This slice never CALLS reload (no invalidation is published yet); it exists so a later slice
// drops in hot reload without touching the read path. (Kept for symmetry with protoCache.reload.)
func (r *defRegistry[T]) reload(ref string, def T) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	cur := r.table()
	next := make(map[string]T, len(cur))
	for k, v := range cur {
		next[k] = v
	}
	next[ref] = def
	r.live.Store(&next)
}

// defRegistries bundles the per-shard global-definition registries so a zone holds one pointer
// (mirroring how it holds one *protoCache). Built once at shard construction, shared read-only.
type defRegistries struct {
	attr   *defRegistry[*attributeDef]
	res    *defRegistry[*resourceDef]
	dmg    *defRegistry[*damageTypeDef]
	affect *defRegistry[*affectDef]
}

// newDefRegistries builds an empty bundle (all three registries empty/published). A bare zone gets
// its own so attr()/resource reads work standalone and report 0/absent — the bare-engine invariant.
func newDefRegistries() *defRegistries {
	return &defRegistries{
		attr:   newDefRegistry[*attributeDef](),
		res:    newDefRegistry[*resourceDef](),
		dmg:    newDefRegistry[*damageTypeDef](),
		affect: newDefRegistry[*affectDef](),
	}
}

// attrDefs / resourceDefs / damageTypeDefs are the zone-goroutine read accessors for the global
// registries. Each is a lock-free atomic.Load under the hood. A bare zone (no shard) falls back to
// its own empty bundle so the reads never nil-deref and report "no content defined".
func (z *Zone) attrDefs() *defRegistry[*attributeDef] {
	return z.defBundle().attr
}
func (z *Zone) resourceDefs() *defRegistry[*resourceDef] {
	return z.defBundle().res
}
func (z *Zone) damageTypeDefs() *defRegistry[*damageTypeDef] {
	return z.defBundle().dmg
}
func (z *Zone) affectDefs() *defRegistry[*affectDef] {
	return z.defBundle().affect
}

// defBundle returns the zone's registry bundle, lazily creating an empty private one if a bare zone
// was constructed without it (defensive — newZone wires one). Single-writer (zone goroutine).
func (z *Zone) defBundle() *defRegistries {
	if z.defs == nil {
		z.defs = newDefRegistries()
	}
	return z.defs
}

// --- The def structs (runtime forms of the content DTOs) -------------------------------------

// attributeDef is the runtime form of an AttributeDTO: a content-defined attribute with its base
// (literal or a parsed formula AST) and an optional clamp range. It is immutable after build —
// shared read-only across zone goroutines via the registry, exactly like a *Prototype.
type attributeDef struct {
	ref         string
	displayName string
	valueKind   string // "int" | "float" | "derived"

	// base is the default base of the attribute, evaluated against an entity's attributes when no
	// per-entity override is present. nil means base 0. A literal is a litNode; a derived attribute
	// is an arbitrary formula tree (formula.go). attr() resolves it recursively (derived-of-derived).
	base formulaNode

	// min/max clamp the resolved value (after mods). nil means unbounded on that side.
	min *float64
	max *float64
}

// resourceDef is the runtime form of a ResourceDTO: a named pool whose MAX is a derived attribute
// (maxAttr) — so gear/affects that raise that attribute flow through to the cap (§1.2). The engine
// holds `current` per entity (Living); this def supplies max/vital/regen. Immutable after build.
type resourceDef struct {
	ref               string
	displayName       string
	maxAttr           string // derived-attr ref capping the pool; "" => no cap (unbounded)
	vital             bool   // depletion drives death (on_depleted) — wired in 5.2/combat
	regen             int    // per-tick flat regen (reserved; regen ticks ride 5.2)
	depletedThreshold int    // reserved (vital depletion threshold)
}

// damageTypeDef is the runtime form of a DamageTypeDTO: a named damage type with a resist/vuln/
// immune matrix (other-type ref -> multiplier). The shared mitigation pipeline (5.3) reads it.
type damageTypeDef struct {
	ref         string
	displayName string
	color       string
	resist      map[string]float64
}

// affectStacking is the stacking mode of an affect_def (P5-D3, docs/PHASE5-PLAN.md §1.4). It governs
// what happens when an affect is applied to a target that already has an instance keyed by the same
// (ref[, source]). The default (zero / unknown) is refresh.
type affectStacking int

const (
	stackRefresh affectStacking = iota // reset duration to full (default); buffs like haste
	stackCount                         // count up to maxStacks, magnitude scales; DoTs like poison
	stackExtend                        // sum remaining + new duration
	stackIgnore                        // first wins; the new application is a no-op
)

// parseStacking maps the content stacking string onto the enum. Unknown/"" => refresh (the §5 default).
func parseStacking(s string) affectStacking {
	switch s {
	case "stack":
		return stackCount
	case "extend":
		return stackExtend
	case "ignore":
		return stackIgnore
	default:
		return stackRefresh
	}
}

// affectModifier is one parsed entry of an affect's modifier list: it adds (add==true) `value` to
// attribute `attr` or multiplies by it (add==false) while the affect is active. The Affected runtime
// sums/multiplies these across active affects into the entity's single mod source (attributes.go §1.1).
type affectModifier struct {
	attr  string
	add   bool // true => additive (flatMod); false => multiplicative (mulMod)
	value float64
}

// affectDef is the runtime form of an AffectDTO (docs/ABILITIES.md §5): a content-defined status
// effect. Immutable after build — shared read-only across zone goroutines via the registry, exactly
// like a *Prototype/*attributeDef. The Affected runtime reads it on attach/tick/expire.
type affectDef struct {
	ref         string
	name        string
	category    string
	stacking    affectStacking
	maxStacks   int  // ceiling for stackCount; >=1
	scopeTarget bool // stack_scope=="target": one instance per ref (ignore source); else per (ref,source)
	dispellable bool

	duration int // base duration in PULSES (heartbeat-denominated; conserved across save/load)

	modifiers []affectModifier // additive/multiplicative attribute mods while active
	prevents  []string         // tags this affect blocks (§6 tag CC); the runtime unions these

	tickInterval int  // fire on_tick every N pulses; 0 => no tick
	hasTick      bool // whether a tick spec was authored (interval may legitimately be 0-guarded)
	// onTick is the RESERVED tick op-list (docs/PHASE5-PLAN.md §1.4 / 5.2 scope boundary): the tick
	// MECHANISM (interval counting + the hook point) is live this slice; the gated op execution lands
	// in 5.3 when the effect-op interpreter exists. Carried opaque so 5.3 wires it without a reparse.
	onTick any
	// onApply/onExpire are the RESERVED apply/expire hooks (5.3). Read-not-run this slice.
	onApply  any
	onExpire any
}
