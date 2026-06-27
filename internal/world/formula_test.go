package world

import (
	"testing"
)

// formula_test.go exercises the prefix-AST evaluator (formula.go) in isolation: parse + eval of the
// arithmetic/min/max/clamp heads, derived-of-derived through an attr resolver, and a detected cycle.

// evalStandalone parses v and evaluates it with a resolver that looks attrs up in the given table.
func evalStandalone(t *testing.T, v any, attrs map[string]float64) (float64, error) {
	t.Helper()
	node, err := parseFormula(v)
	if err != nil {
		t.Fatalf("parseFormula(%v): %v", v, err)
	}
	r := &formulaResolver{
		visited: map[string]bool{},
		resolve: func(ref string, _ map[string]bool) (float64, error) {
			return attrs[ref], nil
		},
	}
	return node.eval(r)
}

func TestFormulaArithmetic(t *testing.T) {
	cases := []struct {
		name string
		ast  any
		want float64
	}{
		{"lit-bare", 5.0, 5},
		{"lit-node", []any{"lit", 7.0}, 7},
		{"add", []any{"+", 2.0, 3.0, 4.0}, 9},
		{"sub", []any{"-", 10.0, 3.0, 2.0}, 5},
		{"unary-neg", []any{"-", 4.0}, -4},
		{"mul", []any{"*", 2.0, 3.0, 4.0}, 24},
		{"div", []any{"/", 20.0, 2.0, 2.0}, 5},
		{"min", []any{"min", 5.0, 2.0, 8.0}, 2},
		{"max", []any{"max", 5.0, 2.0, 8.0}, 8},
		{"clamp-lo", []any{"clamp", -3.0, 0.0, 10.0}, 0},
		{"clamp-hi", []any{"clamp", 99.0, 0.0, 10.0}, 10},
		{"clamp-mid", []any{"clamp", 5.0, 0.0, 10.0}, 5},
		// con*10 + level*5 with con=10, level=2 = 110
		{"nested", []any{
			"+",
			[]any{"*", []any{"attr", "con"}, 10.0},
			[]any{"*", []any{"attr", "level"}, 5.0},
		}, 110},
	}
	attrs := map[string]float64{"con": 10, "level": 2}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := evalStandalone(t, c.ast, attrs)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestFormulaDivByZero(t *testing.T) {
	if _, err := evalStandalone(t, []any{"/", 1.0, 0.0}, nil); err == nil {
		t.Fatal("expected division-by-zero error")
	}
}

// TestFormulaPhase6Heads covers the [G1] additions: floor/ceil/round/mod and the short-circuiting
// conditional. The canonical use is an exact ability modifier: floor((score − 10) / 2).
func TestFormulaPhase6Heads(t *testing.T) {
	attrs := map[string]float64{"score": 15, "zero": 0}
	cases := []struct {
		name string
		ast  any
		want float64
	}{
		{"floor", []any{"floor", 2.9}, 2},
		{"ceil", []any{"ceil", 2.1}, 3},
		{"round", []any{"round", 2.5}, 3},
		{"mod", []any{"mod", 17.0, 5.0}, 2},
		// 5e ability modifier: floor((15 − 10) / 2) = 2.
		{"abil-mod", []any{"floor", []any{"/", []any{"-", []any{"attr", "score"}, 10.0}, 2.0}}, 2},
		// if: cond != 0 -> then.
		{"if-true", []any{"if", 1.0, 7.0, 9.0}, 7},
		{"if-false", []any{"if", 0.0, 7.0, 9.0}, 9},
		// if SHORT-CIRCUITS: the untaken branch's div-by-zero never runs.
		{"if-short-circuit", []any{"if", 1.0, 42.0, []any{"/", 1.0, []any{"attr", "zero"}}}, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := evalStandalone(t, c.ast, attrs)
			if err != nil {
				t.Fatalf("eval: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestFormulaModByZero(t *testing.T) {
	if _, err := evalStandalone(t, []any{"mod", 5.0, 0.0}, nil); err == nil {
		t.Fatal("expected mod-by-zero error")
	}
}

func TestFormulaUnknownHead(t *testing.T) {
	if _, err := parseFormula([]any{"pow", 2.0, 3.0}); err == nil {
		t.Fatal("expected unknown-head parse error")
	}
}

// TestLintAttributeCyclesDetects builds a small derived-attr graph with a mutual reference and
// asserts the load-time lint flags it (and that an acyclic graph passes).
func TestLintAttributeCyclesDetects(t *testing.T) {
	mk := func(ref string, expr any) *attributeDef {
		node, err := parseFormula(expr)
		if err != nil {
			t.Fatalf("parse %s: %v", ref, err)
		}
		return &attributeDef{ref: ref, base: node}
	}
	// a -> b -> a (mutual cycle).
	cyclic := map[string]*attributeDef{
		"a": mk("a", []any{"attr", "b"}),
		"b": mk("b", []any{"attr", "a"}),
	}
	if errs := lintAttributeCycles(cyclic); len(errs) == 0 {
		t.Fatal("expected a cycle to be detected")
	}
	// max_hp = con*10 + level*5; con/level are literal-base (no edges) — acyclic.
	acyclic := map[string]*attributeDef{
		"con":   {ref: "con", base: litNode{v: 10}},
		"level": {ref: "level", base: litNode{v: 1}},
		"max_hp": mk("max_hp", []any{
			"+",
			[]any{"*", []any{"attr", "con"}, 10.0},
			[]any{"*", []any{"attr", "level"}, 5.0},
		}),
	}
	if errs := lintAttributeCycles(acyclic); len(errs) != 0 {
		t.Fatalf("acyclic graph flagged: %v", errs)
	}
}
