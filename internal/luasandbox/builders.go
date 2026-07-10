package luasandbox

import (
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// builders.go — the capped amplifier wrappers. These bound the OUTPUT (or INPUT) size of the builtins that
// build a whole result inside a single Go builtin with no VM re-entry, so neither the per-instruction budget
// (one op) nor the wall-clock deadline (no between-op check inside the builtin) can interrupt them. Each
// wrapper is a complete PRE-CHECK that rejects an over-cap result before the underlying allocation. Ported
// faithfully from the zone sandbox (internal/world/luart.go); a parity test cross-checks the two.

// buildCappedStringTable returns a fresh `string` table with capped amplifiers + safe passthroughs. The DROP
// entries (notably `dump` — bytecode serialization) are simply never copied.
func (b *builder) buildCappedStringTable(raw *lua.LTable) *lua.LTable {
	L := b.L
	t := L.NewTable()
	for _, name := range stringPassthrough {
		if fn := raw.RawGetString(name); fn != lua.LNil {
			t.RawSetString(name, fn)
		}
	}
	rawFormat := raw.RawGetString("format")
	rawGsub := raw.RawGetString("gsub")
	rawFind := raw.RawGetString("find")
	rawMatch := raw.RawGetString("match")
	rawGmatch := raw.RawGetString("gmatch")

	t.RawSetString("rep", L.NewFunction(b.cappedRep))
	t.RawSetString("format", b.wrapFormat(rawFormat))
	t.RawSetString("gsub", b.wrapGsub(rawGsub))
	t.RawSetString("find", b.wrapPattern(rawFind, "find"))
	t.RawSetString("match", b.wrapPattern(rawMatch, "match"))
	if rawGmatch != lua.LNil {
		t.RawSetString("gmatch", b.wrapPattern(rawGmatch, "gmatch"))
	}
	return t
}

// cappedRep is string.rep with the output-size guard: it rejects an n*#s result over StrByteCap BEFORE the
// underlying allocation, so string.rep("A", 2e9) is a clean error, not a multi-GB allocation.
func (b *builder) cappedRep(L *lua.LState) int {
	s := L.CheckString(1)
	n := L.CheckInt(2)
	if n <= 0 || len(s) == 0 {
		L.Push(lua.LString(""))
		return 1
	}
	if n > StrByteCap/len(s) { // len(s)*n can overflow int; check via division
		L.RaiseError("string.rep result too large (cap %d bytes)", StrByteCap)
		return 0
	}
	L.Push(lua.LString(strings.Repeat(s, n)))
	return 1
}

// wrapFormat caps string.format's OUTPUT before delegating. strFormat builds the whole result in a Go builtin
// with no VM re-entry, so the guard must be a complete pre-check. It bounds three amplification vectors: the
// format string itself, explicit field width/precision tokens, and the SUM of the %s/%q string-argument byte
// lengths.
func (b *builder) wrapFormat(raw lua.LValue) *lua.LFunction {
	return b.L.NewFunction(func(l *lua.LState) int {
		format := l.CheckString(1)
		if len(format) > StrByteCap {
			l.RaiseError("string.format format too large (cap %d bytes)", StrByteCap)
			return 0
		}
		if w := maxFormatWidth(format); w > StrByteCap {
			l.RaiseError("string.format field width too large (cap %d bytes)", StrByteCap)
			return 0
		}
		total := len(format)
		for i := 2; i <= l.GetTop(); i++ {
			if s, ok := l.Get(i).(lua.LString); ok {
				total += len(string(s))
				if total > StrByteCap {
					l.RaiseError("string.format result too large (cap %d bytes)", StrByteCap)
					return 0
				}
			}
		}
		return b.callDelegate(l, raw)
	})
}

// wrapGsub caps string.gsub's OUTPUT before delegating. The raw strGsub builds the whole result in Go with no
// early exit, so the input-length cap alone leaves a matches×replacement alloc bomb. The fix bounds the OUTPUT
// for every replacement kind: a STRING replacement's exact worst case is rejected up front; a FUNCTION/TABLE
// replacement is wrapped in a guarded producer that tracks CUMULATIVE returned bytes and errors the instant
// the running total would exceed StrByteCap.
func (b *builder) wrapGsub(raw lua.LValue) *lua.LFunction {
	return b.L.NewFunction(func(l *lua.LState) int {
		subject := l.CheckString(1)
		if len(subject) > PatternInputCap {
			l.RaiseError("string.gsub input too large (cap %d bytes)", PatternInputCap)
			return 0
		}
		repl := l.CheckAny(3)
		switch r := repl.(type) {
		case lua.LString:
			replLen := len(string(r))
			maxMatches := len(subject) + 1
			if replLen > 0 && maxMatches > (StrByteCap-len(subject))/replLen {
				l.RaiseError("string.gsub result too large (cap %d bytes)", StrByteCap)
				return 0
			}
		case *lua.LFunction:
			b.replaceGsubRepl(l, b.outputGuardedFunc(b.callLuaFuncRepl(r)))
		case *lua.LTable:
			b.replaceGsubRepl(l, b.outputGuardedFunc(b.tableLookupRepl(r)))
		}
		return b.callDelegate(l, raw)
	})
}

// replaceGsubRepl rewrites the gsub call on the stack so argument 3 (the replacement) becomes guarded,
// preserving arguments 1, 2 and any 4 (the limit).
func (b *builder) replaceGsubRepl(l *lua.LState, guarded *lua.LFunction) {
	subj := l.Get(1)
	pat := l.Get(2)
	var limit lua.LValue
	if l.GetTop() >= 4 {
		limit = l.Get(4)
	}
	l.SetTop(0)
	l.Push(subj)
	l.Push(pat)
	l.Push(guarded)
	if limit != nil {
		l.Push(limit)
	}
}

// outputGuardedFunc wraps a per-match replacement producer in a Lua function that tracks the CUMULATIVE bytes
// returned across all matches and raises a clean error the moment the total would exceed StrByteCap — the only
// universal bound for a FUNCTION/TABLE gsub, since per-match values are script-controlled and the raw builtin
// has no early exit. The closure's counter lives for the one gsub call.
func (b *builder) outputGuardedFunc(produce func(l *lua.LState) lua.LValue) *lua.LFunction {
	var total int
	return b.L.NewFunction(func(l *lua.LState) int {
		v := produce(l)
		if s, ok := v.(lua.LString); ok {
			total += len(string(s))
		} else if n, ok := v.(lua.LNumber); ok {
			total += len(lua.LNumber(n).String())
		}
		if total > StrByteCap {
			l.RaiseError("string.gsub result too large (cap %d bytes)", StrByteCap)
			return 0
		}
		l.Push(v)
		return 1
	})
}

// callLuaFuncRepl returns a producer that invokes the script's replacement function with the current match
// arguments (already on the guard's stack) and returns its first result.
func (b *builder) callLuaFuncRepl(fn *lua.LFunction) func(l *lua.LState) lua.LValue {
	return func(l *lua.LState) lua.LValue {
		nargs := l.GetTop()
		l.Push(fn)
		for i := 1; i <= nargs; i++ {
			l.Push(l.Get(i))
		}
		l.Call(nargs, 1)
		ret := l.Get(-1)
		l.Pop(1)
		return ret
	}
}

// tableLookupRepl returns a producer that looks up the script's replacement table by the current match.
func (b *builder) tableLookupRepl(tbl *lua.LTable) func(l *lua.LState) lua.LValue {
	return func(l *lua.LState) lua.LValue {
		key := l.Get(1)
		return l.GetTable(tbl, key)
	}
}

// wrapPattern caps the INPUT subject length of find/match/gmatch (backtracking). These return slices of the
// (input-capped) subject, never an amplified result, so the input cap is the correct bound.
func (b *builder) wrapPattern(raw lua.LValue, name string) *lua.LFunction {
	return b.L.NewFunction(func(l *lua.LState) int {
		subject := l.CheckString(1)
		if len(subject) > PatternInputCap {
			l.RaiseError("string.%s input too large (cap %d bytes)", name, PatternInputCap)
			return 0
		}
		return b.callDelegate(l, raw)
	})
}

// callDelegate forwards the current call's arguments to the wrapped raw function and returns its results — the
// shared tail of the capped wrappers (the guard already ran; now run the genuine implementation).
func (b *builder) callDelegate(l *lua.LState, raw lua.LValue) int {
	nargs := l.GetTop()
	args := make([]lua.LValue, nargs)
	for i := 1; i <= nargs; i++ {
		args[i-1] = l.Get(i)
	}
	base := l.GetTop()
	l.Push(raw)
	for _, a := range args {
		l.Push(a)
	}
	l.Call(nargs, lua.MultRet)
	return l.GetTop() - base
}

// buildCappedTableTable returns a fresh `table` namespace with concat capped and the rest passed through.
func (b *builder) buildCappedTableTable(raw *lua.LTable) *lua.LTable {
	L := b.L
	t := L.NewTable()
	for _, name := range tablePassthrough {
		if fn := raw.RawGetString(name); fn != lua.LNil {
			t.RawSetString(name, fn)
		}
	}
	if fn := raw.RawGetString("unpack"); fn != lua.LNil {
		t.RawSetString("unpack", fn)
	}
	t.RawSetString("concat", b.wrapConcat(raw.RawGetString("concat")))
	return b.readOnly(t)
}

// wrapConcat caps table.concat's output size: it sums the array part's byte lengths (plus separators) and
// rejects an over-cap total BEFORE delegating.
func (b *builder) wrapConcat(raw lua.LValue) *lua.LFunction {
	return b.L.NewFunction(func(l *lua.LState) int {
		tbl := l.CheckTable(1)
		sep := l.OptString(2, "")
		i := l.OptInt(3, 1)
		j := l.OptInt(4, tbl.Len())
		var total int
		for k := i; k <= j; k++ {
			v := tbl.RawGetInt(k)
			switch vv := v.(type) {
			case lua.LString:
				total += len(string(vv))
			case lua.LNumber:
				total += 24 // generous upper bound on a formatted number
			default:
				// a non-string/number lets the real concat error, after this cap check.
			}
			total += len(sep)
			if total > StrByteCap {
				l.RaiseError("table.concat result too large (cap %d bytes)", StrByteCap)
				return 0
			}
		}
		return b.callDelegate(l, raw)
	})
}

// buildMathTable returns a fresh `math` namespace: every safe math function passed through, `random` REBOUND
// to the sandbox RNG (never the os-seeded global), `randomseed` a NO-OP (no entropy reset). Returned read-only.
func (b *builder) buildMathTable(raw *lua.LTable) *lua.LTable {
	L := b.L
	t := L.NewTable()
	raw.ForEach(func(k, v lua.LValue) {
		name, ok := k.(lua.LString)
		if !ok {
			return
		}
		switch string(name) {
		case "random", "randomseed":
			// handled below
		default:
			t.RawSet(k, v)
		}
	})
	t.RawSetString("random", L.NewFunction(b.luaMathRandom))
	t.RawSetString("randomseed", L.NewFunction(func(*lua.LState) int { return 0 }))
	return b.readOnly(t)
}

// luaMathRandom mirrors Lua 5.1 math.random but draws from the sandbox RNG: no args -> [0,1); one arg m ->
// [1,m]; two args m,n -> [m,n].
func (b *builder) luaMathRandom(L *lua.LState) int {
	switch L.GetTop() {
	case 0:
		L.Push(lua.LNumber(b.rng.Float64()))
	case 1:
		m := L.CheckInt(1)
		if m < 1 {
			L.RaiseError("bad argument #1 to 'random' (interval is empty)")
			return 0
		}
		L.Push(lua.LNumber(b.rng.Intn(m) + 1))
	default:
		m := L.CheckInt(1)
		n := L.CheckInt(2)
		if m > n {
			L.RaiseError("bad argument #2 to 'random' (interval is empty)")
			return 0
		}
		L.Push(lua.LNumber(b.rng.Intn(n-m+1) + m))
	}
	return 1
}

// maxFormatWidth scans a printf-style format for the largest explicit numeric field width or precision (e.g.
// the 2000000000 in "%2000000000d"), used to reject a width that would expand a tiny format into a huge
// result in one builtin call. Returns 0 if none.
func maxFormatWidth(format string) int {
	largest := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		for i < len(format) && strings.IndexByte("-+ #0", format[i]) >= 0 {
			i++
		}
		start := i
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			i++
		}
		if i > start {
			w := 0
			for _, c := range format[start:i] {
				w = w*10 + int(c-'0')
				if w > StrByteCap {
					break
				}
			}
			if w > largest {
				largest = w
			}
		}
		if i < len(format) && format[i] == '.' {
			i++
			pstart := i
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				i++
			}
			if i > pstart {
				p := 0
				for _, c := range format[pstart:i] {
					p = p*10 + int(c-'0')
					if p > StrByteCap {
						break
					}
				}
				if p > largest {
					largest = p
				}
			}
		}
	}
	return largest
}
