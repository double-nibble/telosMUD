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

// TestLintFiniteEndToEndFromYAML is the wiring proof: a real YAML modifier with `.nan` reaches the
// lint through the actual DTO decode, not a hand-built struct.
func TestLintFiniteEndToEndFromYAML(t *testing.T) {
	var af AffectDTO
	require.NoError(t, yaml.Unmarshal([]byte(
		"ref: cursed\nbody:\n  modifiers:\n    - {attr: autofail_save, op: add, value: .nan}\n"), &af))

	got := LintFinite([]Pack{{Pack: "p", Affects: []AffectDTO{af}}})
	require.Len(t, got, 1)
	require.Equal(t, "NaN", got[0].Kind)
	require.Equal(t, "cursed", got[0].Ref)
}
