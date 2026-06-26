package world

// resources.go is the content-defined resource model (docs/ABILITIES.md §1, docs/PHASE5-PLAN.md
// §1.2). A resource is a named pool whose MAX is a derived attribute (resourceDef.maxAttr) and whose
// CURRENT the engine holds per entity (Living.resCur). So gear/affects that raise max_hp flow through
// derivation (§1.1) to the cap automatically. Slice 5.1 builds the read/clamp; regen ticks and the
// vital on_depleted hook ride the pulse in 5.2/combat (the shape is reserved on resourceDef).
//
// Single-writer: resource currents are zone-goroutine-owned instance state on Living.

// resourceMax returns the derived maximum of resource `name` on entity e: attr(e, def.maxAttr).
// A resource with no maxAttr (or an unknown resource) reports 0 (no cap authored). With no content
// the entity has no resource defs, so this returns 0 and the accessors behave sanely.
func resourceMax(e *Entity, name string) int {
	if e == nil || e.living == nil || e.zone == nil {
		return 0
	}
	def := e.zone.resourceDefs().get(name)
	if def == nil || def.maxAttr == "" {
		return 0
	}
	return int(attr(e, def.maxAttr))
}

// resourceCurrent returns the current value of resource `name`, CLAMPED to its derived max (so a
// max that gear/base changes lowered never reports a stale-high current). An absent current reads
// as the max (a freshly-loaded entity is "full") when a max is defined, else 0. Read-only; clamping
// is computed, not stored — the stored current is only mutated by setResourceCurrent.
func resourceCurrent(e *Entity, name string) int {
	if e == nil || e.living == nil {
		return 0
	}
	max := resourceMax(e, name)
	cur, ok := e.living.resCur[name]
	if !ok {
		// No explicit current yet: full when a max is known, else 0 (contentless).
		return max
	}
	if max > 0 && cur > max {
		return max
	}
	if cur < 0 {
		return 0
	}
	return cur
}

// setResourceCurrent stores a resource's current, clamped into [0, max]. The CANONICAL stored value
// is the clamped one, so a later max RAISE does not silently restore over-cap headroom (the store
// is the floor truth; resourceCurrent re-clamps on read against the live max for the lower-max
// case). Single-writer: zone goroutine. A no-op on an entity with no Living.
func setResourceCurrent(e *Entity, name string, v int) {
	if e == nil || e.living == nil {
		return
	}
	if e.living.resCur == nil {
		e.living.resCur = map[string]int{}
	}
	max := resourceMax(e, name)
	if v < 0 {
		v = 0
	}
	if max > 0 && v > max {
		v = max
	}
	e.living.resCur[name] = v
	// If this drops a regen-able pool below its max, make sure the per-entity tick is running so the
	// pool refills over time (affect_runtime.go ensureTick). The tick re-resolves the entity by id and
	// stops when it has neither affects nor a regen need. A no-op when the tick is already registered
	// or the entity is not on a running zone's pulse path. Zone goroutine only.
	if e.zone != nil && e.zone.pulses != nil && needsRegen(e) {
		affectedComponent(e).ensureTick(e)
	}
}

// hasResource reports whether resource `name` is content-defined (a def is registered). Used to tell
// "this engine knows hp" from "no content" — the bare-engine accessors fall back when false.
func hasResource(e *Entity, name string) bool {
	if e == nil || e.zone == nil {
		return false
	}
	return e.zone.resourceDefs().has(name)
}

// needsRegen reports whether the entity has at least one content-defined resource with a positive
// regen rate that is not already at its derived max — i.e. whether the per-entity tick has regen work
// to do. It is the second reason (besides active affects) the tick stays registered (affect_runtime.go
// ensureTick/maybeStopTick). A contentless/Living-less entity has none. Zone goroutine only.
func needsRegen(e *Entity) bool {
	if e == nil || e.living == nil || e.zone == nil {
		return false
	}
	for ref, def := range e.zone.resourceDefs().table() {
		if def.regen <= 0 {
			continue
		}
		if resourceCurrent(e, ref) < resourceMax(e, ref) {
			return true
		}
	}
	return false
}

// runRegen moves each content-defined resource's CURRENT toward its derived max by the resource_def's
// per-tick regen rate, clamped (never overshooting the max). This is a SELF-effect on the entity's own
// pool — no PvP concern — so it rides the affect tick this slice (docs/PHASE5-PLAN.md §1.4 / 5.2 scope
// boundary). Death/on_depleted is Phase 6 — reserved (regen only raises, never crosses 0). A resource
// already absent (no current stored) reads as full, so regen is a no-op until something spends it.
// Single-writer: zone goroutine (the pulse).
func runRegen(e *Entity) {
	if e == nil || e.living == nil || e.zone == nil {
		return
	}
	for ref, def := range e.zone.resourceDefs().table() {
		if def.regen <= 0 {
			continue
		}
		max := resourceMax(e, ref)
		if max <= 0 {
			continue
		}
		cur := resourceCurrent(e, ref)
		if cur >= max {
			continue
		}
		next := cur + def.regen
		if next > max {
			next = max
		}
		setResourceCurrent(e, ref, next) // clamps defensively too
	}
}
