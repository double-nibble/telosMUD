package world

import (
	"fmt"
	"math"
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
//   - faceEq / faceCount  test the natural KEPT FACES: "at least faceCount of the dice that COUNTED
//     show exactly faceEq" — the ONLY way to author nat-20 auto-crit / nat-1 auto-miss (independent of
//     total) and Blades' 6-6.
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
	faceEq    *float64    // optional: count natural KEPT faces equal to this value (see matches)
	faceCount int         // ... require at least this many such faces (defaults to 1 when faceEq set)
	// when is a STATE predicate (#513): a scoped truthy formula, ANDed with the axes above like any
	// other of them. It is what lets a band be selected by the state of an entity rather than by the
	// roll — the fifth axis, and the only one that does not read the dice at all. See matches.
	when  formulaNode
	label string
	ops   []effectOp
}

// matches reports whether the classified result falls in this band. eval resolves a band-edge formula
// to its value (it carries the check's $actor/$target/$source scope); kept are the natural faces that
// CONTRIBUTED to the magnitude (see rollDiceSpec — identical to every rolled face for any spec that
// discards nothing, which is every kind but keepHigh/keepLow).
func (b *checkBand) matches(total, margin float64, kept []int, eval func(formulaNode) float64) bool {
	// The STATE axis (#513) — the only axis that ignores the roll entirely. It is ANDed with the others
	// exactly like they are ANDed with each other; `when` does not override anything, which is what
	// keeps this a band predicate rather than a new engine concept.
	//
	// It is tested LAST, after the free faceEq scan and the numeric bounds. Every axis is
	// side-effect-free so the order is behaviourally unobservable, but `when` is the only axis that is
	// always a formula evaluation (faceEq is a plain slice scan, and a numeric bound is often absent),
	// so a band like {face_eq: 20, when: ...} should not pay the eval on every roll when the face test
	// would have rejected it anyway.
	//
	// A band carrying ONLY `when` and no dice test is how content expresses an outcome the roll cannot
	// change. ORDERING IS THE WHOLE OF IT, and it is not simply "put it at the top": bands are
	// first-match-wins, so a forced band must sit BELOW anything that must outrank it and above
	// everything it outranks. In 5e a natural 1 misses even against a paralyzed target, so the auto-crit
	// goes UNDER the nat-1 band:
	//
	//	{face_eq: 1, label: miss}                                          # a nat-1 still misses
	//	{when: ["attr", "$target.helpless"], margin_min: 0, label: crit}   # auto-crit on a hit that LANDS
	//	{face_eq: 20, label: crit}                                         # the natural crit, still there
	//	{margin_min: 0, label: hit}
	//	{label: miss}
	//
	// (The margin_min on the forced band is load-bearing too — without it, auto-crit would promote a
	// MISS to a crit.)
	//
	// TRUTHINESS is "strictly positive". It deliberately matches the boon/bane channel's `> 0` rather
	// than using `!= 0`, because the formula vocabulary has no negation: "A unless B" is naturally
	// written `A - B`, and under a non-zero rule that fires again once B exceeds A — reintroducing the
	// very affine-overshoot-on-the-second-stack defect this primitive exists to eliminate (see
	// TestSentinelEncodingBreaksOnStacking).
	//
	// Non-finite reads as false, and this is REACHABLE rather than the dead belt-and-braces it looks
	// like. evalFinite rejects a non-finite FINAL result, so a bare `["attr", x]` on a poisoned
	// attribute already fails closed — but it checks only the top level, so any wrapper LAUNDERS the
	// poison into a finite value: `min($target.helpless, 1)` on an +Inf attribute evaluates to a clean
	// 1. That is the natural way an author normalizes a stacking flag for a boolean predicate, and it
	// would convert an overflowed counter into a guaranteed forced outcome. Screening here catches the
	// bare case; the laundered case is only fully closed by bounding attr() itself.
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
		// faceEq counts the KEPT faces — the dice that actually contributed to the magnitude. For every
		// spec that discards nothing (sum / Fudge / pool) kept IS every rolled face, so this is the
		// identity there and no existing content shifts. It matters only for keepHigh/keepLow, where
		// counting the DISCARDED die is simply wrong: a `{face_eq: 1 -> miss}` band on a 2d20kh1 fired on
		// 9.36% of rolls (measured) because the thrown-away die showed a 1, while the die the check
		// actually used was the higher one. Kept-only reads 0.25% — "the roll was a 1" — and is equally
		// correct in the other direction (a nat-20 crit band under DISadvantage must require BOTH dice to
		// show 20, which all-faces got wrong at 9.75% vs the correct 0.25%).
		//
		// This is load-bearing now rather than cosmetic: the boon/bane channel (below) makes a keep spec
		// something a TRANSIENT condition selects, so a pack authoring the ordinary nat-1/nat-20 bands hits
		// the keep path constantly instead of never.
		n := 0
		for _, f := range kept {
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
	if b.when != nil && !truthyPredicate(eval(b.when)) {
		return false
	}
	return true
}

// A poisoned attribute can no longer reach truthyPredicate as a non-finite value: the fold is bounded
// (attributes.go), and a screened attribute makes its whole check formula error, which evalCheckFormula
// collapses to 0 — so a `when` reading a degraded attribute yields 0 and this predicate is false. The
// `!math.IsInf` clause below is therefore belt-and-braces for a hypothetical non-attribute Inf; the
// live protection is the degraded-refusal in evalCheckFormulaErr.
//
// truthyPredicate is the engine's one definition of "this content predicate holds": STRICTLY POSITIVE
// and finite.
//
// Strictly positive, not merely non-zero, and this matters more than it looks. The formula vocabulary
// has no `not`/`==`/`>=`, so an author writing "A unless B" reaches for `A - B`. Under a non-zero rule
// that expression is true at -1 as well as at 1, so a SECOND source of B flips the predicate back on —
// exactly the affine-overshoot defect that makes sentinel band edges unusable and that this primitive
// was added to replace. Positive-only also agrees with the boon/bane channel's `> 0` in the same file,
// so content has one truthiness rule rather than two.
//
// NaN needs no clause of its own: every comparison against NaN is false, so `v > 0` already rejects
// it. (That is exactly why "> 0" is safer than "!= 0" here — under a non-zero rule NaN would read as
// TRUE.) +Inf is the only non-finite value `v > 0` would accept, so it is the only one rejected
// explicitly; -Inf falls out with NaN.
func truthyPredicate(v float64) bool { return v > 0 && !math.IsInf(v, 1) }

// checkSubject selects who rolls a check (its narration + OnCheck-event identity + bare-ref default
// scope). See checkSpec.subject.
type checkSubject int

const (
	subjActor  checkSubject = iota // the ctx actor rolls (default): a skill use, a to-hit
	subjTarget                     // the ctx target rolls: the saving-throw idiom
)

// checkVs is the threshold a check resolves against: either a DC formula, or a CONTESTED defender
// check (the defender rolls their own spec; the resulting total becomes the DC).
type checkVs struct {
	dc        formulaNode // a literal/formula DC; nil if contested
	contested *checkSpec  // the defender's own spec (only its dice+bonus are used); nil if a DC
}

// checkSpec is a parsed, immutable check (build-time). Shareable across zone goroutines; the per-roll
// randomness comes from the effectCtx rng at resolve time.
//
// # The boon/bane channel (#511) — how a TRANSIENT condition changes a pending roll
//
// `dice` is the neutral roll. `boon`/`bane` are scoped formulas (the same $actor/$target/$source
// vocabulary as `bonus` and the band edges) counting how many boon and bane influences bear on this
// roll. PRESENCE on each side — not the difference between them — selects which of three
// content-authored dice expressions is actually rolled:
//
//	boon > 0 AND bane > 0  -> the neutral `dice` (they cancel)
//	boon > 0 alone         -> boonDice
//	bane > 0 alone         -> baneDice
//	neither, or the selected alternative is unauthored -> the neutral `dice`
//
// The engine deliberately does NOT synthesize the alternative die, and this is the crux of the design
// rather than an implementation shortcut. "Roll it twice and keep the better one" presumes the engine
// knows which direction is better, and it does not: this repo's own demo pack authors a ROLL-UNDER
// avoidance ladder (1d100, `max: $actor.dodge` succeeds), where keeping the HIGHER die makes you worse
// at dodging. Synthesizing keep-highest for "advantage" would have hardcoded d20 roll-high semantics —
// 5e vocabulary — into the engine and silently inverted that ladder. It also cannot express a boon in
// systems whose better-roll is not a keep at all (a Blades pool's boon is an EXTRA POOL DIE: 2d6>=4 ->
// 3d6>=4). So content names all three expressions and the engine only ever SELECTS among them.
//
// ANY-VERSUS-ANY CANCELLATION, deliberately, and NOT a net difference. This is 5e's actual rule: one
// source of advantage against one of disadvantage gives a straight roll, and so do FIVE sources against
// one. A net-difference rule (evaluate boon - bane, select on the sign) reads as equivalent and is not
// — it resolves 5-vs-1 as advantage. Counting influences rather than summing them is also what keeps
// the channel honest as a union abstraction: a system that wants magnitude to matter already has
// `bonus`, which is the additive channel and composes normally.
//
// POLARITY AND THE HARM GATE — read this before authoring a bane. The PvP apply-gate derives whether an
// affect is harmful from the SIGN of its modifiers (affectIsDetrimental, defs.go): a negative additive
// is harm, a positive additive is a buff. That inference assumes higher-is-better, which is exactly
// what a bane counter inverts — so an affect that adds +3 to a bane attribute is a real debuff the
// sign heuristic alone would read as benign. attributeInvertedPolarity (defs.go) closes this by
// DERIVING the inverted set from the channel's own formulas rather than asking content to declare it,
// so an author cannot forget; see that function for the derivation rule and its limits.
//
// Because boon/bane are ordinary attribute formulas they compose through EVERY modifier source the
// entity already has — affects, gear affixes, racial base values, derived attributes — with no new
// per-entity state, no new DTO field, and therefore no store round-trip exposure. An affect grants a
// boon by adding to an ordinary content-named attribute (`modifiers: [{attr: atk_boon, op: add,
// value: 1}]`); nothing about the affect runtime changed to support this.
type checkSpec struct {
	dice       diceSpec
	bonus      formulaNode // over the actor/target/source attrs; nil => +0
	vs         checkVs
	bands      []checkBand
	visibility checkVisibility
	label      string // optional, for emission/logging ("Climb", "Dexterity save")

	// subject names WHO ROLLS this check — the "checker". It governs three things that were previously
	// hardwired to the ctx actor: (1) the DEFAULT scope of bare attribute refs in dice/bonus/vs/bands/
	// boon/bane (a bare `dex_save` reads the roller); (2) who the resolved roll is NARRATED to
	// (emitCheck sends to the roller's own stream); (3) the subject of the OnCheck event (fireCheckEvent),
	// with the OTHER side riding as the event `other`. Default subjActor = the ctx actor rolls (a skill
	// use, a to-hit, an attacker's check) — unchanged, existing behaviour. subjTarget = the ctx TARGET
	// rolls: this is the SAVING-THROW idiom (a caster forces the victim to make a Dex save), where the
	// SAVER must be narrated to and must be the OnCheck subject so content can author "on a successful
	// save, the saver gains X" — otherwise unauthorable, since the roll would fire on the caster. Explicit
	// `$actor`/`$target`/`$source` refs still bind to the FIXED ctx entities regardless of subject; only
	// the bare-ref default and the narration/event identity follow the roller.
	subject checkSubject

	// boon/bane count the influences on each side (see the type comment: presence cancels, it does not
	// sum). nil => 0 on that side. boonDice/baneDice are the content-authored alternatives; nil => that
	// direction has no alternative expression and the neutral `dice` is rolled even when it is selected.
	boon     formulaNode
	bane     formulaNode
	boonDice *diceSpec
	baneDice *diceSpec
}

// effectiveDice applies the boon/bane channel and returns the dice expression to actually roll. `def`
// is the default scope for a BARE attr ref, matching whatever the caller uses for `bonus` on the same
// spec (the actor for a top-level check, the defender for a contested sub-spec) so the two never
// disagree about what an unprefixed name means.
//
// FAST PATH / IDENTITY: a spec with no boon and no bane formula returns spec.dice untouched without
// evaluating anything. Every check authored before #511 takes this path, so the channel cannot perturb
// existing content — and the selection is idempotent by construction, since it only ever hands back one
// of three expressions content wrote itself and never derives a fourth from them.
//
// A BROKEN CHANNEL IS NO CHANNEL. If EITHER side fails to evaluate — a malformed formula, a division
// by zero, an attribute driven to ±Inf/NaN by a modifier fold — the whole channel degrades to the
// neutral die rather than to "that side counted zero". The distinction matters: a lone errored boon
// collapsing to 0 would leave a live bane unopposed, so an authoring bug in the BOON half would
// silently hand the roller the WORSE die, and a poisoned attribute would become a way to pin an
// opponent's roll deterministically. Failing the whole channel neutral is the only direction where a
// broken input cannot be turned into an advantage by whoever broke it.
func effectiveDice(c *effectCtx, spec *checkSpec, def *Entity) diceSpec {
	if spec.boon == nil && spec.bane == nil {
		return spec.dice
	}
	boonV, boonOK := evalCheckCount(c, spec.boon, def)
	baneV, baneOK := evalCheckCount(c, spec.bane, def)
	if !boonOK || !baneOK {
		return spec.dice
	}
	hasBoon, hasBane := boonV > 0, baneV > 0
	switch {
	case hasBoon && hasBane:
		return spec.dice // any-versus-any: they cancel to the straight roll
	case hasBoon && spec.boonDice != nil:
		return *spec.boonDice
	case hasBane && spec.baneDice != nil:
		return *spec.baneDice
	default:
		return spec.dice
	}
}

// evalCheckCount evaluates one side of the channel and reports whether the value is USABLE. It differs
// from evalCheckFormula in refusing to launder a failure into a number: a nil node is a legitimate 0
// (that side simply is not authored), but an evaluation error or a non-finite result yields ok=false so
// the caller can fail the whole channel neutral. NaN is rejected by IsNaN rather than by comparison,
// since every comparison against NaN is false and would otherwise read as "absent".
func evalCheckCount(c *effectCtx, node formulaNode, def *Entity) (float64, bool) {
	if node == nil {
		return 0, true
	}
	v, err := evalCheckFormulaErr(c, node, def)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// checkResult is the outcome of resolving a check (returned to opCheck + emission).
type checkResult struct {
	roll      int      // the dice magnitude (sum, kept-sum, Fudge sum, or pool success count)
	faces     []int    // EVERY natural face rolled (for emission/logging — a player wants to see both dice)
	kept      []int    // the faces that CONTRIBUTED to roll; what the nat-face bands test (see rollDiceSpec)
	dice      diceSpec // the expression actually rolled — spec.dice, or a boon/bane alternative (#511)
	bonus     float64  // the evaluated bonus
	total     float64  // roll + bonus
	dc        float64  // the threshold (DC value, or the contested defender's total)
	margin    float64  // total − dc
	contested bool
	band      *checkBand // the matched band (nil only if bands is empty)
	bandLabel string
}

// resolveCheck rolls the spec, evaluates bonus/vs against the ctx bindings, classifies into the first
// matching band, fires the reserved OnCheck event, and emits per the visibility config. It does NOT
// run the band's ops — opCheck does that, keeping the classifier free of the runOps recursion. Single-
// writer: zone goroutine. Deterministic under a seeded ctx rng.
func resolveCheck(c *effectCtx, spec *checkSpec) checkResult {
	// WHO ROLLS (#535/#511 follow-up): the subject selector picks the roller. Default = the ctx actor;
	// subject: target = the ctx target rolls (the saving-throw idiom). The roller is the DEFAULT scope for
	// bare refs and the narration + OnCheck-event identity; the other side is the counterpart. Explicit
	// $actor/$target/$source refs are unaffected (they bind the fixed ctx entities).
	roller, counterpart := c.actor, c.target
	if spec.subject == subjTarget {
		roller, counterpart = c.target, c.actor
	}
	// The boon/bane channel (#511) picks WHICH content-authored expression this roll uses, before any
	// dice are thrown. Bare refs in boon/bane default to the roller, matching `bonus` on the same spec.
	dice := effectiveDice(c, spec, roller)
	roll, faces, kept := rollDiceSpec(c, dice)
	bonus := evalCheckFormula(c, spec.bonus, roller)
	total := float64(roll) + bonus

	res := checkResult{roll: roll, faces: faces, kept: kept, dice: dice, bonus: bonus, total: total}

	switch {
	case spec.vs.contested != nil:
		// The defender rolls their OWN spec; only its dice+bonus matter (its bands/ops are ignored).
		// The defender's bonus defaults its BARE refs to $target (the defender) — so a contested
		// grapple writes the defender's stat as a plain `["attr","athletics"]` and reads the defender,
		// not the attacker. $actor/$source remain available for an explicit cross-reference.
		res.contested = true
		// The defender's own spec gets its own boon/bane selection, scoped like its bonus ($target by
		// default) — otherwise a contested check would let only the attacker's conditions matter, and a
		// prone/restrained defender would contest a grapple at full strength.
		dRoll, _, _ := rollDiceSpec(c, effectiveDice(c, spec.vs.contested, c.target))
		res.dc = float64(dRoll) + evalCheckFormula(c, spec.vs.contested.bonus, c.target)
	case spec.vs.dc != nil:
		res.dc = evalCheckFormula(c, spec.vs.dc, roller)
	default:
		res.dc = 0 // a pure-total check (e.g. PbtA): bands test the total directly, margin == total
	}
	// Transient to-hit AC bump (7.9 Shield reaction): a defender's reaction may raise its effective AC
	// for the triggering swing only (combat.go sets c.reactACBonus from the to-hit reaction's "ac"
	// delta). The to-hit DC IS the defender's AC, so adding it here raises the threshold the attacker's
	// roll must beat — BEFORE the bands match, so hit/miss re-classifies correctly. 0 (the default) for
	// every other check, so this is inert outside the swing's to-hit path. The bump lives only on this
	// per-swing ctx; it never writes the defender's stored AC.
	res.dc += c.reactACBonus
	res.margin = res.total - res.dc

	// Band edges are formulas scoped like bonus/vs (default = the roller), evaluated lazily per band.
	evalEdge := func(n formulaNode) float64 { return evalCheckFormula(c, n, roller) }
	for i := range spec.bands {
		if spec.bands[i].matches(res.total, res.margin, res.kept, evalEdge) {
			res.band = &spec.bands[i]
			res.bandLabel = spec.bands[i].label
			break
		}
	}

	fireCheckEvent(c, res, roller, counterpart)
	emitCheck(spec, res, roller)
	return res
}

// evalCheckFormula evaluates a check bonus/vs formula with $actor/$target/$source scope dispatch. A
// bare attr ref resolves against `def` (the default scope — the actor for a bonus/DC); a `$scope.`
// prefix selects the bound entity. Each ref pulls the entity's FULLY-DERIVED attr() value (so gear/
// affect modifiers flow in); attr() owns its own cache + cycle guard, so this resolver needs neither.
// A nil node is +0. A malformed formula logs + yields 0 (content-lint is the real gate).
func evalCheckFormula(c *effectCtx, node formulaNode, def *Entity) float64 {
	v, err := evalCheckFormulaErr(c, node, def)
	if err != nil {
		return 0
	}
	return v
}

// evalCheckFormulaErr is evalCheckFormula with the error SURFACED instead of collapsed to 0. Most
// callers (bonus, vs, band edges) genuinely want the lenient 0 — a broken edge should not abort a
// resolution mid-flight, and content-lint is the real gate there. The boon/bane channel does not: for
// it, "this failed" and "this counted zero" have opposite consequences (see effectiveDice), so it needs
// to tell them apart. Same resolver, same scoping, one behaviour difference.
func evalCheckFormulaErr(c *effectCtx, node formulaNode, def *Entity) (float64, error) {
	if node == nil {
		return 0, nil
	}
	r := &formulaResolver{
		visited: map[string]bool{},
		resolve: func(ref string, _ map[string]bool) (float64, error) {
			// $swing.* is a CTX scalar, not an entity attribute ([G-H]) — intercept it before the attr
			// lookup so a to-hit/damage bonus can read the per-swing index (PF iterative -5/-10).
			if v, ok := resolveSwingRef(c, ref); ok {
				return v, nil
			}
			// $depletion.* is the same shape (#407): the arithmetic of the depletion this ctx was built for,
			// so an on_depleted hook can deal the OVERFLOW into another pool.
			if v, ok := resolveDepletionRef(c, ref); ok {
				return v, nil
			}
			ent, bare := resolveCheckScope(c, ref, def)
			// A DEGRADED attribute fails the whole formula (attributes.go attrScreen). Before the
			// modifier fold was bounded, an overflowed attribute was ±Inf and evalFinite rejected it
			// here for free — so this path failed CLOSED, and a deal_damage bonus reading a poisoned
			// attribute contributed 0. Bounding the fold replaced the infinity with a legitimate-looking
			// number, which would have handed every formula a usable one-shot value (measured: 1e12
			// damage where the same fixture previously dealt its base amount). Refusing the degraded
			// marker restores that property explicitly instead of relying on an accident of IEEE-754.
			if attrIsDegraded(ent, bare) {
				return 0, fmt.Errorf("attribute %q is degraded (screened by the fold bound)", bare)
			}
			return attr(ent, bare), nil
		},
	}
	v, err := evalFinite(node, r)
	if err != nil {
		if c.z != nil {
			c.z.log.Debug("check formula error", "err", err)
		}
		return 0, err
	}
	return v, nil
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

// resolveDepletionRef resolves a `$depletion.<field>` ctx-scalar ref (#407) to its value — the sibling of
// resolveSwingRef, deliberately the same shape so content has ONE concept for "a number the engine put on
// this resolution" rather than two. The fields describe the blow that emptied the pool this hook is running
// for:
//
//	overflow — how far PAST 0 the blow drove the pool (the excess it could not absorb). The carry-over
//	           amount a two-track system deals into its lethal pool.
//	applied  — how much the pool actually ABSORBED ("you lost N sanity").
//	amount   — the whole mitigated blow. applied + overflow == amount.
//
// Returns (value, true) for a recognized `$depletion.` ref; (0, false) otherwise, so the caller falls back
// to the attr/entity-scope resolver. An UNKNOWN `$depletion.` field yields (0, true) — a clean 0, not an
// attr miss — so a typo can never silently read an entity attribute of that name. Outside a depletion ctx
// every field is 0 (the zero value), which makes a stray reference inert rather than an error.
func resolveDepletionRef(c *effectCtx, ref string) (float64, bool) {
	if !strings.HasPrefix(ref, "$depletion.") {
		return 0, false
	}
	switch ref[len("$depletion."):] {
	case "overflow":
		return float64(c.depletion.overflow), true
	case "applied":
		return float64(c.depletion.applied), true
	case "amount":
		return float64(c.depletion.amount), true
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
func emitCheck(spec *checkSpec, res checkResult, roller *Entity) {
	s, ok := sessionOf(roller)
	if !ok {
		return // only a player ROLLER emits to a stream (a save narrates to the saver, not the caster)
	}
	vis := resolveVisibility(spec)
	// Staff `rolls on` (#30) upgrades a check hidden ONLY by the engine default (visInherit) to full math
	// for the roller; an explicit content visHide is respected (content intent), so only the default flips.
	if vis == visHide && spec.visibility == visInherit && s.showRolls {
		vis = visShow
	}
	if vis == visHide {
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
	// The EFFECTIVE die's kind, not the spec's: a boon/bane alternative may be a different kind than the
	// neutral expression (a pool system's boon is an extra pool die), and the emission must describe the
	// roll the player actually made.
	case res.dice.kind == dicePool:
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
// (a rage build on a successful save, a proc on a skill use). The subject is the ROLLER (the checker —
// the ctx actor by default, or the ctx target for a `subject: target` save); the counterpart (the
// save's caster, the contested foe) rides as the event `other` so a handler can `target: other`. This
// is why the save idiom needs the subject selector: "on a successful Con save, the saver gains X" must
// fire OnCheck ON THE SAVER, not the caster. Synchronous, single-writer, depth-guarded (event.go).
func fireCheckEvent(c *effectCtx, _ checkResult, roller, counterpart *Entity) {
	if c.z == nil {
		return
	}
	c.z.fireEvent(c, evOnCheck, roller, counterpart, 1)
}
