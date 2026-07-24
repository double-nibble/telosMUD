package world

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/textsan"
)

// aliases_test.go — white-box tests for per-character command aliases (#353): the expansion logic
// (cycle/depth/format-string safety), the alias/unalias commands, the trust-gate non-escalation, and
// the durable + handoff round-trips.

// --- expansion logic (pure) -----------------------------------------------------------------

func TestExpandAliasBasic(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Al")
	aliasesOf(s).m["bc"] = "burn corpse"

	// `bc` -> `burn corpse`
	verb, rest, ok := z.expandAlias(s, "bc", "")
	if !ok || verb != "burn" || rest != "corpse" {
		t.Fatalf("expand `bc` = (%q,%q,%v), want (burn, corpse, true)", verb, rest, ok)
	}
	// `bc altar` -> `burn corpse altar` (trailing input appended, Unix-style)
	verb, rest, ok = z.expandAlias(s, "bc", "altar")
	if !ok || verb != "burn" || rest != "corpse altar" {
		t.Fatalf("expand `bc altar` = (%q,%q,%v), want (burn, `corpse altar`, true)", verb, rest, ok)
	}
}

func TestExpandAliasNoAliasIsFastPath(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Al") // no aliases touched -> s.aliases stays nil
	verb, rest, ok := z.expandAlias(s, "look", "north")
	if ok || verb != "look" || rest != "north" {
		t.Fatalf("no-alias expand = (%q,%q,%v), want (look, north, false)", verb, rest, ok)
	}
	if s.aliases != nil {
		t.Fatal("expandAlias must not lazily allocate alias state on the read path")
	}
}

func TestExpandAliasChained(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Al")
	as := aliasesOf(s)
	as.m["x"] = "y foo"
	as.m["y"] = "say"
	// x -> "y foo" -> "say foo"
	verb, rest, ok := z.expandAlias(s, "x", "")
	if !ok || verb != "say" || rest != "foo" {
		t.Fatalf("chained expand = (%q,%q,%v), want (say, foo, true)", verb, rest, ok)
	}
}

// TestExpandAliasCycleTerminates is the load-bearing safety proof: a self-alias and a 2-cycle must
// terminate rather than loop forever. (A regression that dropped the visited set would hang here.)
func TestExpandAliasCycleTerminates(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Al")
	as := aliasesOf(s)
	// self-cycle
	as.m["loop"] = "loop"
	verb, _, _ := z.expandAlias(s, "loop", "")
	if verb != "loop" {
		t.Fatalf("self-cycle expand verb = %q, want loop (resolved as-is)", verb)
	}
	// 2-cycle a<->b
	as.m["a"] = "b"
	as.m["b"] = "a"
	verb, _, _ = z.expandAlias(s, "a", "")
	if verb != "a" && verb != "b" {
		t.Fatalf("2-cycle expand verb = %q, want a or b (bounded)", verb)
	}
}

// TestExpandAliasFormatStringIsData proves an alias whose expansion carries '%'/'$' is treated as data:
// expansion is pure string concatenation, so the verb args carry the tokens verbatim, never as a format
// string (parity with the player having typed the line — act.go never format-interprets an arg).
func TestExpandAliasFormatStringIsData(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Al")
	aliasesOf(s).m["boom"] = "say %n$s100%d"
	verb, rest, ok := z.expandAlias(s, "boom", "")
	if !ok || verb != "say" || rest != "%n$s100%d" {
		t.Fatalf("format-string expand = (%q,%q,%v), want (say, `%%n$s100%%d`, true)", verb, rest, ok)
	}
}

// TestExpandAliasRecapsToMaxLineBytes proves a pathological chained alias set cannot reconstruct a line
// past the MaxLineBytes ingress cap (security review Finding 1): a 2-byte trigger through 64 chained
// ~250-rune bodies must yield an expansion within the cap, not a 16 KB amplification.
func TestExpandAliasRecapsToMaxLineBytes(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Al")
	as := aliasesOf(s)
	body := strings.Repeat("z", 250)
	// a0 -> "a1 <250z>", a1 -> "a2 <250z>", ... aN -> "say <250z>": each hop appends 250 runes to rest.
	for i := 0; i < aliasMaxCount-1; i++ {
		as.m["a"+strconv.Itoa(i)] = "a" + strconv.Itoa(i+1) + " " + body
	}
	as.m["a"+strconv.Itoa(aliasMaxCount-1)] = "say " + body

	verb, rest, ok := z.expandAlias(s, "a0", "")
	if !ok {
		t.Fatal("expected expansion")
	}
	if len(verb)+1+len(rest) > textsan.MaxLineBytes {
		t.Fatalf("expanded line = %d bytes, want <= %d (MaxLineBytes invariant violated)", len(verb)+1+len(rest), textsan.MaxLineBytes)
	}
}

func TestExpandAliasIsCaseInsensitive(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Al")
	aliasesOf(s).m["bc"] = "burn corpse"
	// dispatch lowercases the verb before expandAlias; simulate that.
	verb, _, ok := z.expandAlias(s, strings.ToLower("BC"), "")
	if !ok || verb != "burn" {
		t.Fatalf("case-insensitive expand = (%q,%v), want (burn, true)", verb, ok)
	}
}

// --- commands (integration via the live zone loop) ------------------------------------------

func TestAliasCommandDefineUseListRemove(t *testing.T) {
	z := NewDemoShard().Zone()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	al := newTestPlayerEntity(z, "Al")
	z.post(joinMsg{s: al})
	waitMarkup(t, al, "The Temple Square")

	// Define.
	drain(al)
	z.post(inputMsg{id: "Al", line: "alias greet say hello"})
	waitMarkup(t, al, "Alias set: greet -> say hello")

	// Use it: `greet` -> `say hello`.
	drain(al)
	z.post(inputMsg{id: "Al", line: "greet"})
	waitMarkup(t, al, "You say, 'hello'")

	// Use with trailing input: `greet there` -> `say hello there`.
	drain(al)
	z.post(inputMsg{id: "Al", line: "greet there"})
	waitMarkup(t, al, "You say, 'hello there'")

	// List.
	drain(al)
	z.post(inputMsg{id: "Al", line: "alias"})
	waitMarkup(t, al, "greet -> say hello")

	// Remove, then it stops expanding.
	drain(al)
	z.post(inputMsg{id: "Al", line: "unalias greet"})
	waitMarkup(t, al, "Alias removed: greet")
	drain(al)
	z.post(inputMsg{id: "Al", line: "greet"})
	waitMarkup(t, al, "Huh?") // no longer an alias, and no `greet` verb exists
}

// TestAliasReservedNamesRefused proves the alias-management verbs can never be shadowed, so a player can
// always recover.
func TestAliasReservedNamesRefused(t *testing.T) {
	z := NewDemoShard().Zone()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	al := newTestPlayerEntity(z, "Al")
	z.post(joinMsg{s: al})
	waitMarkup(t, al, "The Temple Square")

	for _, name := range []string{"alias", "unalias"} {
		drain(al)
		z.post(inputMsg{id: "Al", line: "alias " + name + " say pwned"})
		waitMarkup(t, al, "reserved")
		if al.aliases != nil {
			if _, ok := al.aliases.m[name]; ok {
				t.Fatalf("reserved name %q was stored as an alias", name)
			}
		}
	}
}

// TestAliasNoPrivilegeEscalation proves expansion grants no elevation: a mortal aliasing a token to a
// staff verb (`stat`, MinRank rankStaff, hidden) still hits the trust gate on the RESOLVED verb, so the
// staff verb stays invisible and the line falls through to "Huh?" — never runs.
func TestAliasNoPrivilegeEscalation(t *testing.T) {
	z := NewDemoShard().Zone()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	al := newTestPlayerEntity(z, "Al") // baseline tier: a mortal
	z.post(joinMsg{s: al})
	waitMarkup(t, al, "The Temple Square")

	drain(al)
	z.post(inputMsg{id: "Al", line: "alias sneak stat"})
	waitMarkup(t, al, "Alias set: sneak -> stat")

	drain(al)
	z.post(inputMsg{id: "Al", line: "sneak"})
	// The staff verb is invisible to a mortal: the resolved-verb gate nulls it and dispatch falls through
	// to the unknown-verb path. The player must see "Huh?", never a stat sheet.
	waitMarkup(t, al, "Huh?")
}

// --- caps + validation ----------------------------------------------------------------------

func TestAliasCountCapRefusesNewOverLimit(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Al")
	as := aliasesOf(s)
	for i := 0; i < aliasMaxCount; i++ {
		as.m["a"+string(rune('a'+i%26))+string(rune('0'+i/26))] = "say x"
	}
	if len(as.m) != aliasMaxCount {
		t.Fatalf("setup: have %d aliases, want %d", len(as.m), aliasMaxCount)
	}
	// A NEW alias at cap is refused.
	c := &Context{z: z, s: s, Actor: s.entity, arg: "brandnew say y"}
	if err := cmdAlias(c); err != nil {
		t.Fatalf("cmdAlias: %v", err)
	}
	if _, ok := as.m["brandnew"]; ok {
		t.Fatal("a new alias was stored past the count cap")
	}
	// Redefining an EXISTING alias at cap is still allowed.
	existing := ""
	for k := range as.m {
		existing = k
		break
	}
	c = &Context{z: z, s: s, Actor: s.entity, arg: existing + " say redefined"}
	if err := cmdAlias(c); err != nil {
		t.Fatalf("cmdAlias redefine: %v", err)
	}
	if as.m[existing] != "say redefined" {
		t.Fatalf("redefine at cap failed: %q", as.m[existing])
	}
}

func TestAliasBodyLengthCapRefused(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Al")
	long := "say " + strings.Repeat("z", aliasBodyMaxRunes)
	c := &Context{z: z, s: s, Actor: s.entity, arg: "big " + long}
	if err := cmdAlias(c); err != nil {
		t.Fatalf("cmdAlias: %v", err)
	}
	if s.aliases != nil {
		if _, ok := s.aliases.m["big"]; ok {
			t.Fatal("an over-length expansion was stored")
		}
	}
}

// --- persistence round-trips ----------------------------------------------------------------

func TestAliasStateDurableRoundTrip(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Saver")
	as := aliasesOf(s)
	as.m["bc"] = "burn corpse"
	as.m["gg"] = "say good game"

	dumped := dumpAliasState(s)
	if len(dumped) != 2 {
		t.Fatalf("dumpAliasState = %v, want 2 entries", dumped)
	}
	// Mutating the dump must not alias live state.
	dumped["bc"] = "TAMPERED"
	if as.m["bc"] != "burn corpse" {
		t.Fatal("dumpAliasState returned a map aliasing live session state")
	}

	// Round-trip through the durable JSON form.
	dumped2 := dumpAliasState(s)
	raw, err := marshalAliasState(dumped2)
	if err != nil {
		t.Fatalf("marshalAliasState: %v", err)
	}
	m, err := unmarshalAliasState(raw)
	if err != nil {
		t.Fatalf("unmarshalAliasState: %v", err)
	}
	s2 := newTestPlayerEntity(z, "Saver")
	loadAliasState(s2, m)
	if s2.aliases == nil || s2.aliases.m["bc"] != "burn corpse" || s2.aliases.m["gg"] != "say good game" {
		t.Fatalf("alias state did not round-trip: %+v", s2.aliases)
	}
}

func TestAliasStateHandoffJSONRoundTrip(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Walker")
	aliasesOf(s).m["bc"] = "burn corpse"

	raw := dumpAliasStateJSON(s)
	if raw == "" {
		t.Fatal("dumpAliasStateJSON returned empty for a non-empty alias map")
	}
	s2 := newTestPlayerEntity(z, "Walker")
	loadAliasStateJSON(s2, raw)
	if s2.aliases == nil || s2.aliases.m["bc"] != "burn corpse" {
		t.Fatalf("handoff alias carry did not round-trip: %+v", s2.aliases)
	}

	// An all-default player carries "" (omitted), which the destination loads as no aliases.
	empty := newTestPlayerEntity(z, "Bare")
	if got := dumpAliasStateJSON(empty); got != "" {
		t.Fatalf("dumpAliasStateJSON for a bare player = %q, want empty", got)
	}
	dest := newTestPlayerEntity(z, "Bare")
	loadAliasStateJSON(dest, "")
	if dest.aliases != nil {
		t.Fatal("loading an empty handoff carry installed alias state")
	}
}

// TestLoadAliasStateSanitizesForgedSubtree proves the load path re-applies the bounds + reserved-name +
// control-strip guards, so a forged/corrupt snapshot cannot install an oversized, reserved, or
// control-laden alias (defense-in-depth even though the carrier is signed).
func TestLoadAliasStateSanitizesForgedSubtree(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Victim")
	forged := map[string]string{
		"alias":                                  "say pwned",                              // reserved -> dropped
		"ok":                                     "say fine",                               // kept
		strings.Repeat("n", aliasNameMaxRunes+1): "say x",                                  // over-length name -> dropped
		"big":                                    strings.Repeat("z", aliasBodyMaxRunes+1), // over-length body -> dropped
		"ctrl":                                   "say \x07bell",                           // control rune in BODY stripped
		"na\x07me":                               "say noisy",                              // control rune in NAME stripped
		"empty":                                  "",                                       // empty body -> dropped
	}
	loadAliasState(s, forged)
	if s.aliases == nil {
		t.Fatal("loadAliasState installed nothing")
	}
	m := s.aliases.m
	if _, ok := m["alias"]; ok {
		t.Fatal("a reserved name survived the load guard")
	}
	if m["ok"] != "say fine" {
		t.Fatalf("valid alias dropped: %q", m["ok"])
	}
	if _, ok := m[strings.Repeat("n", aliasNameMaxRunes+1)]; ok {
		t.Fatal("an over-length name survived")
	}
	if _, ok := m["big"]; ok {
		t.Fatal("an over-length body survived")
	}
	if _, ok := m["empty"]; ok {
		t.Fatal("an empty-body alias survived")
	}
	if strings.ContainsRune(m["ctrl"], '\x07') {
		t.Fatalf("a control rune survived the load in a body: %q", m["ctrl"])
	}
	// The control rune in the NAME must be stripped too (symmetric with the body). The forged key
	// "na\x07me" installs as "name".
	if _, ok := m["na\x07me"]; ok {
		t.Fatal("a control-laden alias NAME survived the load unstripped")
	}
	if m["name"] != "say noisy" {
		t.Fatalf("control-stripped name did not install cleanly: %q", m["name"])
	}
}
