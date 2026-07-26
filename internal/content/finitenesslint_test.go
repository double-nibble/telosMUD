package content

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// finitenesslint_test.go pins that a non-finite numeric literal in an attribute-feeding field is
// caught. The headline is that YAML `.inf`/`.nan` decode into a float64 with no error — that is what
// makes the lint necessary rather than academic.

func TestYAMLDecodesFloatSpecialsSilently(t *testing.T) {
	// This is the reachability the lint exists for: the schema's own decoder accepts these.
	for _, tc := range []struct {
		lit  string
		want func(float64) bool
	}{
		{".inf", func(v float64) bool { return math.IsInf(v, 1) }},
		{"-.inf", func(v float64) bool { return math.IsInf(v, -1) }},
		{".nan", func(v float64) bool { return math.IsNaN(v) }},
	} {
		var m AffectModifierDTO
		err := yaml.Unmarshal([]byte("attr: x\nop: add\nvalue: "+tc.lit+"\n"), &m)
		require.NoError(t, err, "YAML %q decodes without error — that is the poison path", tc.lit)
		require.True(t, tc.want(m.Value), "value for %q", tc.lit)
	}
	// A control: an over-range DECIMAL is rejected by the decoder, so the lint is specifically about the
	// float specials, not about magnitude.
	var m AffectModifierDTO
	require.Error(t, yaml.Unmarshal([]byte("attr: x\nop: add\nvalue: 1e400\n"), &m))
}

func TestLintFinite(t *testing.T) {
	inf := math.Inf(1)
	nan := math.NaN()
	ok := 3.0

	packs := []Pack{{
		Pack: "bad",
		Attributes: []AttributeDTO{
			{Ref: "a_inf_max", Max: &inf},
			{Ref: "a_nan_min", Min: &nan},
			{Ref: "a_lit", DefaultBase: BaseSpecDTO{Lit: &inf}},
			{Ref: "a_ok", Min: &ok, Max: &ok, DefaultBase: BaseSpecDTO{Lit: &ok}},
		},
		Affects: []AffectDTO{
			{Ref: "hex", Body: AffectBodyDTO{Modifiers: []AffectModifierDTO{
				{Attr: "x", Op: "add", Value: 1},
				{Attr: "y", Op: "mul", Value: nan},
			}}},
			{Ref: "clean", Body: AffectBodyDTO{Modifiers: []AffectModifierDTO{{Attr: "z", Op: "add", Value: 2}}}},
		},
	}}

	got := LintFinite(packs)

	// A finding per non-finite literal, and NONE for the finite ones.
	byField := map[string]FinitenessViolation{}
	for _, v := range got {
		byField[v.Field] = v
	}
	require.Len(t, got, 4, "exactly the four non-finite literals: %+v", got)
	require.Equal(t, "Inf", byField["attribute a_inf_max.max"].Kind)
	require.Equal(t, "NaN", byField["attribute a_nan_min.min"].Kind)
	require.Equal(t, "Inf", byField["attribute a_lit.default_base.lit"].Kind)

	mod := byField["affect hex modifier[1].value"]
	require.Equal(t, "NaN", mod.Kind)
	require.Equal(t, "hex", mod.Ref, "the finding must name the owning def for the operator")
	require.Equal(t, "bad", mod.Pack)

	// Non-vacuity: a clean pack produces nothing.
	require.Empty(t, LintFinite([]Pack{{Pack: "good", Attributes: []AttributeDTO{{Ref: "a", Min: &ok}}}}))
}

// TestLintFiniteWearableModifiers (#514): a non-finite value on a static WORN modifier is caught, since it
// feeds the same attribute stack as an affect modifier. Item AND mob prototypes are walked.
func TestLintFiniteWearableModifiers(t *testing.T) {
	inf := math.Inf(1)
	nan := math.NaN()

	packs := []Pack{{
		Pack: "bad",
		Zones: []ZoneDTO{{
			Ref: "z1",
			Items: []ProtoDTO{
				{Ref: "z1:obj:cursed-ring", Wearable: &WearableDTO{Modifiers: []AffectModifierDTO{
					{Attr: "str", Op: "add", Value: 1},
					{Attr: "dex", Op: "mul", Value: inf},
				}}},
				{Ref: "z1:obj:clean-ring", Wearable: &WearableDTO{Modifiers: []AffectModifierDTO{{Attr: "str", Op: "add", Value: 2}}}},
			},
			Mobs: []ProtoDTO{
				{Ref: "z1:mob:hexed", Wearable: &WearableDTO{Modifiers: []AffectModifierDTO{{Attr: "con", Op: "add", Value: nan}}}},
			},
		}},
	}}

	got := LintFinite(packs)
	byField := map[string]FinitenessViolation{}
	for _, v := range got {
		byField[v.Field] = v
	}
	require.Len(t, got, 2, "exactly the two non-finite worn modifiers: %+v", got)
	require.Equal(t, "Inf", byField["wearable z1:obj:cursed-ring modifier[1].value"].Kind)
	require.Equal(t, "z1:obj:cursed-ring", byField["wearable z1:obj:cursed-ring modifier[1].value"].Ref)
	require.Equal(t, "NaN", byField["wearable z1:mob:hexed modifier[0].value"].Kind)
}

// TestLintFiniteEndToEndFromYAML is the wiring proof: a real YAML modifier with `.nan` reaches the
// lint through the actual DTO decode, not a hand-built struct.
func TestLintFiniteEndToEndFromYAML(t *testing.T) {
	var af AffectDTO
	require.NoError(t, yaml.Unmarshal([]byte(
		"ref: cursed\nbody:\n  modifiers:\n    - {attr: autofail_save, op: add, value: .nan}\n",
	), &af))

	got := LintFinite([]Pack{{Pack: "p", Affects: []AffectDTO{af}}})
	require.Len(t, got, 1)
	require.Equal(t, "NaN", got[0].Kind)
	require.Equal(t, "cursed", got[0].Ref)
}

// TestLintFiniteWalksBaseFormula pins that a non-finite literal nested in a base FORMULA (not just the
// literal-base form) is caught — the gap the security review flagged.
func TestLintFiniteWalksBaseFormula(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	packs := []Pack{{
		Pack: "bad",
		Attributes: []AttributeDTO{
			{Ref: "a_expr_nan", DefaultBase: BaseSpecDTO{Expr: []any{"+", nan, float64(5)}}},
			{Ref: "a_expr_deep", DefaultBase: BaseSpecDTO{Expr: []any{"*", float64(2), []any{"-", inf, float64(1)}}}},
			{Ref: "a_expr_ok", DefaultBase: BaseSpecDTO{Expr: []any{"+", float64(1), float64(2)}}},
		},
	}}
	got := LintFinite(packs)
	byRef := map[string]int{}
	for _, v := range got {
		byRef[v.Ref]++
	}
	require.Equal(t, 1, byRef["a_expr_nan"], "a NaN literal in a formula is caught")
	require.Equal(t, 1, byRef["a_expr_deep"], "an Inf nested deeper in the tree is caught")
	require.Zero(t, byRef["a_expr_ok"], "an all-finite formula is clean")
}
