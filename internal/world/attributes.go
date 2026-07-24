package world

import (
	"fmt"
	"math"
	"sort"
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
	// degraded records which attributes the finiteness/magnitude screen had to rewrite (attrScreen).
	// It is cleared with `values`, so it never outlives the derivation it describes. Formulas refuse a
	// degraded attribute (evalCheckFormulaErr) so the FORMULA path stays fail-closed, while direct
	// readers still get a bounded number — see attrScreen for why those two want opposite things.
	degraded map[string]bool
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
		// Flush the whole cache on the first read after a dirty: clear it and recompute lazily. The
		// degraded set is cleared with it so a screen never outlives the derivation that produced it.
		l.attrs.values = map[string]float64{}
		l.attrs.degraded = nil
		l.attrs.dirty = false
	}
	if v, ok := l.attrs.values[name]; ok {
		return v
	}
	v, degraded, err := resolveAttr(e, name, map[string]bool{})
	if err != nil {
		// A cycle or malformed formula that escaped the load-time lint: log + return 0 rather than
		// crashing the zone goroutine. Content-lint is the real gate; this is the defensive net.
		if e.zone != nil {
			e.zone.log.Debug("attr resolve error", "attr", name, "rid", e.rid, "err", err)
		}
		v, degraded = 0, false
	}
	l.attrs.values[name] = v
	if degraded {
		if l.attrs.degraded == nil {
			l.attrs.degraded = map[string]bool{}
		}
		l.attrs.degraded[name] = true
		// WARN, not Debug: this is a content defect an operator has to be able to find, and a saturated
		// value is otherwise indistinguishable from a big number someone meant to author. The rate is
		// bounded by cache invalidation (once per attribute per dirty cycle), NOT by read volume —
		// attr() memoizes, so a hot loop reading a degraded attribute logs once.
		if e.zone != nil {
			e.zone.log.Warn("attribute modifier fold produced an out-of-range value; screened",
				"attr", name, "rid", e.rid, "screened_to", v, "ceiling", attrFoldCeiling)
		}
	}
	return v
}

// resolveAttr computes attribute `name` WITHOUT touching the cache, with eval-time cycle detection
// via `visited` (the set of attrs on the current resolution stack). It does base -> flat mods ->
// multipliers -> clamp. A referenced attr (in a derived formula) recurses through resolveAttr so
// derived-of-derived and the visited-set cycle guard both work. Cached values are NOT consulted
// here (the top-level attr() owns the cache); within one resolution we recompute referenced attrs,
// which is fine — content formulas are shallow and the top-level call memoizes the final value.
func resolveAttr(e *Entity, name string, visited map[string]bool) (float64, bool, error) {
	reg := e.zone.attrDefs()
	def := reg.get(name)
	if def == nil {
		// Unknown attribute: 0. Contentless entity / a ref to a non-existent attr resolves sanely.
		return 0, false, nil
	}
	if visited[name] {
		return 0, false, fmt.Errorf("attribute cycle through %q", name)
	}
	visited[name] = true
	defer delete(visited, name)

	// degraded accumulates across this whole resolution: an attribute DERIVED from a screened one is
	// itself untrustworthy, even though its own arithmetic stayed in range. Propagating keeps the
	// formula-side refusal honest — otherwise `armour = soak * 1` would launder a screened `soak` into
	// a clean value, which is the same laundering the boon/bane channel had to close.
	degraded := false

	// 1. base: a per-entity override (race/class/level/point-buy) replaces the def's default base.
	var base float64
	if ov, ok := e.living.attrBase[name]; ok {
		base = ov
	} else if def.base != nil {
		r := &formulaResolver{
			resolve: func(ref string, v map[string]bool) (float64, error) {
				rv, rd, err := resolveAttr(e, ref, v)
				if rd {
					degraded = true
				}
				return rv, err
			},
			visited: visited,
		}
		// The base formula is evaluated with PLAIN eval, not evalFinite, then SCREENED — deliberately.
		// evalFinite would return an error on a non-finite RESULT and resolveAttr would resolve to 0,
		// which is the very undying/immunity outcome this file exists to prevent: 0 on max_hp means
		// resourceMax <= 0. A base that overflows to ±Inf (`1000/defense` with defense 0 taming to Inf,
		// or `1e300*1e300`) must instead be bounded and marked degraded, exactly like the fold. A genuine
		// error (a cycle, a division by zero) is still an error and still resolves to 0 — eval returns
		// those as errors, not as non-finite values, so this only reinterprets the non-finite-RESULT case.
		v, err := def.base.eval(r)
		if err != nil {
			return 0, false, err
		}
		var baseScreened bool
		base, baseScreened = attrScreen(v, 0)
		if baseScreened {
			degraded = true
		}
	}

	// 2. + flat mods, then × multipliers, summed/multiplied across every modifier source.
	flat := 0.0
	mul := 1.0
	for _, src := range e.modSources() {
		flat += src.flatMod(name)
		mul *= src.mulMod(name)
	}
	// The NaN/Inf screen runs BEFORE the declared clamp so the substituted value is then re-clamped
	// into the author's own range — a `max: 100` still wins over the fallback. It must also precede the
	// clamp for a second reason: a declared range does NOT contain a NaN, because every comparison
	// against NaN is false, so `val < *def.min` and `val > *def.max` are both false and a NaN would
	// pass through a [1,5] range untouched.
	val, screened := attrScreen((base+flat)*mul, base)
	if screened {
		degraded = true
	}

	// 3. clamp to the attribute_def's declared range.
	if def.min != nil && val < *def.min {
		val = *def.min
	}
	if def.max != nil && val > *def.max {
		val = *def.max
	}

	// 4. The magnitude BACKSTOP, deliberately after the declared clamp: an author declaring
	// `max: 1e300` must not be able to reintroduce an unbounded value. This is what makes "every
	// attribute read is within ±attrFoldCeiling" a real postcondition rather than an aspiration, and
	// it is what the induction over derived-of-derived rests on.
	if bounded, cut := attrScreen(val, 0); cut {
		val, degraded = bounded, true
	}
	return val, degraded, nil
}

// attrFoldCeiling is the magnitude bound every derived attribute is held within. A trillion is far
// above any stat a ruleset plausibly wants (this engine's own maxKillMagnitude is 1e5, a raid boss's
// max_hp is ~1e7) and far below 2^63, so `int(attr(...))` is provably safe for any value that passes
// this bound.
//
// KNOWN COST, stated rather than discovered later: it is a BACKSTOP applied after the attribute_def's
// own min/max, so it overrides a declared max larger than itself. A pack wanting a genuine
// above-a-trillion accumulator (an idle/incremental XP total) is clamped. That is the deliberate trade
// — an unbounded accumulator and an overflow are indistinguishable to the engine, and the overflow is
// the one with security consequences.
const attrFoldCeiling = 1e12

// attrScreen bounds a derived attribute and reports whether it had to. `fallback` is the value to use
// when the fold is NaN — the pre-modifier base, i.e. "the modifiers composed to nonsense, so ignore
// them" — and is itself screened, since a non-finite base would otherwise walk straight back out.
//
// # Why a MAGNITUDE bound and not just a finiteness screen
//
// A finiteness-only screen bounds nothing: `1e300` is finite, so it passes, and `int(1e300)` still
// wraps. One affect with an ordinary large modifier is enough — no overflow to infinity required. The
// magnitude bound is also what makes the guarantee INDUCTIVE across derived-of-derived: an attribute
// B defined as `A * A` evaluates to 1e24 from a bounded A, and then B's own screen pulls it back to
// the ceiling, so EVERY attribute is within ±ceiling at every level of derivation rather than only at
// the leaves.
//
// # Why screening and erroring are both wrong on their own
//
// This is the part two independent reviews disagreed about, and both were right about their own half.
//
// Erroring (the cycle path's behaviour: resolve to 0) is NOT safe here. Zero on `max_hp` means
// `resourceMax <= 0`, which is the natural-immunity discard in dealDamage AND falsifies the death
// predicate — so failing an attribute closed lands on exactly the security-critical path the bug
// itself reaches. A bounded number is strictly better for a direct reader.
//
// Saturating silently is ALSO not safe, in the other direction. Before any screen existed, an
// overflowed attribute was ±Inf, and evalFinite rejected it at every formula, so the formula path
// failed CLOSED for free: a `deal_damage` bonus reading a poisoned attribute contributed 0. Replacing
// the infinity with a legitimate-looking 1e12 deletes that marker and hands every formula a usable
// one-shot value — measured at 1e12 damage where the same fixture previously dealt its base amount.
//
// So the screen does both: it returns a BOUNDED value for direct readers, and it records that the
// value is DEGRADED so formulas can keep refusing it. The marker is what preserves the fail-closed
// property that finiteness used to provide implicitly.
func attrScreen(v, fallback float64) (float64, bool) {
	if math.IsNaN(v) {
		if !isFiniteBounded(fallback) {
			return 0, true
		}
		return fallback, true
	}
	// ±Inf and any over-ceiling magnitude collapse to the same clause: `Abs(±Inf) > ceiling` is true,
	// so infinity needs no branch of its own. Copysign carries the direction, so a poisoned soak clamps
	// toward +ceiling and a poisoned penalty toward −ceiling.
	if math.Abs(v) > attrFoldCeiling {
		return math.Copysign(attrFoldCeiling, v), true
	}
	return v, false
}

// isFiniteBounded reports whether v is a value the screen would pass unchanged.
func isFiniteBounded(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= attrFoldCeiling
}

// attrIsDegraded reports whether attribute `name` on e had to be screened. It resolves the attribute
// first (through the memoizing accessor) so the flag is always populated before it is read.
func attrIsDegraded(e *Entity, name string) bool {
	if e == nil || e.living == nil {
		return false
	}
	attr(e, name)
	l := e.living
	return l.attrs.degraded != nil && l.attrs.degraded[name]
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
// flatMod / multiplies mulMod across the returned list (resolveAttr). Two sources register here: the 5.2
// Affected runtime (its affect-summed modifier view) and, since #35, the Wearer (its worn-gear affix sum) —
// each via addModSource, each dirtying the cache on change. With no source registered the list is empty and
// derivation is base-only — the bare-engine behaviour.
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
	var errs []error
	for _, cycle := range attributeCycles(defs) {
		errs = append(errs, fmt.Errorf("attribute cycle: %v", cycle))
	}
	return errs
}

// attributeCycles returns each attribute-derivation cycle as its ordered NODE LIST (the DFS stack up to and
// including the back-edge target). It is the structured form lintAttributeCycles renders to strings: the
// full-graph reload validator (#205) needs the node set to attribute a cycle to the reloaded packs (reject a
// cycle only when an in-scope attribute participates), while boot just logs the message. Same DFS, one source.
func attributeCycles(defs map[string]*attributeDef) [][]string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(defs))
	var cycles [][]string
	var visit func(ref string, stack []string)
	visit = func(ref string, stack []string) {
		color[ref] = gray
		stack = append(stack, ref)
		def := defs[ref]
		if def != nil && def.base != nil {
			refs := map[string]bool{}
			def.base.refs(refs)
			// Deterministic edge order (map iteration is random) so the reported cycle set is stable.
			nexts := make([]string, 0, len(refs))
			for next := range refs {
				nexts = append(nexts, next)
			}
			sort.Strings(nexts)
			for _, next := range nexts {
				if defs[next] == nil {
					continue // references a non-derived/absent attr: not a cycle edge
				}
				switch color[next] {
				case gray:
					cycles = append(cycles, append(append([]string{}, stack...), next))
				case white:
					visit(next, stack)
				}
			}
		}
		color[ref] = black
	}
	roots := make([]string, 0, len(defs))
	for ref := range defs {
		roots = append(roots, ref)
	}
	sort.Strings(roots)
	for _, ref := range roots {
		if color[ref] == white {
			visit(ref, nil)
		}
	}
	return cycles
}
