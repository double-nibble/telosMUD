package world

// resources.go is the content-defined resource model (docs/ABILITIES.md §1, docs/PHASE5-PLAN.md
// §1.2). A resource is a named pool whose MAX is a derived attribute (resourceDef.maxAttr) and whose
// CURRENT the engine holds per entity (Living.resCur). So gear/affects that raise max_hp flow through
// derivation (§1.1) to the cap automatically. Slice 5.1 builds the read/clamp; regen ticks ride the pulse
// in 5.2/combat. A pool's on_depleted hook fires from the dealDamage checkpoint (effect_op.go), for any
// pool — vital only decides whether DEATH follows it (#406).
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
	maxV := resourceMax(e, name)
	cur, ok := e.living.resCur[name]
	if !ok {
		// No explicit current yet: full when a max is known, else 0 (contentless).
		return maxV
	}
	// NO CAPACITY READS AS NOTHING. A pool whose def declares a cap (max_attr) that currently derives to
	// <= 0 holds nothing, whatever happens to be stored. Without this the top-end clamp below is skipped
	// for max <= 0 and the pool reads back a POSITIVE current at max 0 — which quietly falsifies the
	// invariant the whole routing/immunity story rests on ("max <= 0 means the entity has no such track").
	// It is reachable without any routing: a legitimate write lands while the pool has capacity, then an
	// OnDamageTaken/OnHit handler or an expiring buff drops the derived max to 0 underneath it.
	//
	// Deliberately gated on maxAttr != "": a pool that declares NO cap is unbounded by design (resourceMax
	// reports 0 for it), and must keep reporting whatever it holds.
	if maxV <= 0 && declaresCap(e, name) {
		return 0
	}
	if maxV > 0 && cur > maxV {
		return maxV
	}
	if cur < 0 {
		return 0
	}
	return cur
}

// declaresCap reports whether resource `name` is content-defined WITH a cap (a max_attr). It separates
// "capped, and the cap currently derives to 0" (no capacity — reads as empty) from "declares no cap at
// all" (unbounded — reads whatever it holds). Zone-goroutine read.
func declaresCap(e *Entity, name string) bool {
	if e == nil || e.zone == nil {
		return false
	}
	def := e.zone.resourceDefs().get(name)
	return def != nil && def.maxAttr != ""
}

// setResourceCurrent stores a resource's current, clamped into [0, max]. The CANONICAL stored value
// is the clamped one, so a later max RAISE does not silently restore over-cap headroom (the store
// is the floor truth; resourceCurrent re-clamps on read against the live max for the lower-max
// case). Single-writer: zone goroutine. A no-op on an entity with no Living.
func setResourceCurrent(e *Entity, name string, v int) {
	l := mutableLiving(e) // COW: fork a proto-aliased mob's Living before mutating its resCur map (else combat damage writes the proto's hp)
	if l == nil {
		return
	}
	if l.resCur == nil {
		l.resCur = map[string]int{}
	}
	maxV := resourceMax(e, name)
	if v < 0 {
		v = 0
	}
	if maxV > 0 && v > maxV {
		v = maxV
	}
	l.resCur[name] = v
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

// restRegenMultiplier scales passive regen while an entity is posResting — the engine "resting heals
// faster" bonus (#39). An integer factor (1 = no bonus). A package var so a test can tune it (set it
// BEFORE ticking; it is read on the zone goroutine and must not be mutated concurrently with a live
// zone). A future content knob could make it per-resource. Applied in runRegen.
var restRegenMultiplier = 2

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
	fighting := position(e) == posFighting
	for ref, def := range e.zone.resourceDefs().table() {
		if def.regen <= 0 {
			continue
		}
		// Pause passive regen for a FIGHTING entity unless the resource opts into regen-in-combat. This is
		// the engine "no rest mid-fight" default (content flag `regen_in_combat`): without it a mob's hp
		// regen claws back a fresh player's per-round damage and the fight never ends. A troll's regen (or
		// a mana pool meant to tick in a fight) sets regen_in_combat: true to keep ticking. The per-entity
		// tick itself stays alive (needsRegen is not combat-gated), so regen resumes the instant combat ends.
		if fighting && !def.regenInCombat {
			continue
		}
		maxV := resourceMax(e, ref)
		if maxV <= 0 {
			continue
		}
		cur := resourceCurrent(e, ref)
		if cur >= maxV {
			continue
		}
		// Lua `regen` formula (7.4f): a pack may compute the per-tick regen amount in Lua (a
		// stat-scaled regen) as an alternative to the def's flat rate. Fail-closed: a missing/broken
		// Lua formula falls back to def.regen. The data formula OR the Lua one, never both.
		amount := def.regen
		if v, ok := e.zone.luaFormula("regen", e, nil); ok {
			amount = int(v)
		}
		// Resting bonus (#39): passive regen is faster while posResting — the engine "rest heals faster"
		// mechanic. Applied to whichever amount was chosen (flat or Lua). A tunable package var, not a
		// hardcoded literal; a future content knob could make it per-resource.
		if position(e) == posResting {
			amount *= restRegenMultiplier
		}
		next := cur + amount
		if next > maxV {
			next = maxV
		}
		setResourceCurrent(e, ref, next) // clamps defensively too
	}
}
