package world

import (
	"fmt"
	"strings"
)

// check.go is the CHECK / SAVE / CONTESTED primitive (docs/PHASE6-PLAN.md §1.1, [G2]) — the single
// biggest new mechanism in Phase 6 and the home of "DM judgment -> builder content + engine roll."
// A `check` is a flow op (effect_op_handlers.go opCheck) STRUCTURALLY identical to if/chance: it
// resolves a content-defined roll against a threshold, classifies the result into one of N ORDERED
// outcome bands, and runs that band's nested op-list via runOps. Attack rolls, saving throws, skill
// checks, and contested rolls are all this ONE shape.
//
// The engine stays ignorant of dice SHAPE (dice.go rolls a content expression) AND outcome ARITY
// (the band list is the union abstraction — binary 5e is the 2-band case; PbtA 3-tier, BRP degrees,
// Blades pools all fit). USER-SETTLED (gap analysis §18.2/§18.4): the check op lives in the effect-op
// interpreter so an exit/object/affect-tick/ability can all invoke it; full ordered-band generality
// from day one.
//
// Scoping: a check's bonus/vs formulas read attributes through the effectCtx bindings — a bare attr
// ref resolves against $actor; an explicit `$actor.`/`$target.`/`$source.` prefix selects the other
// bound entity (so a saving throw reads `$target.dex_save` vs `$source.spell_dc`). Roll visibility is
// config (hidden by default — gap §18.1), opt-in show/summary, overridable per check.

// checkVisibility controls whether the roll math surfaces to the actor.
type checkVisibility int

const (
	visInherit checkVisibility = iota // no explicit setting -> the engine default (hide)
	visHide                           // emit nothing; the band's own ops narrate
	visShow                           // emit the full roll math ("rolled 14 + 6 = 20 vs 15 — success")
	visSummary                        // emit only the band label ("(success)")
)

// checkBand is one ordered outcome band: a test on the rolled total, its margin over the DC, and/or
// the natural faces, plus the op-list to run when it matches. Bands are tested top-down; the FIRST
// match wins, so the last band is conventionally a no-test default. A nil bound is unbounded on that
// side. Every NUMERIC bound is a FORMULA (not a literal) evaluated at resolve time against the same
// $actor/$target/$source scope as bonus/vs — so a band edge can be a DERIVED value (WoW's crit/miss
// boundaries, BRP's dc/5 and dc/20 sub-thresholds), not just a fixed number. The four axes:
//
//   - min / max          test the TOTAL (roll + bonus): roll-HIGH thresholds (PbtA 10+/7-9) AND, for
//     roll-UNDER systems, success = `max: <skill%>` (total <= skill) with the d100 as the bare roll.
//   - marginMin/marginMax test the MARGIN (total − dc): dc-relative windows (Fate shifts, save-by-N,
//     contested ties = {marginMin:0, marginMax:0}).
//   - faceEq / faceCount  test the natural FACES: "at least faceCount dice show exactly faceEq" — the
//     ONLY way to author nat-20 auto-crit / nat-1 auto-miss (independent of total) and Blades' 6-6.
//
// Examples (top-down; first match wins):
//
//	5e attack   {faceEq:20 -> crit} , {faceEq:1 -> miss} , {marginMin:0 -> hit} , {default -> miss}
//	half-on-save {marginMin:0 -> half} , {default -> full}
//	PbtA 3-tier {min:10 -> strong} , {min:7,max:9 -> weak} , {default -> miss}
//	BRP degrees {max:["/",dc,20] -> crit} , {max:["/",dc,5] -> special} , {max:dc -> success} , {default -> fumble}
//	Blades      {faceEq:6,faceCount:2 -> crit} , {min:1 -> success} , {default -> failure}  (count-pool total)
type checkBand struct {
	min       formulaNode // total >= min     (nil: no lower bound)
	max       formulaNode // total <= max     (nil: no upper bound)
	marginMin formulaNode // margin >= marginMin   (nil: not margin-floor-tested)
	marginMax formulaNode // margin <= marginMax   (nil: not margin-ceiling-tested)
	faceEq    *float64    // optional: count natural faces equal to this value (ALL rolled faces, see note)
	faceCount int         // ... require at least this many such faces (defaults to 1 when faceEq set)
	label     string
	ops       []effectOp
}

// matches reports whether the classified result falls in this band. eval resolves a band-edge formula
// to its value (it carries the check's $actor/$target/$source scope); faces are the natural dice.
func (b *checkBand) matches(total, margin float64, faces []int, eval func(formulaNode) float64) bool {
	if b.min != nil && total < eval(b.min) {
		return false
	}
	if b.max != nil && total > eval(b.max) {
		return false
	}
	if b.marginMin != nil && margin < eval(b.marginMin) {
		return false
	}
	if b.marginMax != nil && margin > eval(b.marginMax) {
		return false
	}
	if b.faceEq != nil {
		// faceEq counts ALL rolled faces, not just the KEPT ones. For advantage (2d20kh1) this is
		// correct for nat-20 crit (5e crits on a 20 on EITHER die). It diverges for a nat-1 auto-MISS
		// band under advantage (a single 1 on the discarded die would match though you shouldn't auto-
		// miss) — a narrow combo; a faceKeptOnly flag is the v2 fix if it ever bites. Builders pairing
		// faceEq-miss with advantage should know this.
		n := 0
		for _, f := range faces {
			if float64(f) == *b.faceEq {
				n++
			}
		}
		need := b.faceCount
		if need < 1 {
			need = 1
		}
		if n < need {
			return false
		}
	}
	return true
}

// checkVs is the threshold a check resolves against: either a DC formula, or a CONTESTED defender
// check (the defender rolls their own spec; the resulting total becomes the DC).
type checkVs struct {
	dc        formulaNode // a literal/formula DC; nil if contested
	contested *checkSpec  // the defender's own spec (only its dice+bonus are used); nil if a DC
}

// checkSpec is a parsed, immutable check (build-time). Shareable across zone goroutines; the per-roll
// randomness comes from the effectCtx rng at resolve time.
type checkSpec struct {
	dice       diceSpec
	bonus      formulaNode // over the actor/target/source attrs; nil => +0
	vs         checkVs
	bands      []checkBand
	visibility checkVisibility
	label      string // optional, for emission/logging ("Climb", "Dexterity save")
}

// checkResult is the outcome of resolving a check (returned to opCheck + emission).
type checkResult struct {
	roll      int     // the dice magnitude (sum, kept-sum, Fudge sum, or pool success count)
	faces     []int   // the natural faces rolled (for emission / future nat-crit bands)
	bonus     float64 // the evaluated bonus
	total     float64 // roll + bonus
	dc        float64 // the threshold (DC value, or the contested defender's total)
	margin    float64 // total − dc
	contested bool
	band      *checkBand // the matched band (nil only if bands is empty)
	bandLabel string
}

// resolveCheck rolls the spec, evaluates bonus/vs against the ctx bindings, classifies into the first
// matching band, fires the reserved OnCheck event, and emits per the visibility config. It does NOT
// run the band's ops — opCheck does that, keeping the classifier free of the runOps recursion. Single-
// writer: zone goroutine. Deterministic under a seeded ctx rng.
func resolveCheck(c *effectCtx, spec *checkSpec) checkResult {
	roll, faces := rollDiceSpec(c, spec.dice)
	bonus := evalCheckFormula(c, spec.bonus, c.actor)
	total := float64(roll) + bonus

	res := checkResult{roll: roll, faces: faces, bonus: bonus, total: total}

	switch {
	case spec.vs.contested != nil:
		// The defender rolls their OWN spec; only its dice+bonus matter (its bands/ops are ignored).
		// The defender's bonus defaults its BARE refs to $target (the defender) — so a contested
		// grapple writes the defender's stat as a plain `["attr","athletics"]` and reads the defender,
		// not the attacker. $actor/$source remain available for an explicit cross-reference.
		res.contested = true
		dRoll, _ := rollDiceSpec(c, spec.vs.contested.dice)
		res.dc = float64(dRoll) + evalCheckFormula(c, spec.vs.contested.bonus, c.target)
	case spec.vs.dc != nil:
		res.dc = evalCheckFormula(c, spec.vs.dc, c.actor)
	default:
		res.dc = 0 // a pure-total check (e.g. PbtA): bands test the total directly, margin == total
	}
	res.margin = res.total - res.dc

	// Band edges are formulas scoped like bonus/vs (default $actor), evaluated lazily per band.
	evalEdge := func(n formulaNode) float64 { return evalCheckFormula(c, n, c.actor) }
	for i := range spec.bands {
		if spec.bands[i].matches(res.total, res.margin, res.faces, evalEdge) {
			res.band = &spec.bands[i]
			res.bandLabel = spec.bands[i].label
			break
		}
	}

	fireCheckEvent(c, res)
	emitCheck(c, spec, res)
	return res
}

// evalCheckFormula evaluates a check bonus/vs formula with $actor/$target/$source scope dispatch. A
// bare attr ref resolves against `def` (the default scope — the actor for a bonus/DC); a `$scope.`
// prefix selects the bound entity. Each ref pulls the entity's FULLY-DERIVED attr() value (so gear/
// affect modifiers flow in); attr() owns its own cache + cycle guard, so this resolver needs neither.
// A nil node is +0. A malformed formula logs + yields 0 (content-lint is the real gate).
func evalCheckFormula(c *effectCtx, node formulaNode, def *Entity) float64 {
	if node == nil {
		return 0
	}
	r := &formulaResolver{
		visited: map[string]bool{},
		resolve: func(ref string, _ map[string]bool) (float64, error) {
			// $swing.* is a CTX scalar, not an entity attribute ([G-H]) — intercept it before the attr
			// lookup so a to-hit/damage bonus can read the per-swing index (PF iterative -5/-10).
			if v, ok := resolveSwingRef(c, ref); ok {
				return v, nil
			}
			ent, bare := resolveCheckScope(c, ref, def)
			return attr(ent, bare), nil
		},
	}
	v, err := node.eval(r)
	if err != nil {
		if c.z != nil {
			c.z.log.Debug("check formula error", "err", err)
		}
		return 0
	}
	return v
}

// resolveCheckScope maps a (possibly scoped) attr ref to (entity, bareName). "$target.dex_save" ->
// (c.target, "dex_save"); "athletics" -> (def, "athletics"). An unknown scope falls back to def.
func resolveCheckScope(c *effectCtx, ref string, def *Entity) (*Entity, string) {
	if !strings.HasPrefix(ref, "$") {
		return def, ref
	}
	rest := ref[1:]
	scope, name := rest, ""
	if dot := strings.IndexByte(rest, '.'); dot >= 0 {
		scope, name = rest[:dot], rest[dot+1:]
	}
	switch scope {
	case "actor":
		return c.actor, name
	case "target":
		return c.target, name
	case "source":
		return c.source, name
	default:
		return def, name
	}
}

// resolveSwingRef resolves a `$swing.<field>` ctx-scalar ref ([G-H]) to its value. The only field this
// slice exposes is `index` — the 0-based swing index within the current combat round (combat.go sets
// effectCtx.swingIndex per swing) — so PF iterative attacks (`-5*$swing.index`) are authorable. Returns
// (value, true) when `ref` is a recognized `$swing.` ref; (0, false) otherwise (the caller falls back
// to the attr/entity-scope resolver). An unknown `$swing.` field yields (0, true) — a clean 0, not an
// attr miss — so a typo doesn't silently read an entity attribute.
func resolveSwingRef(c *effectCtx, ref string) (float64, bool) {
	if !strings.HasPrefix(ref, "$swing.") {
		return 0, false
	}
	switch ref[len("$swing."):] {
	case "index":
		return float64(c.swingIndex), true
	default:
		return 0, true
	}
}

// resolveVisibility resolves the effective visibility for a check: an explicit per-check setting wins;
// otherwise the engine default is HIDE (USER-SETTLED gap §18.1 — hidden by default). The per-ability
// and per-pack override layers are a reserved seam (a later wire-up threads them above the default).
func resolveVisibility(spec *checkSpec) checkVisibility {
	if spec.visibility != visInherit {
		return spec.visibility
	}
	return visHide
}

// emitCheck surfaces the roll to the actor per the resolved visibility. hide => nothing (the band's
// ops narrate). show => the full math; summary => just the band label. Phase 6 emits via the actor's
// own stream (send); the GMCP structured emit is a reserved Phase-9 hook.
func emitCheck(c *effectCtx, spec *checkSpec, res checkResult) {
	vis := resolveVisibility(spec)
	if vis == visHide {
		return
	}
	s, ok := sessionOf(c.actor)
	if !ok {
		return
	}
	label := spec.label
	if label == "" {
		label = "Check"
	}
	if vis == visSummary {
		s.send(textFrame(fmt.Sprintf("[%s] (%s)", label, res.bandLabel)))
		return
	}
	// visShow — full math, with a contested/pool variant.
	switch {
	case res.contested:
		s.send(textFrame(fmt.Sprintf("[%s] %d%+d = %d vs %d (contested) — %s",
			label, res.roll, int(res.bonus), int(res.total), int(res.dc), res.bandLabel)))
	case spec.dice.kind == dicePool:
		s.send(textFrame(fmt.Sprintf("[%s] %d successes — %s", label, res.roll, res.bandLabel)))
	case spec.vs.dc == nil && !res.contested:
		s.send(textFrame(fmt.Sprintf("[%s] %d%+d = %d — %s",
			label, res.roll, int(res.bonus), int(res.total), res.bandLabel)))
	default:
		s.send(textFrame(fmt.Sprintf("[%s] %d%+d = %d vs %d — %s",
			label, res.roll, int(res.bonus), int(res.total), int(res.dc), res.bandLabel)))
	}
}

// fireCheckEvent fires the OnCheck in-zone event ([G2e]/[G3]) for content to react to a resolved check
// (a rage build on a successful save, a proc on a skill use). The subject is the checker (c.actor); the
// counterpart (c.target — the save's caster, the contested foe) rides as the event `other` so a handler
// can `target: other`. Synchronous, single-writer, depth-guarded (event.go). Slice 6.2 made this a real
// dispatch (was reserved-log in 6.1).
func fireCheckEvent(c *effectCtx, _ checkResult) {
	if c.z == nil {
		return
	}
	c.z.fireEvent(c, evOnCheck, c.actor, c.target, 1)
}
