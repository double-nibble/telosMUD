package world

// predicates_test.go exercises the richer `if` predicate vocabulary (#542): comparison operators
// (>=,<=,>,<,==,!=) over a numeric LHS (a pool current OR a formula — an attribute / a ctx scalar like
// $depletion.overflow) against a FORMULA RHS (a derived threshold like max_hp/2). This is what lets
// "bloodied" (hp <= half of max) and "instant death on overflow" be authored declaratively.

import (
	"math/rand"
	"testing"
)

// ranThen runs opIf and reports whether the THEN branch fired, observed via a `marker` resource the
// then-branch bumps to 1 (the else-branch leaves it 0). The subject is the ctx target.
func ranThen(t *testing.T, z *Zone, subject *Entity, op *effectOp) bool {
	t.Helper()
	setResourceCurrent(subject, "marker", 0)
	op.then = []effectOp{{kind: "modify_resource", resource: "marker", amount: 1, tgt: "self"}}
	c := &effectCtx{z: z, actor: subject, source: subject, target: subject, mag: 1, rng: rand.New(rand.NewSource(1))}
	if err := opIf(c, op); err != nil {
		t.Fatalf("opIf: %v", err)
	}
	return resourceCurrent(subject, "marker") == 1
}

func predicateZone(t *testing.T) (*Zone, *session) {
	z, s := combatZone(t)
	z.defs.res.register("marker", &resourceDef{ref: "marker"}) // uncapped counter for observing the branch
	z.defs.attr.register("max_reactions", &attributeDef{ref: "max_reactions", base: litNode{v: 3}})
	z.defs.res.register("reactions", &resourceDef{ref: "reactions", maxAttr: "max_reactions", perRound: true})
	return z, s
}

// TestIfDerivedThresholdBloodied proves "hp <= half of max_hp" (a DERIVED threshold with the <= operator)
// — the 5e "bloodied" predicate, previously unauthorable declaratively.
func TestIfDerivedThresholdBloodied(t *testing.T) {
	z, s := predicateZone(t)
	hero := s.entity
	half := opFormula("/", an("max_hp"), litNode{v: 2}) // max_hp / 2 = 50

	// hp 40 <= 50 -> bloodied (then fires).
	setResourceCurrent(hero, "hp", 40)
	op := &effectOp{kind: "if", ifResource: "hp", ifCmp: "<=", ifThreshold: half}
	if !ranThen(t, z, hero, op) {
		t.Fatal("hp 40 <= max_hp/2 (50) should be bloodied (then), but else fired")
	}
	// hp 60 <= 50 is false -> not bloodied (else).
	setResourceCurrent(hero, "hp", 60)
	op2 := &effectOp{kind: "if", ifResource: "hp", ifCmp: "<=", ifThreshold: half}
	if ranThen(t, z, hero, op2) {
		t.Fatal("hp 60 <= max_hp/2 (50) should be false (else), but then fired")
	}
	// Exact boundary: hp 50 <= 50 -> true (inclusive).
	setResourceCurrent(hero, "hp", 50)
	op3 := &effectOp{kind: "if", ifResource: "hp", ifCmp: "<=", ifThreshold: half}
	if !ranThen(t, z, hero, op3) {
		t.Fatal("hp 50 <= max_hp/2 (50) should be true (inclusive <=)")
	}
}

// TestIfFormulaLHSOverflow proves a FORMULA left-hand side reading a ctx scalar: "$depletion.overflow
// >= max_hp" — the "instant death on massive overflow" predicate an on_depleted hook uses.
func TestIfFormulaLHSOverflow(t *testing.T) {
	z, s := predicateZone(t)
	hero := s.entity // max_hp 100
	op := &effectOp{
		kind:        "if",
		ifValue:     an("$depletion.overflow"), // LHS = the depletion ctx scalar
		ifCmp:       ">=",
		ifThreshold: an("max_hp"), // RHS = max_hp
	}
	// overflow 120 >= 100 -> instant death (then).
	c := &effectCtx{z: z, actor: hero, source: hero, target: hero, mag: 1, rng: rand.New(rand.NewSource(1))}
	c.depletion.overflow = 120
	setResourceCurrent(hero, "marker", 0)
	op.then = []effectOp{{kind: "modify_resource", resource: "marker", amount: 1, tgt: "self"}}
	if err := opIf(c, op); err != nil {
		t.Fatalf("opIf: %v", err)
	}
	if resourceCurrent(hero, "marker") != 1 {
		t.Fatal("overflow 120 >= max_hp 100 should fire then (instant death), but else fired")
	}
	// overflow 80 >= 100 -> false (else).
	c2 := &effectCtx{z: z, actor: hero, source: hero, target: hero, mag: 1, rng: rand.New(rand.NewSource(1))}
	c2.depletion.overflow = 80
	setResourceCurrent(hero, "marker", 0)
	if err := opIf(c2, op); err != nil {
		t.Fatalf("opIf: %v", err)
	}
	if resourceCurrent(hero, "marker") != 0 {
		t.Fatal("overflow 80 >= max_hp 100 should be false (else), but then fired")
	}
}

// TestIfComparators exercises each operator against a literal threshold.
func TestIfComparators(t *testing.T) {
	z, s := predicateZone(t)
	hero := s.entity
	setResourceCurrent(hero, "hp", 50)
	cases := []struct {
		cmp  string
		rhs  float64
		want bool // for hp == 50
	}{
		{">=", 50, true},
		{">=", 51, false},
		{"<=", 50, true},
		{"<=", 49, false},
		{">", 49, true},
		{">", 50, false},
		{"<", 51, true},
		{"<", 50, false},
		{"==", 50, true},
		{"==", 51, false},
		{"!=", 51, true},
		{"!=", 50, false},
	}
	for _, tc := range cases {
		op := &effectOp{kind: "if", ifResource: "hp", ifCmp: tc.cmp, ifThreshold: litNode{v: tc.rhs}}
		if got := ranThen(t, z, hero, op); got != tc.want {
			t.Errorf("hp 50 %s %v: then fired=%v, want %v", tc.cmp, tc.rhs, got, tc.want)
		}
	}
}

// TestIfLegacyReactionBudgetUnchanged proves the pre-#542 `if reactions >= min` (default cmp, literal
// RHS via ifResourceMin, no formula) is byte-for-byte unchanged.
func TestIfLegacyReactionBudgetUnchanged(t *testing.T) {
	z, s := predicateZone(t)
	hero := s.entity
	setResourceCurrent(hero, "reactions", 1)
	// Legacy shape: no ifCmp (defaults >=), no ifThreshold (uses ifResourceMin literal).
	op := &effectOp{kind: "if", ifResource: "reactions", ifResourceMin: 1}
	if !ranThen(t, z, hero, op) {
		t.Fatal("reactions 1 >= 1 should fire then (legacy reaction-budget guard)")
	}
	setResourceCurrent(hero, "reactions", 0)
	op2 := &effectOp{kind: "if", ifResource: "reactions", ifResourceMin: 1}
	if ranThen(t, z, hero, op2) {
		t.Fatal("reactions 0 >= 1 should be false (else)")
	}
}

// TestIfUnknownCmpFallsBackToGTE proves a typo/empty comparator defaults to ">=" (never silently
// inverts), matching the legacy behaviour.
func TestIfUnknownCmpFallsBackToGTE(t *testing.T) {
	z, s := predicateZone(t)
	hero := s.entity
	setResourceCurrent(hero, "hp", 50)
	op := &effectOp{kind: "if", ifResource: "hp", ifCmp: "≥typo", ifThreshold: litNode{v: 40}}
	if !ranThen(t, z, hero, op) {
		t.Fatal("unknown cmp should fall back to >= : 50 >= 40 is true")
	}
}

// TestIfBrokenOperandFailsToFalse proves the #542-review fix: a degraded/broken formula operand makes
// the comparison INDETERMINATE, and the predicate fails to FALSE (skip then) rather than collapsing the
// operand to 0. The headline hazard: instant-death `$depletion.overflow >= max_hp` with a broken RHS
// (here a division-by-zero standing in for a degraded max_hp) would, under a 0-collapse, become
// `overflow >= 0` = always true and fire the HARMFUL branch on a debuffed victim.
func TestIfBrokenOperandFailsToFalse(t *testing.T) {
	z, s := predicateZone(t)
	hero := s.entity
	op := &effectOp{
		kind:        "if",
		ifValue:     an("$depletion.overflow"),
		ifCmp:       ">=",
		ifThreshold: opFormula("/", litNode{v: 1}, litNode{v: 0}), // errors -> indeterminate
	}
	c := &effectCtx{z: z, actor: hero, source: hero, target: hero, mag: 1, rng: rand.New(rand.NewSource(1))}
	c.depletion.overflow = 120
	setResourceCurrent(hero, "marker", 0)
	op.then = []effectOp{{kind: "modify_resource", resource: "marker", amount: 1, tgt: "self"}}
	if err := opIf(c, op); err != nil {
		t.Fatalf("opIf: %v", err)
	}
	if resourceCurrent(hero, "marker") != 0 {
		t.Fatal("a broken threshold must fail the predicate to false, not fire the harmful branch (0-collapse hazard)")
	}
}

// TestIfParseRejectsUnknownComparator proves an unknown comparator is rejected at parse (never silently
// coerced to >=, which could invert a fire-when-low predicate).
func TestIfParseRejectsUnknownComparator(t *testing.T) {
	if _, err := parseOp(map[string]any{"op": "if", "resource": "hp", "cmp": "=<", "threshold": 10.0}); err == nil {
		t.Fatal("expected a parse error for an unknown comparator")
	}
}

// TestIfParseRejectsPredicateWithoutLHS proves a comparator/threshold with no left-hand side is rejected
// (it would otherwise silently branch else).
func TestIfParseRejectsPredicateWithoutLHS(t *testing.T) {
	if _, err := parseOp(map[string]any{"op": "if", "cmp": "<=", "threshold": 10.0}); err == nil {
		t.Fatal("expected a parse error for a predicate with no LHS")
	}
}

// TestIfEqualityRelativeEpsilon proves == uses a RELATIVE epsilon: at 1e12 scale a 1e-3 difference is
// float noise and reads as equal, where an absolute 1e-9 epsilon would (wrongly) read them as unequal.
func TestIfEqualityRelativeEpsilon(t *testing.T) {
	if !compareIf(1e12, "==", 1e12+1e-3) {
		t.Fatal("== should treat near-equal large values as equal (relative epsilon)")
	}
	if compareIf(1e12, "==", 2e12) {
		t.Fatal("== should not treat clearly different large values as equal")
	}
	if !compareIf(50, "==", 50) || compareIf(50, "==", 51) {
		t.Fatal("== must still be exact at hp scale")
	}
}

// TestIfPredicateParsesFromContent proves the content syntax wires end-to-end through parseOp: a nested
// resource_min block with op+value, and the flat cmp/lhs/threshold keys.
func TestIfPredicateParsesFromContent(t *testing.T) {
	// Nested resource_min block form: {resource: hp, op: "<=", value: [/, [attr, max_hp], 2]}.
	op, err := parseOp(map[string]any{
		"op": "if",
		"resource_min": map[string]any{
			"resource": "hp",
			"op":       "<=",
			"value":    []any{"/", []any{"attr", "max_hp"}, 2.0},
		},
		"then": []any{map[string]any{"op": "modify_resource", "resource": "marker", "amount": 1.0, "target": "self"}},
	})
	if err != nil {
		t.Fatalf("parseOp (nested): %v", err)
	}
	if op.ifResource != "hp" || op.ifCmp != "<=" || op.ifThreshold == nil {
		t.Fatalf("nested if did not parse: resource=%q cmp=%q threshold=%v", op.ifResource, op.ifCmp, op.ifThreshold)
	}

	// Flat form: cmp + lhs (formula) + threshold (formula).
	op2, err := parseOp(map[string]any{
		"op":        "if",
		"cmp":       ">=",
		"lhs":       []any{"attr", "$depletion.overflow"},
		"threshold": []any{"attr", "max_hp"},
		"then":      []any{},
	})
	if err != nil {
		t.Fatalf("parseOp (flat): %v", err)
	}
	if op2.ifCmp != ">=" || op2.ifValue == nil || op2.ifThreshold == nil {
		t.Fatalf("flat if did not parse: cmp=%q lhs=%v threshold=%v", op2.ifCmp, op2.ifValue, op2.ifThreshold)
	}
}
