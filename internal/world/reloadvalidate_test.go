package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// TestValidatePacks covers the #192 pre-publish gate: a clean attribute graph validates, a malformed base
// formula and an attribute reference cycle are both reported (so republish blocks the publish). It reuses
// the SAME boot functions (parseAttributeBase + lintAttributeCycles), so "validated" == what boot builds.
func TestValidatePacks(t *testing.T) {
	lit := func(v float64) content.BaseSpecDTO { return content.BaseSpecDTO{Lit: &v} }
	expr := func(e any) content.BaseSpecDTO { return content.BaseSpecDTO{Expr: e} }

	// Valid: a literal base + a derived attribute referencing it (no cycle).
	valid := []content.Pack{{Pack: "p", Attributes: []content.AttributeDTO{
		{Ref: "con", DefaultBase: lit(10)},
		{Ref: "hp", DefaultBase: expr([]any{"*", []any{"attr", "con"}, 10.0})},
	}}}
	if p := validatePacks(valid); len(p) != 0 {
		t.Fatalf("valid pack flagged: %v", p)
	}

	// A base formula that is not a valid node (a bare string) is a parse problem.
	bad := []content.Pack{{Pack: "p", Attributes: []content.AttributeDTO{
		{Ref: "broken", DefaultBase: expr("not-a-node")},
	}}}
	if p := validatePacks(bad); len(p) != 1 {
		t.Fatalf("bad base formula: want 1 problem, got %v", p)
	}

	// An attribute reference cycle a <-> b (would break derived-stat resolution).
	cyc := []content.Pack{{Pack: "p", Attributes: []content.AttributeDTO{
		{Ref: "a", DefaultBase: expr([]any{"attr", "b"})},
		{Ref: "b", DefaultBase: expr([]any{"attr", "a"})},
	}}}
	if p := validatePacks(cyc); len(p) == 0 {
		t.Fatal("attribute cycle not detected")
	}

	// Attributes merge across packs last-write-wins by ref, so a cycle spanning two packs is still caught.
	split := []content.Pack{
		{Pack: "a", Attributes: []content.AttributeDTO{{Ref: "a", DefaultBase: expr([]any{"attr", "b"})}}},
		{Pack: "b", Attributes: []content.AttributeDTO{{Ref: "b", DefaultBase: expr([]any{"attr", "a"})}}},
	}
	if p := validatePacks(split); len(p) == 0 {
		t.Fatal("cross-pack attribute cycle not detected")
	}
}
