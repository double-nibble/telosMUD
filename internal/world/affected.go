package world

// affected.go is the Affected runtime (docs/ABILITIES.md §5, docs/PHASE5-PLAN.md §1.4): the engine
// owns an affect's duration/stacking/tick/expire and feeds its modifiers into attribute derivation
// (§1.1) and its prevents tags into the tag-CC gate (§6). Content defines the affect; the engine
// knows the KIND. Slice 5.2 builds the RUNTIME with a stat-modifying + CC affect; an affect's
// on_tick op-list (a poison DoT) is a RESERVED hook here — the tick MECHANISM is live, the gated op
// execution lands in 5.3 when the effect-op interpreter exists. Resource regen (a self-effect on the
// entity's own pool, no PvP concern) DOES ride the tick this slice.
//
// # The single-mod-source design (resolves the 5.1 carry-forward)
//
// The Affected component IS the single modSource for the entity (attributes.go §1.1). It is
// registered ONCE via addModSource on the first attach, and its flatMod/mulMod SUM over every
// currently-active affect's modifiers. This is deliberately NOT one source per affect: that was the
// 5.1-review bug class (addModSource accumulation — a source per affect that never gets removed on
// expire). One source per entity, mutated in place, keeps the modSrcs slice bounded and the dirty
// invalidation simple. Any change (apply / stack / expire) recomputes the summed modifiers + the
// prevents union and DIRTIES the entity's attr cache (markAttrsDirty), so the next attr() recomputes.
//
// # Single-writer
//
// Every field here is zone-goroutine-owned: attach is called from a command/op (zone goroutine), the
// tick fires on the pulse scheduler (zone goroutine, pulse.go), and expire runs inside the tick. No
// lock; no DB I/O on the zone goroutine. The component's reference-typed fields are reset to empty on
// a COW (component.go cloneComponent) — a prototype-authored entity carries no runtime affects.

// affectInstance is one live affect on an entity: which def, who applied it, how much time remains,
// its magnitude and stack count. Keyed in Affected.byKey by (ref[, source]) per the def's stack_scope.
type affectInstance struct {
	def       *affectDef
	source    *Entity // the actor that applied it (nil for a self/ambient affect); part of the key
	remaining int     // pulses left before expiry; decremented each tick, expires at 0
	magnitude float64 // the applied magnitude (scales modifier/tick effect); 1 by default
	stacks    int     // current stack count (>=1); scales magnitude for stackCount affects
	sinceTick int     // pulses since the last on_tick fire (for tick.interval counting)
}

// Affected is the live status-effect component (component.go KindAffected). It holds the entity's
// active affect instances, the SUMMED modifier contribution it feeds derivation as a single mod
// source, and the union prevents set the tag-CC query reads. It is added to an entity on its first
// affect (attach) and carries the per-entity tick handle so the tick is registered once and cancelled
// when the entity has neither affects nor a regen need.
//
// Affected implements modSource (flatMod/mulMod) — it IS the entity's single affect-sourced mod
// source. It is registered via addModSource on first attach (registered==true thereafter).
type Affected struct {
	// list is the active affect instances in stable application order (tick iterates it; modifiers
	// sum over it). byKey indexes it for the stacking lookup. Kept in sync by attach/expire.
	list  []*affectInstance
	byKey map[affectKey]*affectInstance

	// flat/mul are the PRE-SUMMED modifier contribution across every active affect, recomputed
	// (recomputeMods) on any apply/stack/expire. flatMod/mulMod just read these maps — O(1) on the
	// hot derivation path, not O(affects). nil maps read as the identity (0 add / 1 mul).
	flat map[string]float64
	mul  map[string]float64

	// prevents is the UNION of every active affect's prevents tags (a set), recomputed alongside the
	// modifiers. preventsTag reads it; the engine never names a CC type (§6 — open strings).
	prevents map[string]int // tag -> count of active affects preventing it (a multiset for clean removal)

	// registered records that this component has already been addModSource'd onto the entity, so a
	// second attach does not register a duplicate source (the single-source invariant).
	registered bool

	// tick is the per-ENTITY pulse handle (NOT per affect): one callback drives every affect's
	// countdown + tick + expiry AND resource regen. nil when no tick is registered. Set by
	// ensureTick, cancelled by maybeStopTick when the entity has neither affects nor a regen need.
	tick *pulseHandle
}

func (*Affected) componentKind() Kind { return KindAffected }

// affectKey identifies an affect instance for the stacking lookup. The source is part of the key only
// for stack_scope=="source" (the default — one instance per (ref, applier)); for stack_scope=="target"
// the source is zeroed so there is one instance per ref regardless of who applied it.
type affectKey struct {
	ref    string
	source *Entity
}

// keyFor builds the stacking key for def applied by source, honouring the def's stack_scope.
func keyFor(def *affectDef, source *Entity) affectKey {
	if def.scopeTarget {
		return affectKey{ref: def.ref} // per-ref: ignore the applier
	}
	return affectKey{ref: def.ref, source: source}
}

// flatMod / mulMod implement modSource: the entity's affect-sourced additive/multiplicative
// contribution to attribute `ref`, pre-summed in recomputeMods. The identity (0 / 1) for an attr no
// active affect touches. Hot-path read on the zone goroutine — a plain map lookup, no per-affect scan.
func (a *Affected) flatMod(ref string) float64 {
	if a.flat == nil {
		return 0
	}
	return a.flat[ref]
}

func (a *Affected) mulMod(ref string) float64 {
	if a.mul == nil {
		return 1
	}
	if v, ok := a.mul[ref]; ok {
		return v
	}
	return 1
}

// preventsTag reports whether any active affect on the entity prevents tag `tag` (§6 tag CC). It is
// the standalone query 5.3's lifecycle step-3 enforcement will call; testable on its own this slice.
// O(1) — reads the pre-unioned prevents multiset. A nil/absent Affected component prevents nothing.
func preventsTag(e *Entity, tag string) bool {
	a, ok := Get[*Affected](e)
	if !ok || a.prevents == nil {
		return false
	}
	return a.prevents[tag] > 0
}

// preventsAny reports whether any active affect prevents ANY of the given tags — the exact step-3
// query shape (does an affect block any tag this ability carries?). Returns the first blocked tag.
func preventsAny(e *Entity, tags []string) (string, bool) {
	a, ok := Get[*Affected](e)
	if !ok || a.prevents == nil {
		return "", false
	}
	for _, t := range tags {
		if a.prevents[t] > 0 {
			return t, true
		}
	}
	return "", false
}

// affectedComponent returns the entity's Affected component, lazily creating + adding it on first
// use. The created component is registered as the entity's single affect mod source (addModSource)
// the first time, satisfying the single-source invariant. Single-writer: zone goroutine.
func affectedComponent(e *Entity) *Affected {
	if a, ok := Get[*Affected](e); ok {
		return a
	}
	a := &Affected{byKey: map[affectKey]*affectInstance{}}
	Add(e, a)
	// Register the component as the entity's SINGLE affect-sourced mod source, exactly once. From here
	// flatMod/mulMod (summed over active affects) feed derivation (attributes.go §1.1). addModSource
	// dirties the cache. The registered flag guards against a duplicate source if this ever re-runs.
	if !a.registered {
		addModSource(e, a)
		a.registered = true
	}
	return a
}

// hasActiveAffects reports whether the entity carries any live affect (the tick keeps running while
// true). A nil/absent Affected component has none.
func (a *Affected) hasActiveAffects() bool { return a != nil && len(a.list) > 0 }
