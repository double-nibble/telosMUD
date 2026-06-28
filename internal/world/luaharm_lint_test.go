package world

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// luaharm_lint_test.go — the BUILD-FAILING funnel-reuse lint (docs/PHASE7-PLAN.md slice 7.3c,
// T8). The "can't-forget" property that keeps the harm gate un-bypassable AS THE LUA SURFACE
// GROWS: the Lua-binding files (luahandle.go / luamud.go / luaharm.go) must NEVER call a direct
// state-mutator (a vital/affect/flag/position/attr write) — every harm vector must route the
// approved funnels (dealDamage / applyDebuff / guardCrossPlayerWrite / guardHarmful) or the
// existing op handlers that funnel them (opHeal / opModifyResource / opApplyAffect /
// opRemoveAffect / opDispel). This is an AST CALL-EXPRESSION check (not a grep — a grep would
// false-positive on a doc comment that merely NAMES a funnel, and miss a method-value call), so
// it genuinely fails the build the moment a new binding method introduces a direct write.
//
// If this test fails: you added a direct mutator to a Lua-binding file. Route the existing
// funnel/op-handler instead (the can't-bypass property, P7-D3 invariant 3) — do NOT add the
// mutator to the allow-list to silence the test.

// denyMutators is the set of direct state-mutator functions a Lua-binding file may NOT call.
// They write a vital/affect/flag/position/attribute directly, bypassing the harm gate. Each is
// reachable only through an approved funnel/op-handler, which gates first.
var denyMutators = map[string]bool{
	"setResourceCurrent": true, // vital/resource write — must route dealDamage/opHeal/opModifyResource
	"setFlag":            true, // flag write — no Lua flag-write surface this phase (P7-D3 prohibition)
	"setPosition":        true, // position write — must route the death/combat seam, not a binding
	"setAttrBase":        true, // attribute write — no Lua attr-write surface
	"applyAffect":        true, // affect attach — must route applyDebuff/opApplyAffect (gates first)
	"addThreat":          true, // threat write — only the dealDamage pipeline accrues threat
}

// denyMethods is the set of mutating METHODS (selector calls like a.expire(...)) a binding file
// may not call directly — affect removal must route opRemoveAffect/opDispel, which gate first.
var denyMethods = map[string]bool{
	"expire": true, // Affected.expire — affect removal must route opRemoveAffect/opDispel
}

// luaBindingFiles are the source files implementing the Lua API surface — the trust boundary
// this lint guards. As new binding files are added (entry points 7.4, reactions 7.9) they MUST
// be added here so the can't-forget property keeps covering the whole surface.
var luaBindingFiles = []string{
	"luahandle.go",
	"luamud.go",
	"luaharm.go",
}

func TestLuaBindingsHaveNoDirectMutators(t *testing.T) {
	fset := token.NewFileSet()
	for _, file := range luaBindingFiles {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				// A bare function call: setFlag(...), applyAffect(...).
				if denyMutators[fn.Name] {
					pos := fset.Position(call.Pos())
					t.Errorf("%s:%d: Lua binding calls direct mutator %q — route the harm funnel/op-handler instead (T8, P7-D3 invariant 3)",
						file, pos.Line, fn.Name)
				}
			case *ast.SelectorExpr:
				// A method call: a.expire(...), x.setFlag(...).
				if denyMethods[fn.Sel.Name] {
					pos := fset.Position(call.Pos())
					t.Errorf("%s:%d: Lua binding calls direct mutator method %q — route the harm funnel/op-handler instead (T8)",
						file, pos.Line, fn.Sel.Name)
				}
				// Also catch a package/var-qualified direct mutator (world.setFlag style) by the
				// selector's final name.
				if denyMutators[fn.Sel.Name] {
					pos := fset.Position(call.Pos())
					t.Errorf("%s:%d: Lua binding calls direct mutator %q — route the harm funnel/op-handler instead (T8)",
						file, pos.Line, fn.Sel.Name)
				}
			}
			return true
		})
	}
}

// TestLuaLintCatchesAViolation is a META-test: it proves the lint above actually DETECTS a
// direct mutator, so the can't-forget guard cannot silently rot into a no-op. It builds a tiny
// AST containing a setFlag call and asserts the same inspection logic flags it.
func TestLuaLintCatchesAViolation(t *testing.T) {
	src := `package world
func bad() { setFlag(nil, "x", true); a.expire(nil, nil) }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}
	var hits int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if denyMutators[fn.Name] {
				hits++
			}
		case *ast.SelectorExpr:
			if denyMethods[fn.Sel.Name] || denyMutators[fn.Sel.Name] {
				hits++
			}
		}
		return true
	})
	if hits != 2 {
		t.Fatalf("the funnel-reuse lint failed to catch the synthetic violations: hits=%d, want 2", hits)
	}
}
