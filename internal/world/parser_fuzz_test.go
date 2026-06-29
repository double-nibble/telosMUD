package world

import (
	"reflect"
	"strings"
	"testing"
)

// parser_fuzz_test.go fuzzes the command-parsing surface (W6). Two complementary targets:
//   - FuzzParseTargetSpec — the PURE targeting-grammar tokenizer (the `2.sword` / `all.coin` form):
//     arbitrary bytes in, bounded + well-formed TargetSpec out, deterministic, never panics.
//   - FuzzDispatch — the FULL player-input ingress: arbitrary bytes routed through z.dispatch (the same
//     path inputMsg takes from the gate) must never panic, whatever verb/args/control-bytes they carry.
// The seed corpus runs hermetically every commit; the long `-fuzz` run goes to the nightly tier.

// FuzzParseTargetSpec pins parseTargetSpec's output invariants on ANY input:
//   - no panic;
//   - the numeric selector is BOUNDED (atoiBounded caps N below 1e6): index ∈ {-1, 0} ∪ [1, 1e6);
//   - every keyword is non-empty, already lower-cased, and whitespace-free (split on space/tab);
//   - keyword count never exceeds the whitespace-delimited word count of the input;
//   - `bare` implies `all` (a bare "all" is the all-in-scope, no-keyword form);
//   - the parse is DETERMINISTIC (same input → DeepEqual spec).
func FuzzParseTargetSpec(f *testing.F) {
	for _, s := range []string{
		"sword", "steel sword", "all.coin", "2.goblin", "all", "0.x", "999999.x", "1000000.x",
		".sword", "a.sword", "  ", "", "ALL.Gold COIN", "\x00\xff sword", "2.", "all.",
		strings.Repeat("kw ", 200),
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, arg string) {
		ts := parseTargetSpec(arg)

		// Numeric selector bounded.
		if ts.index < -1 || ts.index >= 1_000_000 {
			t.Fatalf("parseTargetSpec(%q).index = %d, out of the bounded range [-1, 1e6)", arg, ts.index)
		}
		// Keywords well-formed.
		for _, kw := range ts.keywords {
			if kw == "" {
				t.Fatalf("parseTargetSpec(%q) produced an empty keyword", arg)
			}
			if kw != strings.ToLower(kw) {
				t.Fatalf("parseTargetSpec(%q) keyword %q is not lower-cased", arg, kw)
			}
			if strings.ContainsAny(kw, " \t") {
				t.Fatalf("parseTargetSpec(%q) keyword %q contains whitespace", arg, kw)
			}
		}
		// Keyword count is bounded by the input's word count.
		if n := len(strings.Fields(arg)); len(ts.keywords) > n {
			t.Fatalf("parseTargetSpec(%q) produced %d keywords from %d words", arg, len(ts.keywords), n)
		}
		// bare ⇒ all.
		if ts.bare && !ts.all {
			t.Fatalf("parseTargetSpec(%q) set bare without all", arg)
		}
		// Deterministic.
		if ts2 := parseTargetSpec(arg); !reflect.DeepEqual(ts, ts2) {
			t.Fatalf("parseTargetSpec(%q) not deterministic: %+v vs %+v", arg, ts, ts2)
		}
	})
}

// FuzzDispatch routes an arbitrary input LINE through the full command dispatch and asserts it never
// panics — the player-visible ingress hardening. dispatch is the exact path inputMsg takes from the
// gate, so this exercises verb resolution, abbreviation, every command handler's argument parsing, and
// the targeting/act machinery against hostile bytes (NUL, ESC, invalid UTF-8, giant selectors, the
// `from`/`all.` grammar). A fresh zone+player per exec keeps each input independent; a target mob in the
// room means success paths (kill/get/cast on a real target) are exercised, not only the not-found ones.
// The session's out channel is non-blocking (drops on full), so a long line burst never deadlocks.
func FuzzDispatch(f *testing.F) {
	for _, s := range []string{
		"look", "north", "say hello there", "get all.sword", "kill goblin", "cast fireball goblin",
		"wear helmet", "get 2.coin from chest", "equipment", "inventory", "who", "",
		"   ", "\x00\xff\xfe", "gossip ]\x1b[31m$name: \xffadmin says", "get 999999.x",
		"all.", ".all", "2.", "get . from .", "kill", strings.Repeat("a ", 1000),
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, line string) {
		z, caster := abilityTestZone(t)
		makeMobTarget(z, caster.entity, "goblin") // a real target so kill/cast/get success paths run
		// The assertion is simply: this returns without panicking, for ANY line.
		z.dispatch(caster, line)
	})
}
