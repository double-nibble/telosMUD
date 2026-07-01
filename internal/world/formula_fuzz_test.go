package world

import (
	"encoding/json"
	"math"
	"testing"
)

// formula_fuzz_test.go fuzzes the content-facing derived-attribute formula evaluator (W6/W10 — the
// thinnest failure surface in formula.go: NaN/overflow/deep-nesting/clamp edges beyond the explicit
// div/mod-by-zero unit tests). A formula is builder-authored prefix-AST JSON parsed by parseFormula and
// evaluated per attr() call, so a pathological formula in a content pack must FAIL CLOSED (a clean
// parse/eval error), never panic the zone and never silently yield a NaN that then poisons every
// downstream derivation (combat math, clamps, resource maxes).
//
// Invariants for ANY json-decodable input (asserted against evalFinite, the guarded chokepoint EVERY
// top-level formula consumer goes through — check bonus, attribute base, grant base):
//   - parseFormula never panics — malformed structure returns an error;
//   - eval never panics — a bad op (div-by-zero, wrong arity, unknown head, cycle) returns an error;
//   - a SUCCESSFUL evalFinite is always FINITE — never NaN, never ±Inf. Both are unambiguously broken (NaN
//     compares false to everything and silently corrupts clamps/derivations; `int(+Inf)`=maxint64 injects a
//     maxint value into a game quantity), so evalFinite fails closed rather than returning either. (Raw
//     opNode.eval may still yield an intermediate ±Inf that a `min`/`clamp` tames back to finite — only the
//     FINAL consumed value is guarded, so a tamed-intermediate formula still succeeds.)
func FuzzFormulaEval(f *testing.F) {
	for _, s := range []string{
		`["+",1,2]`, `["-",5,3]`, `["*",2,3]`, `["/",10,2]`, `["/",1,0]`,
		`["mod",7,3]`, `["mod",5,0]`, `["min",1,2,3]`, `["max",1,2,3]`,
		`["clamp",5,1,10]`, `["clamp",5,10,1]`, // clamp with INVERTED bounds
		`["floor",2.7]`, `["ceil",2.1]`, `["round",2.5]`,
		`["if",1,10,20]`, `["if",0,10,20]`, // truthy / falsy condition
		`["attr","strength"]`, `["attr",""]`,
		`5`, `3.14`, `-2`,
		`["*",1e308,1e308]`,              // overflow -> +Inf
		`["-",1e308,["*",-1e308,1e308]]`, // Inf arithmetic (NaN candidate)
		`[[[[[[[["+",1,1]]]]]]]]`,        // deep nesting (parser/eval recursion)
		`[]`, `{}`, `"str"`, `null`, `true`,
		`["bogushead",1]`, `["+"]`, `["if",1]`, // malformed: unknown head / bad arity
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, src string) {
		var v any
		if err := json.Unmarshal([]byte(src), &v); err != nil {
			return // not JSON — parseFormula is never reached for non-JSON content in prod
		}
		node, err := parseFormula(v)
		if err != nil || node == nil {
			return // clean parse rejection — fail-closed, no panic
		}
		r := &formulaResolver{
			visited: map[string]bool{},
			resolve: func(ref string, _ map[string]bool) (float64, error) {
				// A finite, varied stand-in for any attr ref (the resolver never recurses here, so no
				// cycle arises from this side — parse/eval cycle guards are exercised structurally).
				return float64(len(ref)%7) + 1, nil
			},
		}
		got, err := evalFinite(node, r)
		if err != nil {
			return // clean eval error (incl. a non-finite result) — fail-closed
		}
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Fatalf("evalFinite(%q) returned a non-finite value (%v) with no error — it must fail closed, not poison derivation", src, got)
		}
	})
}
