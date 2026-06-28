package world

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// luaharm_lint_test.go — the BUILD-FAILING funnel-reuse lint (docs/PHASE7-PLAN.md slice 7.3c,
// T8; hardened TYPE-AWARE in 7.4). The "can't-forget" property that keeps the harm gate
// un-bypassable AS THE LUA SURFACE GROWS: the Lua-binding files must NEVER write entity state
// directly — every harm vector must route the approved funnels (dealDamage / applyDebuff /
// guardCrossPlayerWrite / guardHarmful) or the existing op handlers that funnel them.
//
// Two structurally-distinct write classes are caught:
//
//  1. CALL writes — a call to a direct mutator function/method (setFlag(...), applyAffect(...),
//     a.expire(...)). The call-expression check (denyMutators / denyMethods).
//  2. ASSIGNMENT writes — a field/index assignment whose LHS targets ENTITY STATE. This is
//     TYPE-AWARE (go/types via go/packages), NOT a field-name denylist: a name list misses
//     fields as the structs evolve (e.zone / cooldowns / threat / combatRef / exits / …) AND is
//     alias-evadable (`m := e.living.flags; m[k]=v`). The type check resolves the LHS base type
//     and flags a write whose ultimate base is an entity-state type (*Entity / *Living /
//     *Affected / *Room / *affectInstance / *Wearer), so it covers every field automatically and
//     distinguishes `e.zone = x` (entity state → DENY) from `rt.inv = x` (a *luaRuntime field →
//     ALLOW). The local-alias map/slice write (`m[k]=v`, m a bare ident of map/slice type) is
//     also rejected — a binding has no legitimate local-collection write, and this closes the
//     alias path the base-type-of-selector check alone can't trace.
//
// If this test fails: you added a direct write to a Lua-binding file. Route the existing
// funnel/op-handler instead (P7-D3 invariant 3) — do NOT widen any allow-list to silence it.

// denyMutators is the set of direct state-mutator FUNCTIONS a Lua-binding file may NOT call.
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

// entityStateTypes is the set of struct type NAMES whose FIELDS are entity state — a write to any
// field of one (or to a map/slice that is such a field) bypasses the harm funnel. Matched by the
// type name (pointer-unwrapped). This list is the ONE thing to extend if a NEW entity-state
// struct is added — far more stable than a per-field list, and a missing TYPE (unlike a missing
// field) is conspicuous. The meta-test guards the matching logic against rot.
var entityStateTypes = map[string]bool{
	"Entity":         true,
	"Living":         true,
	"Affected":       true,
	"Room":           true,
	"affectInstance": true,
	"Wearer":         true,
}

// luaBindingFiles are the source files implementing the Lua API surface — the trust boundary
// this lint guards. As new binding files are added they MUST be added here.
var luaBindingFiles = map[string]bool{
	"luahandle.go": true,
	"luamud.go":    true,
	"luaharm.go":   true,
	"luaentry.go":  true, // 7.4 entry points
}

// lintViolation is one detected direct write (for the build-failing test + the meta-test).
type lintViolation struct {
	file string
	line int
	what string
}

// TestLuaBindingsHaveNoDirectMutators loads the world package with full type info and inspects
// the binding files for both write classes. It FAILS THE BUILD on any direct entity-state write.
func TestLuaBindingsHaveNoDirectMutators(t *testing.T) {
	pkg := loadWorldPackage(t)
	var violations []lintViolation
	for _, f := range pkg.Syntax {
		name := baseName(pkg.Fset.Position(f.Pos()).Filename)
		if !luaBindingFiles[name] {
			continue
		}
		violations = append(violations, inspectBindingWrites(pkg.Fset, f, pkg.TypesInfo, name)...)
	}
	for _, v := range violations {
		t.Errorf("%s:%d: Lua binding makes a direct entity-state write (%s) — route the harm funnel/op-handler instead (T8, P7-D3 invariant 3)",
			v.file, v.line, v.what)
	}
}

// inspectBindingWrites walks one parsed binding file (with type info) and returns every
// direct-write violation. It is the SHARED inspection logic the build-failing test and the
// meta-test both exercise, so the guard cannot rot into a no-op. It first builds a per-file
// TAINT set — local idents assigned DIRECTLY from an entity-state field (`m := e.living.flags`)
// — so a write through such an alias (`m[k]=v`) is flagged WITHOUT over-rejecting a
// binding-owned local map (binds[...]= …), which a blanket "any local map write" rule would.
func inspectBindingWrites(fset *token.FileSet, f *ast.File, info *types.Info, file string) []lintViolation {
	tainted := taintedAliases(f, info)
	var out []lintViolation
	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			switch fn := node.Fun.(type) {
			case *ast.Ident:
				if denyMutators[fn.Name] {
					out = append(out, lintViolation{file, fset.Position(node.Pos()).Line, "call " + fn.Name})
				}
			case *ast.SelectorExpr:
				if denyMethods[fn.Sel.Name] || denyMutators[fn.Sel.Name] {
					out = append(out, lintViolation{file, fset.Position(node.Pos()).Line, "call ." + fn.Sel.Name})
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				if what, bad := entityStateWrite(lhs, info, tainted); bad {
					out = append(out, lintViolation{file, fset.Position(node.Pos()).Line, what})
				}
			}
		}
		return true
	})
	return out
}

// taintedAliases scans f for local idents bound DIRECTLY to an entity-state field — `m :=
// e.living.flags` / `x := inst.source` — and returns the set of their type objects. A write
// through such an alias is the evasion the base-type-of-selector rule misses; tracking the
// alias by its declared object (not just its name) is precise and avoids flagging an unrelated
// binding-owned local of the same shape.
func taintedAliases(f *ast.File, info *types.Info) map[types.Object]bool {
	tainted := map[types.Object]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			if i >= len(as.Lhs) {
				break
			}
			// RHS is an entity-state field selector (e.living.flags / inst.source)?
			sel, ok := rhs.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if _, isState := entityStateBaseName(sel.X, info); !isState {
				continue
			}
			// LHS ident -> taint its object.
			if id, ok := as.Lhs[i].(*ast.Ident); ok && info != nil {
				if obj := info.ObjectOf(id); obj != nil {
					tainted[obj] = true
				}
			}
		}
		return true
	})
	return tainted
}

// entityStateWrite reports whether an assignment LHS writes entity state, and a description. It
// is TYPE-AWARE:
//
//   - SELECTOR LHS (x.field): flag if the type x is selected FROM is an entity-state type —
//     `e.zone = …`, `inst.source = …`, `e.living.flags = …` (base e.living is *Living).
//   - INDEX LHS (x[k]): `e.living.flags[k] = …` resolves through the e.living selector (base
//     *Living → DENY). A bare-Ident base is flagged ONLY when it is a TAINTED alias of an
//     entity-state field (`m := e.living.flags; m[k]=v`), so a binding-owned local map
//     (binds[...] = …) is correctly allowed.
func entityStateWrite(lhs ast.Expr, info *types.Info, tainted map[types.Object]bool) (string, bool) {
	switch e := lhs.(type) {
	case *ast.SelectorExpr:
		if name, ok := entityStateBaseName(e.X, info); ok {
			return "assign " + name + "." + e.Sel.Name, true
		}
	case *ast.IndexExpr:
		if sel, ok := e.X.(*ast.SelectorExpr); ok {
			if name, ok2 := entityStateBaseName(sel.X, info); ok2 {
				return "assign " + name + "." + sel.Sel.Name + "[]", true
			}
		}
		if id, ok := e.X.(*ast.Ident); ok && info != nil {
			if obj := info.ObjectOf(id); obj != nil && tainted[obj] {
				return "assign (entity-state alias write) " + id.Name + "[]", true
			}
		}
	}
	return "", false
}

// entityStateBaseName returns the entity-state type name `expr` evaluates to (pointer
// unwrapped), and whether it is one. `expr` is the receiver of a field selection.
func entityStateBaseName(expr ast.Expr, info *types.Info) (string, bool) {
	name := namedTypeName(exprType(expr, info))
	if entityStateTypes[name] {
		return name, true
	}
	return "", false
}

func exprType(expr ast.Expr, info *types.Info) types.Type {
	if info == nil {
		return nil
	}
	if tv, ok := info.Types[expr]; ok {
		return tv.Type
	}
	// A bare ident receiver (e in e.zone) lands in Uses/Defs, not Types.
	if id, ok := expr.(*ast.Ident); ok {
		if obj := info.ObjectOf(id); obj != nil {
			return obj.Type()
		}
	}
	return nil
}

// namedTypeName returns the bare type name of t (pointer unwrapped), or "".
func namedTypeName(t types.Type) string {
	if t == nil {
		return ""
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}

// loadWorldPackage loads the world package with full syntax + type info. Loading "." (the
// current package) is fast and cached; a load failure fails the test loudly (the guard must not
// silently degrade into a no-op).
func loadWorldPackage(t *testing.T) *packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedFiles,
		Fset: token.NewFileSet(),
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		t.Fatalf("loading the world package for the lint: %v", err)
	}
	for _, p := range pkgs {
		if p.TypesInfo != nil && len(p.Syntax) > 0 {
			return p
		}
	}
	t.Fatal("could not load the world package with type info for the lint")
	return nil
}

func baseName(full string) string {
	if i := strings.LastIndexByte(full, '/'); i >= 0 {
		return full[i+1:]
	}
	return full
}

// --- the META-test: the lint catches every class, and does NOT over-reject -----------------

// TestLuaLintCatchesAViolation proves the type-aware inspection DETECTS each write class (the
// CRITICAL e.zone= and HIGH cooldowns[x]= cases, the e.living.flags alias) and does NOT
// false-positive on legitimate binding code (rt.inv = …, a binding-owned local map). It
// type-checks a SELF-CONTAINED synthetic package (declaring its own entity-state type names, so
// it needs no importer gymnastics) and runs the SAME inspectBindingWrites the build-failing test
// uses — so the guard cannot rot.
func TestLuaLintCatchesAViolation(t *testing.T) {
	src := `package synth

type Entity struct {
	zone     *int
	location *int
	living   *Living
	room     *Room
}
type Living struct {
	flags     map[string]bool
	cooldowns map[string]int
}
type Affected struct{ list []int }
type affectInstance struct{ source *int }
type Room struct{ exits map[string]string }
type rt struct{ inv *int }

func setFlag(a *Living, k string, v bool) {}
func (a *Affected) expire(e *Entity, x *int) {}

func mustCatch(e *Entity, a *Affected, inst *affectInstance, r *rt) {
	setFlag(e.living, "x", true)        // 1 call write
	a.expire(e, nil)                    // 2 call write
	e.zone = nil                        // 3 CRITICAL cross-zone re-parent
	e.location = nil                    // 4
	inst.source = nil                   // 5
	e.living.flags["x"] = true          // 6 index-of-selector, base *Living
	e.living.cooldowns["x"] = 0         // 7 HIGH rate-gate bypass
	a.list = append(a.list, 1)          // 8
	e.room.exits["n"] = "x"             // 9 index-of-selector, base *Room

	m := e.living.flags                 //   TAINTED alias of an entity-state field
	m["k"] = true                       // 10 alias write -> MUST flag

	// MUST-NOT-FLAG (legitimate binding code):
	r.inv = nil                         // a *rt field — NOT entity state
	local := map[string]int{}           // a binding-owned local map (NOT an entity-state alias)
	local["x"] = 1                      // -> MUST NOT flag (precise alias tracking, not blanket)
	_ = local
	_ = m
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synth.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}
	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: importer.Default(), Error: func(error) {}}
	if _, err := conf.Check("synth", fset, []*ast.File{f}, info); err != nil {
		// Tolerated: the synthetic has intentional unused-var diagnostics; type info is still
		// populated for the expressions the lint reads.
		t.Logf("synthetic type-check diagnostics (tolerated): %v", err)
	}

	got := inspectBindingWrites(fset, f, info, "synth.go")
	desc := map[string]bool{}
	for _, v := range got {
		desc[v.what] = true
	}

	mustCatch := []string{
		"call setFlag",
		"call .expire",
		"assign Entity.zone", // CRITICAL
		"assign Entity.location",
		"assign affectInstance.source",
		"assign Living.flags[]",
		"assign Living.cooldowns[]", // HIGH
		"assign Affected.list",
		"assign Room.exits[]",
	}
	for _, m := range mustCatch {
		if !desc[m] {
			t.Errorf("the lint MISSED required violation %q; caught: %v", m, sortedKeys(desc))
		}
	}
	// The TAINTED alias write (m := e.living.flags; m[k]=v) must be caught.
	if !anyHasPrefix(desc, "assign (entity-state alias write)") {
		t.Errorf("the lint MISSED the entity-state alias write (m := e.living.flags; m[k]=v); caught: %v", sortedKeys(desc))
	}
	// MUST-NOT-FLAG: the *rt field write and the binding-owned local map (local["x"]=1) must NOT
	// be flagged — precise alias tracking, not a blanket local-collection rule.
	for d := range desc {
		if strings.Contains(d, ".inv") {
			t.Errorf("the lint over-rejected a legitimate non-entity field write: %q", d)
		}
		if strings.Contains(d, "local[]") || strings.Contains(d, "local collection") {
			t.Errorf("the lint over-rejected a binding-owned local map write: %q", d)
		}
	}
}

func anyHasPrefix(m map[string]bool, p string) bool {
	for k := range m {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
