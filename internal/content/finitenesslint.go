package content

import "math"

// finitenesslint.go rejects NON-FINITE numeric literals (±Inf, NaN) in the content fields that feed the
// attribute modifier stack. It exists because YAML 1.1 float literals — `.inf`, `-.inf`, `.nan` —
// decode straight into a DTO `float64` with no error (only an over-range decimal like `1e400` is
// rejected, as a string). A single `value: .nan` on an affect modifier is therefore an authorable
// poison: NaN escapes an attribute's declared min/max clamp (every comparison against NaN is false),
// and ±Inf blows the derivation.
//
// The ENGINE screen (world.attrScreen) is the real defense — it bounds the fold and marks the result
// degraded regardless of how the non-finite value got there. This lint is defense in depth and, more
// usefully, OPERATOR SIGNAL: it names the offending pack/field at load so a builder fixes the typo,
// rather than discovering a silently-degraded attribute in play. It walks only the fields that feed an
// attribute value — modifier magnitudes and attribute bounds/bases — not every float in the schema,
// because those are the ones whose non-finiteness has a security consequence.

// FinitenessViolation is one non-finite numeric literal found in a pack.
type FinitenessViolation struct {
	Pack  string
	Field string // a human path, e.g. "affect weaken modifier[0].value"
	Ref   string // the owning def's ref, for the operator
	Kind  string // "Inf" or "NaN"
}

// LintFinite returns a finding for every non-finite numeric literal in the attribute-feeding fields of
// every pack. Read-only; build-time (boot lint + reload gate).
func LintFinite(packs []Pack) []FinitenessViolation {
	var out []FinitenessViolation
	check := func(pack, ref, field string, v float64) {
		switch {
		case math.IsInf(v, 0):
			out = append(out, FinitenessViolation{Pack: pack, Ref: ref, Field: field, Kind: "Inf"})
		case math.IsNaN(v):
			out = append(out, FinitenessViolation{Pack: pack, Ref: ref, Field: field, Kind: "NaN"})
		}
	}
	checkPtr := func(pack, ref, field string, v *float64) {
		if v != nil {
			check(pack, ref, field, *v)
		}
	}

	for _, p := range packs {
		for _, a := range p.Attributes {
			checkPtr(p.Pack, a.Ref, "attribute "+a.Ref+".min", a.Min)
			checkPtr(p.Pack, a.Ref, "attribute "+a.Ref+".max", a.Max)
			checkPtr(p.Pack, a.Ref, "attribute "+a.Ref+".default_base.lit", a.DefaultBase.Lit)
			// A base FORMULA (the nested-array AST) can carry `.inf`/`.nan` as a bare number literal
			// anywhere in the tree — `default_base: {expr: ["+", .nan, 5]}`. Walk it; the DefaultBase.Lit
			// check above only covers the literal-base form.
			walkFormulaLiterals(a.DefaultBase.Expr, func(v float64) {
				check(p.Pack, a.Ref, "attribute "+a.Ref+".default_base.expr", v)
			})
		}
		for _, af := range p.Affects {
			for i, m := range af.Body.Modifiers {
				check(p.Pack, af.Ref, fieldPath("affect", af.Ref, i, "value"), m.Value)
			}
		}
	}
	return out
}

// walkFormulaLiterals descends a decoded formula-AST value (the generic nested-array form) and calls
// `visit` for every bare numeric literal it finds. A formula node is either a number (a literal), a
// list whose head is an op string followed by argument nodes, or (defensively) other shapes it
// ignores. Both float64 (YAML/JSON decode) and int are handled.
func walkFormulaLiterals(n any, visit func(float64)) {
	switch v := n.(type) {
	case float64:
		visit(v)
	case int:
		visit(float64(v))
	case []any:
		// [op, arg, arg, ...] — skip the head (an op string or `lit`/`attr` marker) but a `["lit", n]`
		// still surfaces its number as an element, so walk every element and let the number case catch it.
		for _, e := range v {
			walkFormulaLiterals(e, visit)
		}
	}
}

// fieldPath renders a stable, index-bearing field path without pulling in fmt (this file is on the
// content boot path and stays dependency-light).
func fieldPath(kind, ref string, idx int, field string) string {
	return kind + " " + ref + " modifier[" + itoa(idx) + "]." + field
}

// itoa is a tiny base-10 formatter for the small indices here (avoids fmt for one integer).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
