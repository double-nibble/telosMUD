package world

import "reflect"

// Components (docs/MUDLIB.md §3). A component is a typed struct granting a capability;
// an Entity is identity + containment + a bag of these. Access is generic and
// type-safe via Get/Must/Has/Add below. This is "ECS-lite": composition over
// inheritance, but the world interacts with whole objects, not hot loops over
// component arrays — so the store is a per-entity map keyed by component type, not a
// columnar array.
//
// Slice 1 (docs/PHASE3-PLAN.md) defines the interface and the three components the
// session split needs (Room, Living, PlayerControlled, see components.go); the rest of
// the core set arrives as later slices make them functional.

// Kind is a stable tag for a component category. It is intentionally cheap (an int
// enum) so future code paths — serialization, Lua registration, the COW prototype
// cache (slice 3) — can switch on a component's kind without a type assertion. The map
// keys off reflect.Type, not Kind, so two structs can't collide; Kind is the
// human/serialization-facing label.
type Kind int

// Kind values: the entity-kind tags an Entity carries. KindInvalid is the zero value.
const (
	KindInvalid Kind = iota
	KindPhysical
	KindLiving
	KindPlayerControlled
	KindMob
	KindRoom
	KindContainer
	KindWearable
	KindWeapon
	KindArmor
	KindSkilled
	KindAffected
	KindScripted
)

// Component is the capability a struct grants when added to an entity. The single
// method makes the set closed and self-describing: every concrete component declares
// its Kind, and only declared components can live in a componentSet.
type Component interface {
	componentKind() Kind
}

// componentSet is the per-entity component store (MUDLIB §3). Keyed by the dynamic
// reflect.Type of the component so each component type appears at most once on an
// entity and the generic accessors below can do an exact-type lookup. Only the owning
// zone goroutine ever touches it, so it needs no lock.
//
// The hot components Living and Room are ALSO held as direct typed pointers on Entity
// (the MUDLIB §3 escape hatch); Add keeps both views in sync so the map stays the
// source of truth for Has/Get while the pointer stays the source of truth for the
// movement/look/combat fast path.
type componentSet map[reflect.Type]Component

// Get looks up the component of type T on e and asserts it. Returns the zero value and
// false when absent (MUDLIB §3).
func Get[T Component](e *Entity) (T, bool) {
	var zero T
	if e.comps == nil {
		return zero, false
	}
	c, ok := e.comps[reflect.TypeFor[T]()]
	if !ok {
		return zero, false
	}
	return c.(T), true
}

// Must returns the component of type T on e, panicking if absent. Use only where an
// invariant guarantees presence (e.g. a room entity always has *Room).
func Must[T Component](e *Entity) T {
	c, ok := Get[T](e)
	if !ok {
		panic("world: missing required component " + reflect.TypeFor[T]().String())
	}
	return c
}

// Has reports whether e carries a component of type T.
func Has[T Component](e *Entity) bool {
	_, ok := Get[T](e)
	return ok
}

// Add installs component c of type T on e, replacing any existing one of the same
// type. It also promotes the two hot components (*Room, *Living) to their direct
// pointer fields on the entity so the fast paths never pay a map lookup (MUDLIB §3).
func Add[T Component](e *Entity, c T) {
	if e.comps == nil {
		e.comps = componentSet{}
	}
	e.comps[reflect.TypeFor[T]()] = c
	// Keep the direct-pointer escape hatches in sync. The any-assertion is cheap and
	// runs only at add time, never on the hot path. The pointer is valid only while the
	// component is present: if a component-remove API is ever added, it must nil the
	// matching e.room / e.living there to avoid a dangling pointer.
	switch v := any(c).(type) {
	case *Room:
		e.room = v
	case *Living:
		e.living = v
	}
}

// --- Component copy-on-write (MUDLIB §5) -----------------------------------------------
//
// A prototype-backed instance starts with the prototype's CANONICAL component pointers in
// its comps map (and in the e.room/e.living hot fields): the component DATA is shared with
// the prototype and every sibling instance. Before mutating a component, a caller takes a
// mutable copy via mutableComponent, which performs copy-on-write the first time: it clones
// the shared component (deep-copying its reference-typed fields), installs the clone on
// THIS instance only, and returns it. Subsequent mutations of the same component on the
// same instance see the already-owned clone and don't re-copy.
//
// "Still shared" is detected by pointer identity against the prototype's template
// component: spawn aliases the exact pointer, and the COW clone replaces it, so identity
// distinguishes shared-immutable from instance-owned. An entity with no prototype is always
// already-owned (nothing to protect), so mutableComponent returns its component unchanged.

// cloneComponent returns a deep-enough copy of c that mutating the copy cannot alias the
// original: every reference-typed field (slice/map) is reallocated. A component type with
// only value fields is copied by the shallow struct copy alone. New component types with
// reference-typed fields MUST be handled here, or a COW of that component would alias the
// prototype — the default panics loudly rather than silently sharing.
func cloneComponent(c Component) Component {
	switch v := c.(type) {
	case *Room:
		cp := *v // shallow copy of the value fields (sector, flags)
		cp.exits = make(map[string]ProtoRef, len(v.exits))
		for k, dst := range v.exits {
			cp.exits[k] = dst
		}
		return &cp
	case *Living:
		// Living gained reference-typed instance state in Phase 5.1 (attrBase/resCur maps + the
		// derivation cache). A COW MUST reallocate the maps or a spawned mob mutating its bases/
		// currents would alias the prototype's (players are prototype==nil and never reach this
		// path, but a future prototype-backed mob does). The attrs cache is a pure function of
		// bases+mods+defs, so the clone starts with an EMPTY (dirty) cache — it recomputes lazily.
		// fighting is a live pointer, intentionally instance-set (copied by pointer here, like
		// before). position is a value field copied by the shallow struct copy.
		cp := *v
		if v.attrBase != nil {
			cp.attrBase = make(map[string]float64, len(v.attrBase))
			for k, val := range v.attrBase {
				cp.attrBase[k] = val
			}
		}
		if v.resCur != nil {
			cp.resCur = make(map[string]int, len(v.resCur))
			for k, val := range v.resCur {
				cp.resCur[k] = val
			}
		}
		if v.flags != nil {
			cp.flags = make(map[string]bool, len(v.flags))
			for k, val := range v.flags {
				cp.flags[k] = val
			}
		}
		cp.attrs = attrCache{dirty: true} // fresh, empty, dirty: recompute lazily on this instance
		// modSrcs are runtime-registered (affects/gear); the clone starts with NONE so it never
		// aliases the prototype's slice — a COW'd instance re-registers its own sources (5.2).
		cp.modSrcs = nil
		return &cp
	case *PlayerControlled:
		cp := *v
		if v.aliases != nil {
			cp.aliases = make(map[string]string, len(v.aliases))
			for k, val := range v.aliases {
				cp.aliases[k] = val
			}
		}
		return &cp
	case *Physical:
		cp := *v
		return &cp
	case *Container:
		cp := *v // all value fields (capacity/closed/locked/keyRef)
		return &cp
	case *Wearable:
		cp := *v // single value field (locations bitmask)
		return &cp
	case *Weapon:
		cp := *v // all value fields (dice/type/class/verb)
		return &cp
	case *Affected:
		// The Affected component carries only RUNTIME state (live affect instances, the summed
		// modifier maps, the prevents set, the per-entity tick handle) — a prototype never authors
		// affects, so a COW resets every reference-typed field to EMPTY rather than aliasing the
		// prototype's. The clone re-registers its own mod source on its first attach (registered=false),
		// and re-arms its own tick — a COW'd instance never inherits the prototype's tick handle (which
		// would be a stale cross-instance pointer). In practice a prototype-backed mob spawns with no
		// Affected component at all; this case exists so the COW `default` panic never fires if one is
		// ever authored, satisfying the component.go invariant.
		cp := &Affected{byKey: map[affectKey]*affectInstance{}}
		return cp
	case *Wearer:
		// The worn map is reference-typed: a COW must reallocate it so a spawned mob
		// re-equipping itself never aliases the prototype's worn map (players are
		// prototype==nil and never reach this path). The mapped *Entity values are live
		// instance objects, copied by pointer.
		cp := *v
		cp.worn = make(map[WearLoc]*Entity, len(v.worn))
		for loc, e := range v.worn {
			cp.worn[loc] = e
		}
		return &cp
	default:
		panic("world: cloneComponent missing case for " + reflect.TypeOf(c).String() +
			" — a COW of this component would alias the prototype")
	}
}

// mutableComponent returns the instance's own copy of its component of type T, performing
// copy-on-write the first time (component.go preamble). The caller mutates the returned
// pointer freely: the write lands only on this instance, never on the prototype or a
// sibling. Panics if the entity has no component of type T (mirrors Must — use only where
// presence is invariant-guaranteed, e.g. mutating a room's exits on a known room entity).
func mutableComponent[T Component](e *Entity) T {
	cur := Must[T](e)
	if e.prototype == nil {
		return cur // no prototype to protect: already instance-owned
	}
	if shared, ok := e.prototype.comps[reflect.TypeFor[T]()]; ok && any(cur) == any(shared) {
		// Still aliased to the prototype's template component: clone it onto this instance.
		owned := cloneComponent(cur).(T)
		Add(e, owned) // replaces the comps entry AND re-promotes the *Room/*Living hot pointer
		e.zone.log.Debug("cow: component", "rid", e.rid, "proto", e.proto,
			"kind", reflect.TypeFor[T]().String())
		return owned
	}
	return cur // already instance-owned (COW'd earlier, or never shared)
}

// mutableRoom is the hot-path COW shortcut for a room entity's Room component (MUDLIB §3
// escape hatch + §5 COW): it returns an instance-owned *Room whose exits map may be safely
// mutated, copying-on-write off the prototype the first time. Equivalent to
// mutableComponent[*Room](e) but named for the common movement/builder call site.
func mutableRoom(e *Entity) *Room { return mutableComponent[*Room](e) }
