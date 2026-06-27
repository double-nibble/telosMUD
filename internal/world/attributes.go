package world

import (
	"fmt"
)

// attributes.go is the modifier-stack derivation (docs/ABILITIES.md §1, docs/PHASE5-PLAN.md §1.1).
// attr(e, name) resolves a content-defined attribute through the stack:
//
//	base            (per-entity override, else the attributeDef's default_base — a literal or a
//	                 derived FORMULA evaluated against OTHER attrs, recursive)
//	  -> + flat mods   (Σ additive modifiers: gear coarse this phase, affects in 5.2)
//	  -> × multipliers  (Π multiplicative modifiers)
//	  -> clamp(min,max) (from the attribute_def, if declared)
//
// Single-writer: the per-entity stat state (base overrides, the derivation cache, resource currents)
// is read/written ONLY on the owning zone goroutine. The attributeDef registry is the read-lock-free
// atomic-swap table (defs.go), so reading a def from the hot path never blocks.
//
// # Cache + invalidation (attr() is hot)
//
// Derived values memoize per entity in Living.attrCache, gated by a dirty bit. ANY change to a base
// override or a modifier source dirties the WHOLE cache (markAttrsDirty) — coarse but correct, and
// the common case (a combat round changing one affect) recomputes a handful of attrs, not a tree
// walk per attr() call. The cache is cleared, not selectively invalidated: derived-of-derived means
// a single base change can ripple anywhere, so a whole-cache flush is the simple correct choice.
//
// # Cycle detection
//
// Two layers: (1) a load-time content-lint (lintAttributeCycles) rejects a def graph with a self/
// mutual reference, so authored content can't ship a cycle; (2) a per-resolution visited set in
// resolveAttr errors defensively if one ever slips through (a hot-reloaded def, a future dynamic
// formula). The eval-time guard never fires for linted content; it is the backstop.

// modSource contributes additive/multiplicative modifiers to attributes. The Affected runtime (5.2)
// is the real implementer (affect-sourced mods); gear is coarse this phase. The derivation sums
// flatMod across every source and multiplies mulMod, so 5.2 just registers a source and feeds it —
// the PLUMBING is here now. A source returns 0 (add) / 1 (mul) for an attr it does not modify.
type modSource interface {
	// flatMod returns the additive modifier this source contributes to attr `ref` (0 if none).
	flatMod(ref string) float64
	// mulMod returns the multiplicative factor this source contributes to attr `ref` (1 if none).
	mulMod(ref string) float64
}

// attrCache memoizes resolved attribute values for one entity, gated by `dirty`. A dirty cache is
// flushed wholesale on the next read; a clean hit returns the stored value. Zone-goroutine-owned.
type attrCache struct {
	dirty  bool
	values map[string]float64
}

// markAttrsDirty invalidates an entity's whole derivation cache. Called whenever a base override or
// a modifier source changes (setAttrBase, gear change, affect apply/expire in 5.2). Cheap: it just
// flips the flag; the recompute is lazy on the next attr(). A no-op on an entity with no Living.
func markAttrsDirty(e *Entity) {
	l := mutableLiving(e) // COW: fork a proto-aliased mob's Living before touching its attrs cache (else the proto's cache dirties/recomputes)
	if l == nil {
		return
	}
	l.attrs.dirty = true
}

// attr resolves attribute `name` on entity e through the full modifier stack, memoized. It is the
// public hot-path accessor. With no Living or no such attributeDef it returns 0 (a contentless or
// stat-less entity behaves sanely — the bare-engine invariant). Single-writer: zone goroutine only.
func attr(e *Entity, name string) float64 {
	if e == nil || e.living == nil {
		return 0
	}
	// The derivation cache (attrs) is INSTANCE state that must never be written through to a shared
	// prototype: a proto-aliased mob writing l.attrs.values[name] would store into the prototype's
	// Living (and every sibling's, since they alias the same pointer). COW the Living before the cache
	// write so the memo lands on this instance only. The fork happens on the FIRST attr() of a spawned
	// mob (cheap pointer-identity check thereafter); the clone starts with a fresh empty/dirty cache, so
	// it recomputes its own values — never serving a sibling's memo. A player (prototype==nil) and an
	// already-COW'd mob fall through unchanged.
	l := mutableLiving(e)
	if l.attrs.dirty || l.attrs.values == nil {
		// Flush the whole cache on the first read after a dirty: clear it and recompute lazily.
		l.attrs.values = map[string]float64{}
		l.attrs.dirty = false
	}
	if v, ok := l.attrs.values[name]; ok {
		return v
	}
	v, err := resolveAttr(e, name, map[string]bool{})
	if err != nil {
		// A cycle or malformed formula that escaped the load-time lint: log + return 0 rather than
		// crashing the zone goroutine. Content-lint is the real gate; this is the defensive net.
		if e.zone != nil {
			e.zone.log.Debug("attr resolve error", "attr", name, "rid", e.rid, "err", err)
		}
		v = 0
	}
	l.attrs.values[name] = v
	return v
}

// resolveAttr computes attribute `name` WITHOUT touching the cache, with eval-time cycle detection
// via `visited` (the set of attrs on the current resolution stack). It does base -> flat mods ->
// multipliers -> clamp. A referenced attr (in a derived formula) recurses through resolveAttr so
// derived-of-derived and the visited-set cycle guard both work. Cached values are NOT consulted
// here (the top-level attr() owns the cache); within one resolution we recompute referenced attrs,
// which is fine — content formulas are shallow and the top-level call memoizes the final value.
func resolveAttr(e *Entity, name string, visited map[string]bool) (float64, error) {
	reg := e.zone.attrDefs()
	def := reg.get(name)
	if def == nil {
		// Unknown attribute: 0. Contentless entity / a ref to a non-existent attr resolves sanely.
		return 0, nil
	}
	if visited[name] {
		return 0, fmt.Errorf("attribute cycle through %q", name)
	}
	visited[name] = true
	defer delete(visited, name)

	// 1. base: a per-entity override (race/class/level/point-buy) replaces the def's default base.
	var base float64
	if ov, ok := e.living.attrBase[name]; ok {
		base = ov
	} else if def.base != nil {
		r := &formulaResolver{
			resolve: func(ref string, v map[string]bool) (float64, error) {
				return resolveAttr(e, ref, v)
			},
			visited: visited,
		}
		v, err := def.base.eval(r)
		if err != nil {
			return 0, err
		}
		base = v
	}

	// 2. + flat mods, then × multipliers, summed/multiplied across every modifier source.
	flat := 0.0
	mul := 1.0
	for _, src := range e.modSources() {
		flat += src.flatMod(name)
		mul *= src.mulMod(name)
	}
	val := (base + flat) * mul

	// 3. clamp to the attribute_def's declared range.
	if def.min != nil && val < *def.min {
		val = *def.min
	}
	if def.max != nil && val > *def.max {
		val = *def.max
	}
	return val, nil
}

// setAttrBase installs a per-entity base override for attribute `name` (the instance state that
// holds race/class/level/point-buy bases; for a player, prototype==nil so it lives here directly).
// It dirties the cache so the next attr() recomputes. Single-writer: zone goroutine.
func setAttrBase(e *Entity, name string, base float64) {
	l := mutableLiving(e) // COW: fork a proto-aliased mob's Living before mutating its attrBase map (else a base override leaks to the proto)
	if l == nil {
		return
	}
	if l.attrBase == nil {
		l.attrBase = map[string]float64{}
	}
	l.attrBase[name] = base
	markAttrsDirty(e)
}

// modSources returns every modifier source for entity e (gear + affects). The derivation sums
// flatMod / multiplies mulMod across the returned list (resolveAttr). Coarse this phase: gear is a
// stub (no Armor/affix component wired yet) and affects arrive in 5.2 — but the PLUMBING is live.
// 5.2's Affected runtime registers its modifier view via addModSource (and dirties the cache); a
// gear-change hook does the same. With no source registered the list is empty and derivation is
// base-only — the bare-engine behaviour.
func (e *Entity) modSources() []modSource {
	if e.living == nil {
		return nil
	}
	return e.living.modSrcs
}

// addModSource registers a modifier source on an entity and dirties its derivation cache so the new
// contribution lands on the next attr(). This is the seam the 5.2 Affected runtime (and a gear
// hook) feeds: register the source once, then dirty on every change. Single-writer: zone goroutine.
func addModSource(e *Entity, src modSource) {
	l := mutableLiving(e) // COW: fork a proto-aliased mob's Living before appending a mod source (else a gear/affect source leaks to the proto + siblings)
	if l == nil {
		return
	}
	l.modSrcs = append(l.modSrcs, src)
	markAttrsDirty(e)
}

// lintAttributeCycles validates that the attribute def graph has no self/mutual reference, so
// authored content can never ship a derived attribute whose formula (transitively) references
// itself. Run once at build time (defineGlobals) over the whole registry; a cycle is a content
// error logged loudly (the malformed def still loads, but attr() will defensively return 0 for it).
// Uses a 3-colour DFS over the static ref graph (each def's formula refs).
func lintAttributeCycles(defs map[string]*attributeDef) []error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(defs))
	var errs []error
	var visit func(ref string, stack []string)
	visit = func(ref string, stack []string) {
		color[ref] = gray
		stack = append(stack, ref)
		def := defs[ref]
		if def != nil && def.base != nil {
			refs := map[string]bool{}
			def.base.refs(refs)
			for next := range refs {
				if defs[next] == nil {
					continue // references a non-derived/absent attr: not a cycle edge
				}
				switch color[next] {
				case gray:
					errs = append(errs, fmt.Errorf("attribute cycle: %v -> %s", append(append([]string{}, stack...), next), next))
				case white:
					visit(next, stack)
				}
			}
		}
		color[ref] = black
	}
	for ref := range defs {
		if color[ref] == white {
			visit(ref, nil)
		}
	}
	return errs
}
