package world

import (
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// help_test.go — the builder-defined `help` system (#64). The demo pack ships help_defs (help_defs.yaml), so
// newCmdEnv (NewDemoShard) has real topics to browse; the auto-included command index is derived from the base
// command table. These tests exercise: the index, topic resolution (ref/keyword/prefix + precedence over the
// command auto-entry), the command auto-entry fallback, staff-verb hiding, and determinism of a multi-match.

// TestHelpIndex: bare `help` lists the content categories + topics and the auto-included command set, and
// HIDES a staff verb (stat is MinRank+CmdHidden) from a mortal.
func TestHelpIndex(t *testing.T) {
	e := newCmdEnv(t)
	out, _ := e.run("help")
	for _, want := range []string{"Getting Started", "Combat", "Communication", "Commands", "kill", "look", "help", "score"} {
		if !has(out, want) {
			t.Errorf("help index missing %q; got:\n%s", want, strings.Join(out, "\n"))
		}
	}
	// A staff verb (stat: MinRank+CmdHidden) must not appear in a mortal's index.
	if has(out, "stat") {
		t.Errorf("help index leaked a hidden staff verb to a mortal:\n%s", strings.Join(out, "\n"))
	}
}

// TestHelpTopicResolution: a topic is reachable by ref leaf, by a declared keyword, and by a keyword prefix,
// and the rendered topic carries its title + body.
func TestHelpTopicResolution(t *testing.T) {
	e := newCmdEnv(t)
	cases := []struct{ arg, wantBody string }{
		{"combat", "Start a fight"},        // ref leaf (implicit keyword)
		{"fighting", "Start a fight"},      // a declared keyword
		{"comb", "Start a fight"},          // a keyword prefix
		{"communication", "say <message>"}, // another topic by ref leaf
		{"tell", "reply <message>"},        // reach the comms topic by its `tell` keyword
	}
	for _, tc := range cases {
		t.Run(tc.arg, func(t *testing.T) {
			out, _ := e.run("help " + tc.arg)
			if !has(out, tc.wantBody) {
				t.Errorf("help %q missing body %q; got:\n%s", tc.arg, tc.wantBody, strings.Join(out, "\n"))
			}
		})
	}
}

// TestHelpTopicWinsOverCommand: `kill` is BOTH a combat-topic keyword and a command verb. Topic resolution
// runs first, so the rich topic shows — not the minimal command auto-entry.
func TestHelpTopicWinsOverCommand(t *testing.T) {
	e := newCmdEnv(t)
	out, _ := e.run("help kill")
	if !has(out, "Start a fight") {
		t.Errorf("help kill should show the combat topic (keyword match), got:\n%s", strings.Join(out, "\n"))
	}
	if has(out, "No detailed help has been written") {
		t.Errorf("help kill fell through to the command auto-entry despite a topic keyword:\n%s", strings.Join(out, "\n"))
	}
}

// TestHelpCommandAutoEntry: a registered command with no authored topic still gets a minimal help entry (with
// its aliases), and abbreviation resolves — `help sc` documents `score`.
func TestHelpCommandAutoEntry(t *testing.T) {
	e := newCmdEnv(t)
	out, _ := e.run("help score")
	if !has(out, "built-in command") || !has(out, "sc") {
		t.Errorf("help score auto-entry = %s", strings.Join(out, "\n"))
	}
	// Abbreviation: `help sc` resolves to score too.
	out, _ = e.run("help sc")
	if !has(out, "score") {
		t.Errorf("help sc should document score; got:\n%s", strings.Join(out, "\n"))
	}
}

// TestHelpUnknown: a topic that matches neither a content topic nor a visible command reports no help.
func TestHelpUnknown(t *testing.T) {
	e := newCmdEnv(t)
	out, _ := e.run("help zzzznotathing")
	if !has(out, "There is no help on") {
		t.Errorf("help unknown = %s", strings.Join(out, "\n"))
	}
}

// TestHelpStaffVerbHiddenFromMortal: `help <staffverb>` for a mortal is indistinguishable from no help — a
// staff verb's existence never leaks through the help surface (same posture as dispatch's trust gate).
func TestHelpStaffVerbHiddenFromMortal(t *testing.T) {
	e := newCmdEnv(t)
	out, _ := e.run("help stat") // stat is MinRank+CmdHidden
	if !has(out, "There is no help on") {
		t.Errorf("help stat leaked a staff verb to a mortal: %s", strings.Join(out, "\n"))
	}
}

// TestCommandVisible: the shared visibility predicate hides CmdHidden and above-rank staff verbs, and passes
// an ordinary mortal verb.
func TestCommandVisible(t *testing.T) {
	e := newCmdEnv(t)
	s := e.actor // a default mortal (tier "")
	if e.z.commandVisible(&Command{Name: "secret", Flags: CmdHidden}, s) {
		t.Error("a CmdHidden verb should not be visible")
	}
	if e.z.commandVisible(&Command{Name: "wiz", MinRank: rankStaff}, s) {
		t.Error("a staff verb should be hidden from a mortal")
	}
	if !e.z.commandVisible(&Command{Name: "look"}, s) {
		t.Error("an ordinary mortal verb should be visible")
	}
}

// TestResolveHelpDeterministic: when two topics share a keyword, resolution is stable (the smaller ref wins)
// regardless of map iteration order.
func TestResolveHelpDeterministic(t *testing.T) {
	e := newCmdEnv(t)
	e.z.helpDefs().register("help:aaa", buildHelpDef(content.HelpDTO{Ref: "help:aaa", Keywords: []string{"shared"}}))
	e.z.helpDefs().register("help:bbb", buildHelpDef(content.HelpDTO{Ref: "help:bbb", Keywords: []string{"shared"}}))
	for i := 0; i < 20; i++ {
		if def := e.z.resolveHelp("shared"); def == nil || def.ref != "help:aaa" {
			t.Fatalf("resolveHelp(shared) = %v, want help:aaa every time", def)
		}
	}
}

// TestResolveHelpExactBeatsPrefix: an EXACT keyword match wins over a PREFIX match regardless of keyword
// order within a def — a def whose prefix-matching keyword precedes its exact one still classifies as exact.
func TestResolveHelpExactBeatsPrefix(t *testing.T) {
	e := newCmdEnv(t)
	// help:zprefix only PREFIX-matches "combat" (via "combatant"); help:zexact EXACTLY matches it. Refs are
	// chosen so the prefix def sorts FIRST — if bucketing were order-dependent it could win. Exact must win.
	e.z.helpDefs().register("help:zprefix", buildHelpDef(content.HelpDTO{Ref: "help:zprefix", Keywords: []string{"combatant"}}))
	e.z.helpDefs().register("help:zzexact", buildHelpDef(content.HelpDTO{Ref: "help:zzexact", Keywords: []string{"combatant", "combatx"}}))
	// A def whose prefix keyword ("combatant") is listed BEFORE its exact keyword ("combatx"): querying the
	// exact token must still classify it as exact and return it, not the pure-prefix def.
	if def := e.z.resolveHelp("combatx"); def == nil || def.ref != "help:zzexact" {
		t.Fatalf("resolveHelp(combatx) = %v, want help:zzexact (exact must beat prefix)", def)
	}
}

// TestBuildHelpDefImplicitLeafKeyword: the ref leaf becomes an implicit keyword and declared keywords are
// lower-cased + deduped (including a duplicate of the leaf).
func TestBuildHelpDefImplicitLeafKeyword(t *testing.T) {
	def := buildHelpDef(content.HelpDTO{Ref: "help:combat", Keywords: []string{"Fighting", "combat"}})
	want := map[string]bool{"combat": true, "fighting": true}
	if len(def.keywords) != len(want) {
		t.Fatalf("keywords = %v, want the leaf + deduped lower-cased set %v", def.keywords, want)
	}
	for _, kw := range def.keywords {
		if !want[kw] {
			t.Errorf("unexpected keyword %q (not lower-cased/deduped?)", kw)
		}
	}
}

// TestHelpEmptyPackStillHasCommandIndex: a bare zone (no content topics) still renders a usable command index
// from the auto-included base command set — the empty-boot invariant for the help surface.
func TestHelpEmptyPackStillHasCommandIndex(t *testing.T) {
	z := newZone("bare")
	s := newTestPlayerEntity(z, "Solo")
	idx := z.renderHelpIndex(s)
	if !strings.Contains(idx, "Commands") || !strings.Contains(idx, "look") {
		t.Errorf("bare-zone help index missing the auto-included command set:\n%s", idx)
	}
}
