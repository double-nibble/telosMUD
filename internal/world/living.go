package world

// living.go holds the public accessors for an entity's vitals and stats (docs/PHASE5-PLAN.md §6:
// "Living made real behind accessors"). Phase 1-4 carried hp/maxHP/mp/mv as raw int fields on
// Living; Phase 5.1 replaced them with content-defined resources + attributes. To keep call sites
// (and the handoff/character/zone/COW tests) from churning, EVERY read goes through one of these
// accessors, which resolve through the resource/derivation model (resources.go / attributes.go).
//
// The accessors name the well-known stdlib resources/attributes ("hp", "mana", "strength", …) as a
// CONVENIENCE for engine call sites that want a vital by its conventional name. The names are NOT
// hardcoded mechanics — they are plain ref lookups into the content registries, so an entity whose
// content does not define "hp" simply reports 0 (the bare-engine invariant). The pillar holds: the
// engine stores/queries by ref; content gives the refs meaning.
//
// Single-writer: these read zone-goroutine-owned state and must be called on the owning goroutine.

// The conventional resource/attribute refs the accessors below resolve. Content that wants the
// engine accessors to light up uses these refs; content is free to define others and read them via
// the generic Attr/Resource accessors directly.
const (
	resHP   = "hp"
	resMana = "mana"
)

// Attr is the generic resolved-attribute accessor on an entity: attr(e, ref) through the full
// modifier stack (base -> flat mods -> multipliers -> clamp), memoized. Returns 0 for an unknown
// attribute or a stat-less/contentless entity. The public face of attributes.go's attr().
func (e *Entity) Attr(ref string) float64 { return attr(e, ref) }

// Resource is the generic resource-current accessor: the live current of pool `ref`, clamped to its
// derived max. 0 for an unknown resource / no content. The public face of resources.go.
func (e *Entity) Resource(ref string) int { return resourceCurrent(e, ref) }

// ResourceMax is the derived maximum of pool `ref` (attr(e, def.maxAttr)). 0 when no cap/def.
func (e *Entity) ResourceMax(ref string) int { return resourceMax(e, ref) }

// HP / MaxHP / Mana / MaxMana are the conventional-vital accessors the engine and combat (Phase 6)
// use. Each is a thin lookup of the corresponding stdlib resource by its conventional ref, so a
// world whose content omits that resource reports 0 — never a panic, never a stale stub field.
func (e *Entity) HP() int      { return resourceCurrent(e, resHP) }
func (e *Entity) MaxHP() int   { return resourceMax(e, resHP) }
func (e *Entity) Mana() int    { return resourceCurrent(e, resMana) }
func (e *Entity) MaxMana() int { return resourceMax(e, resMana) }

// SetHP / SetMana set a conventional vital's current, clamped to [0, derived max] (resources.go).
// Single-writer: zone goroutine. A no-op when the entity has no Living.
func (e *Entity) SetHP(v int)   { setResourceCurrent(e, resHP, v) }
func (e *Entity) SetMana(v int) { setResourceCurrent(e, resMana, v) }
