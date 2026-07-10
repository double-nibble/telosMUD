package luasandbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lint_test.go — the SOLE-CHOKEPOINT build-failing lint for this package, the sibling of the zone's
// TestNoRawLuaCallsOutsideChokepoint. The instruction budget + wall-clock deadline are enforced only while a
// context is set (SetContext), and Runtime.call is the ONE place that arms/clears it. A raw L.PCall/DoString
// anywhere else silently loses both budgets. This lint fails the build if a raw Lua-entry call appears
// outside the sanctioned functions. Because this package has no type other than *lua.LState carrying these
// methods, a syntactic (method-name + enclosing-function) check is precise enough — no type resolution needed.

// pcallAllowed are the functions permitted to call L.PCall (THE chokepoint).
var pcallAllowed = map[string]bool{"call": true}

// callAllowed are the functions permitted to call L.Call — the capped-wrapper delegates that run a genuine
// builtin after their guard (the direct analog of the zone lint's callExemptFuncs).
var callAllowed = map[string]bool{"callDelegate": true, "callLuaFuncRepl": true}

func TestNoRawLuaCallsOutsideChokepoint(t *testing.T) {
	fset := token.NewFileSet()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			fnName := fn.Name.Name
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				line := fset.Position(call.Pos()).Line
				switch sel.Sel.Name {
				case "PCall":
					if !pcallAllowed[fnName] {
						t.Errorf("%s:%d: raw L.PCall in %s() — route through Runtime.call so the deadline+instruction budget are armed (sole-chokepoint invariant)", name, line, fnName)
					}
				case "Call":
					if !callAllowed[fnName] {
						t.Errorf("%s:%d: raw L.Call in %s() — only the capped-wrapper delegates may call a raw builtin (sole-chokepoint invariant)", name, line, fnName)
					}
				// CallByParam / Resume / DoString / DoFile / LoadString all run VM bytecode and must NEVER
				// appear here — they'd execute Lua without arming the deadline+budget. Mirrors the zone lint's
				// rawLuaCallMethods so the two sole-chokepoint enforcers cannot diverge.
				case "CallByParam", "Resume", "DoString", "DoFile", "LoadString":
					t.Errorf("%s:%d: L.%s in %s() — forbidden; compile via Runtime.Compile and invoke through Runtime.call (arms the deadline+instruction budget)", name, line, sel.Sel.Name, fnName)
				}
				return true
			})
		}
	}
}
