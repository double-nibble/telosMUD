package world

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// identity_lint_test.go — the BUILD-FAILING zone-locality lint (#410, slice 1 of #72). The
// "can't-forget" property that keeps zone identity and zone CONTENT distinct as the package grows:
// no file in internal/world may decide "is this ref mine" by comparing a parseRef zone result
// against a *Zone's `id` (or `template`) field. That decision has exactly one home —
// Zone.ownsZoneRef / Zone.localRoom in identity.go.
//
// WHY A LINT AND NOT JUST A CODE REVIEW: six such raw comparisons existed before #410, and they were
// individually invisible — `destZone != z.id` reads as obviously correct. Once instanced zones land
// (`crypt#7` hosting the AUTHORED refs of `crypt`), every one of them is a bug that reads EVERY exit
// in the instance as leaving the zone. The worst is combat_commands.go's flee: move() already refuses
// to walk while fighting, so with flee refused in every room of every instance a party that wipes has
// no way out at all. A seventh site added later reintroduces the whole class silently, and — because
// `template == id` for every zone that exists today — NO behavioural test can catch it until minting
// lands. That is exactly the shape of bug a build-failing structural lint exists for.
//
// If this test fails: you added a raw zone-identity comparison. Call z.ownsZoneRef(zoneID) (the
// routing question — "does this ref stay inside me") or z.localRoom(ref) (routing + the room lookup)
// instead. Do NOT add your file to the exemption set to silence it.

// zoneIdentityFields are the *Zone fields that answer "which live zone actor is this" (id) and
// "whose content is this" (template). Comparing a parseRef zone result against EITHER by hand is the
// violation: `id` is wrong inside an instance, and `template` alone is wrong for a plain zone whose
// callers may hold its id — ownsZoneRef is the only correct combination of the two.
var zoneIdentityFields = map[string]bool{
	"id":       true,
	"template": true,
}

// zoneIdentityExemptFiles are the files allowed to compare a parseRef zone result against a *Zone
// identity field. identity.go is THE home of the predicate; nothing else qualifies.
var zoneIdentityExemptFiles = map[string]bool{
	"identity.go": true,
}

// TestNoRawZoneIDComparisons is the build-failing lint. It loads the world package with full type
// info and FAILS on any `<parseRef zone> == z.id` / `!= c.z.id` / `== rt.zone.template` comparison
// outside identity.go. The receiver is resolved BY TYPE (*Zone), not by variable name, so `z`, `c.z`,
// `rt.zone` and any future spelling are all covered, while an unrelated `.id` on some other type
// (a content DTO's `z.Ref`, a session field) is correctly ignored.
func TestNoRawZoneIDComparisons(t *testing.T) {
	pkg := loadWorldPackage(t)
	live := 0
	for _, f := range pkg.Syntax {
		name := baseName(pkg.Fset.Position(f.Pos()).Filename)
		if strings.HasSuffix(name, "_test.go") || zoneIdentityExemptFiles[name] {
			continue
		}
		live += len(parseRefZoneIdents(f, pkg.TypesInfo))
		for _, v := range inspectZoneIDComparisons(pkg.Fset, f, pkg.TypesInfo, name) {
			t.Errorf("%s:%d: %s — raw zone-identity comparison. Use z.ownsZoneRef(zoneID) or z.localRoom(ref) instead: a `== z.id` reads every exit in an INSTANCED zone (#72) as leaving the zone (identity.go, #410)",
				v.file, v.line, v.what)
		}
	}
	// ANTI-ROT: the lint is only meaningful while parseRef results are actually bound to named
	// idents somewhere it inspects. If parseRef is renamed or every call site changes shape, the
	// taint set silently empties and the lint degrades into a no-op that passes forever.
	if live == 0 {
		t.Fatal("the zone-locality lint found NO parseRef zone bindings to inspect — it has gone inert (was parseRef renamed?); fix parseRefZoneIdents rather than deleting this guard")
	}
}

// inspectZoneIDComparisons walks one parsed file (with type info) and returns every raw
// zone-identity comparison, tagged with its enclosing function. It is the SHARED inspection logic
// the build-failing test and the meta-test both run, so the guard cannot rot into a no-op.
//
// LIMITATION (taint is ONE HOP, direct-binding only): `destZone, _ := parseRef(ref); destZone == z.id`
// IS caught; laundering through a second assignment (`zn := destZone; zn == z.id`) or through a
// helper function parameter is NOT. This closes the realistic, idiomatic shape — the one all six
// pre-#410 sites were written in — not a deliberate evasion.
//
// It also does not see a zone prefix reconstructed by string manipulation rather than by parseRef —
// `strings.HasPrefix(string(ref), z.id+":")` would evade it entirely and is wrong for exactly the same
// reason. No such site exists in internal/world today (every HasPrefix here parses "$", "formula:",
// channels, help topics or dice). Do not introduce one; use ownsZoneRef.
func inspectZoneIDComparisons(fset *token.FileSet, f *ast.File, info *types.Info, file string) []lintViolation {
	tainted := parseRefZoneIdents(f, info)
	if len(tainted) == 0 {
		return nil
	}
	var out []lintViolation
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		fnName := fn.Name.Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
				return true
			}
			if what, bad := zoneIDComparison(bin, info, tainted); bad {
				out = append(out, lintViolation{file, fset.Position(bin.Pos()).Line, what + " in " + fnName + "()"})
			}
			return true
		})
	}
	return out
}

// parseRefZoneIdents returns the set of local objects bound to parseRef's FIRST result (the zone
// segment) — `destZone, destRoom := parseRef(ref)`, `zoneOf, _ := parseRef(ref)`. A blank first
// result (`_, destRoom := parseRef(ref)`) binds nothing and is correctly not tainted: that call site
// asked only for the room key and cannot make a locality decision at all.
func parseRefZoneIdents(f *ast.File, info *types.Info) map[types.Object]bool {
	out := map[types.Object]bool{}
	if info == nil {
		return out
	}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) != 2 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		fnIdent, ok := call.Fun.(*ast.Ident)
		if !ok || fnIdent.Name != "parseRef" {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || id.Name == "_" {
			return true
		}
		if obj := info.ObjectOf(id); obj != nil {
			out[obj] = true
		}
		return true
	})
	return out
}

// zoneIDComparison reports whether bin compares a tainted (parseRef-zone) ident against a *Zone
// identity field, in EITHER operand order, and describes it. The `.id` receiver is resolved by TYPE
// so an unrelated `.id`/`.Ref` on another type is not flagged.
func zoneIDComparison(bin *ast.BinaryExpr, info *types.Info, tainted map[types.Object]bool) (string, bool) {
	if info == nil {
		return "", false
	}
	for _, pair := range [][2]ast.Expr{{bin.X, bin.Y}, {bin.Y, bin.X}} {
		id, ok := pair[0].(*ast.Ident)
		if !ok {
			continue
		}
		obj := info.ObjectOf(id)
		if obj == nil || !tainted[obj] {
			continue
		}
		sel, ok := pair[1].(*ast.SelectorExpr)
		if !ok || !zoneIdentityFields[sel.Sel.Name] {
			continue
		}
		if namedTypeName(exprType(sel.X, info)) != "Zone" {
			continue
		}
		return "compare parseRef zone `" + id.Name + "` against Zone." + sel.Sel.Name, true
	}
	return "", false
}

// --- the META-test: the lint catches the class, and does NOT over-reject -------------------

// TestZoneIdentityLintCatchesAViolation proves the inspection DETECTS each shape the six pre-#410
// sites were written in (both operand orders, a plain `z`, a nested `c.z` / `rt.zone` receiver, and
// a comparison against `template` as well as `id`) and does NOT false-positive on the legitimate
// neighbours (the bare-ref `zoneID == ""` test, a comparison between two zone ids, an `.id` on a
// non-Zone type, and a room ref bound from parseRef's SECOND result). It type-checks a
// self-contained synthetic package and runs the SAME inspectZoneIDComparisons the build-failing test
// uses — so the guard cannot rot into a no-op.
func TestZoneIdentityLintCatchesAViolation(t *testing.T) {
	src := `package synth

type ProtoRef string

type Zone struct {
	id       string
	template string
	rooms    map[ProtoRef]*int
}
type Context struct{ z *Zone }
type luaRuntime struct{ zone *Zone }
type session struct{ id string }

func parseRef(ref ProtoRef) (string, ProtoRef) { return "", ref }

func mustCatch(z *Zone, c *Context, rt *luaRuntime, ref ProtoRef) {
	destZone, destRoom := parseRef(ref)
	_ = destRoom
	if destZone != z.id {          // 1 the commands.go / targeting.go shape
		return
	}
	if z.id == destZone {          // 2 reversed operand order
		return
	}
	if destZone != c.z.id {        // 3 the combat_commands.go nested receiver
		return
	}
	if destZone == rt.zone.id {    // 4 the luaharm.go nested receiver
		return
	}
	if destZone == z.template {    // 5 template is identity too — ownsZoneRef owns the combination
		return
	}
}

func mustNotFlag(z *Zone, s *session, ref ProtoRef, other *Zone) {
	// The BARE-REF test: a parseRef zone compared to the empty string is not an identity comparison.
	zoneID, roomRef := parseRef(ref)
	if zoneID == "" {
		return
	}
	// parseRef's SECOND result (a room key) is not a zone and never taints.
	if roomRef == "x" {
		return
	}
	// Two zone ids compared to each other — no parseRef result involved.
	if z.id == other.id {
		return
	}
	// An .id field on an unrelated type must not be flagged even against a tainted ident.
	if zoneID == s.id {
		return
	}
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
		// Tolerated: the synthetic carries intentional diagnostics; type info is still populated
		// for the expressions the lint reads.
		t.Logf("synthetic type-check diagnostics (tolerated): %v", err)
	}

	got := inspectZoneIDComparisons(fset, f, info, "synth.go")
	inFunc := map[string]int{}
	desc := map[string]bool{}
	for _, v := range got {
		desc[v.what] = true
		switch {
		case strings.HasSuffix(v.what, "in mustCatch()"):
			inFunc["mustCatch"]++
		case strings.HasSuffix(v.what, "in mustNotFlag()"):
			inFunc["mustNotFlag"]++
		}
	}

	for _, want := range []string{
		"compare parseRef zone `destZone` against Zone.id in mustCatch()",
		"compare parseRef zone `destZone` against Zone.template in mustCatch()",
	} {
		if !desc[want] {
			t.Errorf("the lint MISSED required violation %q; caught: %v", want, sortedKeys(desc))
		}
	}
	// All five mustCatch shapes must be flagged — not just the one that happens to be first.
	if inFunc["mustCatch"] != 5 {
		t.Errorf("the lint flagged %d of 5 raw-comparison shapes in mustCatch; caught: %v", inFunc["mustCatch"], sortedKeys(desc))
	}
	// And none of the legitimate neighbours may be flagged.
	if inFunc["mustNotFlag"] != 0 {
		t.Errorf("the lint OVER-REJECTED %d legitimate comparison(s) in mustNotFlag; caught: %v", inFunc["mustNotFlag"], sortedKeys(desc))
	}
}
