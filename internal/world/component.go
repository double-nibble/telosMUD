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
