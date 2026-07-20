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
	for _, name := range stringTransforms {
		if fn := raw.RawGetString(name); fn != lua.LNil {
			t.RawSetString(name, b.wrapTransform(fn, "string."+name))
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

// chargeStrAlloc bills n bytes of about-to-be-built string against the CALL's cumulative allocation budget
// (#438), raising the same error the fork's concat charge raises when the call has spent it.
//
// The per-operation StrByteCap checks each wrapper already performs are a different bound and both are
// needed. StrByteCap stops ONE operation being a bomb; it says nothing about ten thousand legal ones, so
// `for i=1,10000 do t[i] = string.rep("x", 1048576) end` is 10 GB of individually-permitted allocations. This
// is what makes the budget a per-CALL bound rather than a per-op one.
//
// Charged BEFORE the allocation, like every other guard here — charging after would bill for memory already
// taken.
func chargeStrAlloc(l *lua.LState, n int, what string) bool {
	if l.ChargeStringBytes(int64(n)) {
		return true
	}
	l.RaiseError("%s: string allocation budget exceeded (cap %d bytes per call). Note that building a string with repeated `..` costs O(n^2) — a 700-line report assembled that way charges megabytes; use table.concat",
		what, StrAllocCap)
	return false
}

// wrapTransform charges the per-call allocation budget for a 1:1 string builtin (lower/upper/reverse/char)
// and delegates. There is NO per-operation cap, deliberately: the result is the size of the input, which is
// already bounded by whatever built it, so one call can never be a bomb. The loop is the bomb — the same
// distinction chargeStrAlloc exists for.
//
// The charge is the INPUT length, which for these four equals the output length (`char`'s output is one byte
// per argument, hence the argument-count fallback). Approximating rather than computing the exact result is
// fine here and not fine for an amplifier: these cannot be off by more than a constant factor.
func (b *builder) wrapTransform(raw lua.LValue, what string) *lua.LFunction {
	return b.L.NewFunction(func(l *lua.LState) int {
		n := l.GetTop() // string.char: one output byte per argument
		if s, ok := l.Get(1).(lua.LString); ok {
			n = len(string(s))
		}
		if !chargeStrAlloc(l, n, what) {
			return 0
		}
		return b.callDelegate(l, raw)
	})
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
	if !chargeStrAlloc(L, len(s)*n, "string.rep") {
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
		width := maxFormatWidth(format)
		if width > StrByteCap {
			l.RaiseError("string.format field width too large (cap %d bytes)", StrByteCap)
			return 0
		}
		// The field WIDTH counts toward the charge, not just toward the per-op cap (#438). "%1000000d"
		// allocates a megabyte from an eleven-byte format string and no arguments, so charging only
		// len(format) plus the %s argument lengths undercharges it by ~100,000x — enough to make the loop
		// version a gigabyte-per-call bomb that bills almost nothing.
		total := len(format) + width
		for i := 2; i <= l.GetTop(); i++ {
			if s, ok := l.Get(i).(lua.LString); ok {
				total += len(string(s))
				if total > StrByteCap {
					l.RaiseError("string.format result too large (cap %d bytes)", StrByteCap)
					return 0
				}
			}
		}
		if !chargeStrAlloc(l, total, "string.format") {
			return 0
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
		// Charged per MATCH rather than per gsub call: `total` above is this gsub's running sum, so billing it
		// would re-charge every earlier match on each new one.
		if s, ok := v.(lua.LString); ok && !chargeStrAlloc(l, len(string(s)), "string.gsub") {
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
		// NOT charged here, deliberately (#438). gopher-lua's tableConcat is implemented ON TOP of the concat
		// opcode's helper, which the fork already charges — so billing it again here charged table.concat
		// exactly 2x (measured), halving the effective budget for the very idiom the docs recommend INSTEAD of
		// the quadratic accumulator. The coupling to the delegate's internals is invisible, so a test asserts
		// table.concat is charged approximately once; if the delegate ever stops routing through the opcode,
		// that test fails rather than the bound silently disappearing.
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
