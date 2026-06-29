package world

import (
	"strings"
	"testing"
)

// lua_fuzz_test.go fuzzes the CONTENT-SCRIPT COMPILE boundary (W6). compileChunk runs arbitrary
// builder-supplied Lua source through the vendored gopher-lua parser (rt.L.Load). Content is a separate
// trust boundary (a builder writes the Lua), and the compile step has NO instruction budget — that only
// guards EXECUTION — so a pathological source (deeply nested, unterminated, control-byte-laden, huge) is
// a compile-time DoS candidate the runtime sandbox tests do not cover. The runtime-invocation robustness
// (no host panic, budget-bounded termination, sandbox isolation) is already pinned by the sandbox
// journey tests (TestRunawayLuaCommandDoesNotWedgeZone, TestPanicInLuaPathRecoversAndZoneServes); this
// target adds the missing compile-path hardening.
//
// Invariants for compileChunk on ANY source:
//   - NO PANIC and no fatal (a malformed/hostile body must FAIL CLOSED, never crash the zone).
//   - Trichotomy holds: empty/whitespace → (nil, nil); otherwise EXACTLY one of (chunk, error) — never
//     both non-nil (you don't get a chunk AND an error), never both nil for non-empty source.
//   - DETERMINISTIC: the same source yields the same compiles-or-not outcome.
//
// NOTE: a single reused runtime is intentional — compileChunk only Loads (parses) source, it does not
// execute or cache, so calls are independent; reuse keeps the fuzzer fast and still exercises the real
// "same VM compiles many bodies" path.
func FuzzLuaCompile(f *testing.F) {
	for _, s := range []string{
		"return 1",
		"while true do end",
		"function f() return f() end",
		"self:say('hello ' .. ev.actor:name())",
		"on('greet', function(ev) self:say('hi') end)",
		"this is not ) valid lua (",
		"local x = '", // unterminated string
		"--[[ unterminated long comment",
		"",
		"   ",
		"\x00\xff invalid bytes \x1b",
		"local t = {" + strings.Repeat("1,", 5000) + "}", // huge table literal
		strings.Repeat("(", 2000),                        // deeply nested open-parens (parser depth)
		strings.Repeat("a+", 5000) + "a",                 // huge expression
		"local 漢字 = 1; return 漢字",                        // non-ASCII identifiers
	} {
		f.Add(s)
	}

	z := newZone("luafuzz")
	rt := z.lua

	f.Fuzz(func(t *testing.T, src string) {
		ch, err := rt.compileChunk("fuzz", src)

		// Never both a chunk AND an error.
		if ch != nil && err != nil {
			t.Fatalf("compileChunk(%q) returned BOTH a chunk and an error: %v", src, err)
		}
		// Empty/whitespace is the inert (nil, nil) no-op; non-empty must resolve to exactly one side.
		if strings.TrimSpace(src) == "" {
			if ch != nil || err != nil {
				t.Fatalf("compileChunk(empty) must be (nil, nil), got (%v, %v)", ch, err)
			}
		} else if ch == nil && err == nil {
			t.Fatalf("compileChunk(%q) returned (nil, nil) for non-empty source", src)
		}

		// Deterministic compiles-or-not.
		ch2, err2 := rt.compileChunk("fuzz", src)
		if (ch == nil) != (ch2 == nil) || (err == nil) != (err2 == nil) {
			t.Fatalf("compileChunk(%q) non-deterministic: (%v,%v) then (%v,%v)", src, ch, err, ch2, err2)
		}
	})
}
