package world

import (
	"fmt"
	"math"

	"github.com/double-nibble/telosmud/internal/logcap"
)

// formula.go is the prefix-AST expression evaluator for derived attributes (docs/PHASE5-PLAN.md
// §1.1, USER DECISION: prefix-AST JSON, NOT Lua, NOT infix). A formula is a nested array:
//
//	["+", ["*", ["attr","con"], 10], ["*", ["attr","level"], 5]]   == con*10 + level*5
//
// Allowed heads: + - * / min max clamp floor ceil round mod, the 3-ary conditional ["if",c,t,e]
// (short-circuiting), ["attr", name], ["lit", n]. A bare JSON number is also a literal (a convenience
// so {"lit": 5} and 5 both parse). An ["attr", name] node pulls that attribute's RESOLVED value
// (recursive attr()), so derived-of-derived works. floor/ceil/round/mod/if are the Phase 6 [G1]
// additions for exact integer derivation; in a CHECK formula (check.go) an attr ref may carry a
// `$actor.`/`$target.`/`$source.` scope prefix the check resolver dispatches on. Parsing happens once
// at content-load (parseFormula -> a typed tree); evaluation happens per attr() call (memoized by
// the caller). Both parse-time and eval-time guard against reference cycles.

// formulaNode is one node of a parsed formula tree. Eval pulls attribute values through the
// resolver (an entity's attr() bound at the call site) and is the only place a formula touches
// entity state — keeping the tree itself immutable and shareable across zone goroutines.
type formulaNode interface {
	// eval computes this node's value. r resolves an ["attr",name] reference to its current value;
	// it carries the visited set for eval-time cycle detection. An error aborts the whole resolve.
	eval(r *formulaResolver) (float64, error)
	// refs appends every attribute ref this subtree references (for the load-time cycle lint).
	refs(into map[string]bool)
}

// formulaResolver is the eval-time context: it resolves an attr ref to its value and tracks the
// chain of refs currently being resolved so a self/mutual cycle errors instead of recursing forever.
type formulaResolver struct {
	// resolve returns the value of attribute `ref` (a recursive attr() on the same entity). It is
	// supplied by the derivation layer (attributes.go); formula.go never knows about entities.
	resolve func(ref string, visited map[string]bool) (float64, error)
	// visited is the set of attribute refs on the current resolution stack.
	visited map[string]bool
}

// litNode is a literal constant.
type litNode struct{ v float64 }

func (n litNode) eval(*formulaResolver) (float64, error) { return n.v, nil }
func (litNode) refs(map[string]bool)                     {}

// attrNode references another attribute; eval pulls its resolved value (recursive).
type attrNode struct{ ref string }

func (n attrNode) eval(r *formulaResolver) (float64, error) {
	return r.resolve(n.ref, r.visited)
}
func (n attrNode) refs(into map[string]bool) { into[n.ref] = true }

// opNode is an n-ary arithmetic/min/max/clamp operation over its children.
type opNode struct {
	op   string
	args []formulaNode
}

func (n opNode) refs(into map[string]bool) {
	for _, a := range n.args {
		a.refs(into)
	}
}

func (n opNode) eval(r *formulaResolver) (float64, error) {
	// `if` SHORT-CIRCUITS (the only non-strict head): evaluate the condition, then ONLY the taken
	// branch — so a div-by-zero / costly subtree in the untaken branch never runs. ["if",cond,then,else],
	// cond != 0 => then. Phase 6 [G1] — exact integer derivation (5e mods, PF BAB, WoW ratings).
	if n.op == "if" {
		if len(n.args) != 3 {
			return 0, fmt.Errorf("formula: if needs exactly 3 args (cond, then, else), got %d", len(n.args))
		}
		cond, err := n.args[0].eval(r)
		if err != nil {
			return 0, err
		}
		if cond != 0 {
			return n.args[1].eval(r)
		}
		return n.args[2].eval(r)
	}

	vals := make([]float64, len(n.args))
	for i, a := range n.args {
		v, err := a.eval(r)
		if err != nil {
			return 0, err
		}
		vals[i] = v
	}
	switch n.op {
	case "+":
		var s float64
		for _, v := range vals {
			s += v
		}
		return s, nil
	case "-":
		if len(vals) == 0 {
			return 0, nil
		}
		if len(vals) == 1 {
			return -vals[0], nil // unary negate
		}
		s := vals[0]
		for _, v := range vals[1:] {
			s -= v
		}
		return s, nil
	case "*":
		s := 1.0
		for _, v := range vals {
			s *= v
		}
		return s, nil
	case "/":
		if len(vals) < 2 {
			return 0, fmt.Errorf("formula: / needs >=2 args, got %d", len(vals))
		}
		s := vals[0]
		for _, v := range vals[1:] {
			if v == 0 {
				return 0, fmt.Errorf("formula: division by zero")
			}
			s /= v
		}
		return s, nil
	case "min":
		if len(vals) == 0 {
			return 0, fmt.Errorf("formula: min needs >=1 arg")
		}
		m := vals[0]
		for _, v := range vals[1:] {
			if v < m {
				m = v
			}
		}
		return m, nil
	case "max":
		if len(vals) == 0 {
			return 0, fmt.Errorf("formula: max needs >=1 arg")
		}
		m := vals[0]
		for _, v := range vals[1:] {
			if v > m {
				m = v
			}
		}
		return m, nil
	case "clamp":
		// clamp(x, lo, hi)
		if len(vals) != 3 {
			return 0, fmt.Errorf("formula: clamp needs exactly 3 args, got %d", len(vals))
		}
		x, lo, hi := vals[0], vals[1], vals[2]
		if x < lo {
			return lo, nil
		}
		if x > hi {
			return hi, nil
		}
		return x, nil
	case "floor":
		if len(vals) != 1 {
			return 0, fmt.Errorf("formula: floor needs exactly 1 arg, got %d", len(vals))
		}
		return math.Floor(vals[0]), nil
	case "ceil":
		if len(vals) != 1 {
			return 0, fmt.Errorf("formula: ceil needs exactly 1 arg, got %d", len(vals))
		}
		return math.Ceil(vals[0]), nil
	case "round":
		if len(vals) != 1 {
			return 0, fmt.Errorf("formula: round needs exactly 1 arg, got %d", len(vals))
		}
		return math.Round(vals[0]), nil
	case "mod":
		// mod(a, b): the remainder a − b·trunc(a/b) (Go/math.Mod semantics). b==0 errors.
		if len(vals) != 2 {
			return 0, fmt.Errorf("formula: mod needs exactly 2 args, got %d", len(vals))
		}
		if vals[1] == 0 {
			return 0, fmt.Errorf("formula: mod by zero")
		}
		return math.Mod(vals[0], vals[1]), nil
	default:
		return 0, fmt.Errorf("formula: unknown op %q", logcap.Value(n.op))
	}
}

// evalFinite evaluates a formula node and FAILS CLOSED on a non-finite result (docs/REMAINING.md §4). A
// content formula can overflow to ±Inf (`1e308*1e308`) or produce NaN (`Inf-Inf`, `0*Inf`) with no
// arithmetic error; left unchecked that non-finite value reaches a game value as `int(+Inf)`=maxint64 (e.g.
// maxint damage on the ungated paths) or `int(NaN)`=0. Only the TOP-LEVEL result is checked, so a harmless
// intermediate Inf that a `min`/`clamp` tames back to a finite value is still allowed — only a formula whose
// FINAL value is non-finite is rejected. Every top-level formula consumer (check bonus, attribute base,
// grant base) evaluates through this, so the guard is inherited uniformly rather than re-implemented per op.
func evalFinite(n formulaNode, r *formulaResolver) (float64, error) {
	v, err := n.eval(r)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("formula: non-finite result (NaN or ±Inf)")
	}
	return v, nil
}

// parseFormula parses a generic decoded JSON/YAML value (the FormulaNodeDTO) into a typed tree. It
// accepts:
//   - a number              -> litNode
//   - ["lit", n]            -> litNode
//   - ["attr", name]        -> attrNode
//   - [op, arg, arg, ...]   -> opNode (op in + - * / min max clamp)
//
// nil parses to nil (no formula). It is called once at content load (build time), never on the hot
// path, so the reflection-y type switching here costs nothing in steady state.
func parseFormula(v any) (formulaNode, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case float64:
		return litNode{v: t}, nil
	case int:
		return litNode{v: float64(t)}, nil
	case int64:
		return litNode{v: float64(t)}, nil
	case []any:
		return parseArrayNode(t)
	default:
		// %v on a decoded value can itself be a huge builder-controlled string; cap it (#481). The type
		// is the primary diagnostic — the value is a bounded hint.
		return nil, fmt.Errorf("formula: unexpected node %T (%v)", v, logcap.Value(fmt.Sprintf("%v", v)))
	}
}

// parseArrayNode parses the [head, args...] array form.
func parseArrayNode(arr []any) (formulaNode, error) {
	if len(arr) == 0 {
		return nil, fmt.Errorf("formula: empty node")
	}
	head, ok := arr[0].(string)
	if !ok {
		return nil, fmt.Errorf("formula: node head must be a string, got %T", arr[0])
	}
	switch head {
	case "lit":
		if len(arr) != 2 {
			return nil, fmt.Errorf("formula: lit needs exactly 1 value")
		}
		f, err := toFloat(arr[1])
		if err != nil {
			return nil, fmt.Errorf("formula: lit value: %w", err)
		}
		return litNode{v: f}, nil
	case "attr":
		if len(arr) != 2 {
			return nil, fmt.Errorf("formula: attr needs exactly 1 name")
		}
		name, ok := arr[1].(string)
		if !ok {
			return nil, fmt.Errorf("formula: attr name must be a string, got %T", arr[1])
		}
		return attrNode{ref: name}, nil
	case "+", "-", "*", "/", "min", "max", "clamp", "floor", "ceil", "round", "mod", "if":
		args := make([]formulaNode, 0, len(arr)-1)
		for _, a := range arr[1:] {
			child, err := parseFormula(a)
			if err != nil {
				return nil, err
			}
			args = append(args, child)
		}
		return opNode{op: head, args: args}, nil
	default:
		return nil, fmt.Errorf("formula: unknown head %q", logcap.Value(head))
	}
}

// toFloat coerces a decoded scalar to float64 (JSON numbers decode as float64; YAML may give int).
func toFloat(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	default:
		return 0, fmt.Errorf("formula: expected a number, got %T", v)
	}
}
