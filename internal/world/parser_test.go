package world

import (
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// Unit tests for slice 2: command abbreviation (parser.go), the Diku targeting grammar
// (targeting.go), and act() perspective messaging (act.go). They drive the parser/
// resolver/act primitives directly against zone-owned data — no gRPC, no goroutines.

// --- Abbreviation (MUDLIB §6) ---

func TestAbbreviationResolves(t *testing.T) {
	cases := []struct {
		typed string
		want  string // canonical command name, or "" for no match
	}{
		{"north", "north"}, // exact
		{"n", "north"},     // alias / single-letter abbreviation -> direction, not a rarer verb
		{"s", "south"},
		{"e", "east"},
		{"w", "west"},
		{"u", "up"},
		{"d", "down"},
		{"l", "look"}, // alias
		{"lo", "look"},
		{"loo", "look"},
		{"look", "look"},
		{"'", "say"}, // punctuation alias
		{"sa", "say"},
		{"say", "say"},
		{"who", "who"},
		{"wh", "who"}, // "wh" prefixes only "who" -> who
		{"q", "quit"}, // "q" prefixes only "quit"
		{"qu", "quit"},
		// Non-cardinal movement verbs (#360): exact matches resolve to themselves, but they are registered
		// LOW-priority so they never STEAL a shorter command's abbreviation. `o` must stay `open` (the
		// earlier-registered container verb), NOT `out`; `e` stays `east`, not `enter`/`exit`.
		{"enter", "enter"},
		{"exit", "exit"},
		{"out", "out"},
		{"o", "open"},      // #360 regression guard: `out` must NOT steal `o` from `open`
		{"en", "enter"},    // "en" prefixes only enter (was "Huh?" before #360) -> benign new abbreviation
		{"ex", "exit"},     // "ex" prefixes only exit
		{"ou", "out"},      // "ou" prefixes only out
		{"frobnicate", ""}, // unknown
		{"", ""},           // empty
		{"x", ""},          // matches nothing
	}
	for _, tc := range cases {
		cmd, ok := baseTable.resolve(tc.typed)
		if tc.want == "" {
			if ok {
				t.Errorf("resolve(%q) = %q, want no match", tc.typed, cmd.Name)
			}
			continue
		}
		if !ok || cmd.Name != tc.want {
			got := "<none>"
			if ok {
				got = cmd.Name
			}
			t.Errorf("resolve(%q) = %q, want %q", tc.typed, got, tc.want)
		}
	}
}

// TestAbbreviationPriority verifies the documented tie-break: when a prefix matches
// several canonical names, the highest-priority (earliest-registered) command wins. With
// the base table, "w" prefixes both "west" and "who"; "west" is registered first (a
// movement verb), so "w" must resolve to west, never who. This is the "n->north not
// nuke" rule made concrete with the verbs that actually exist.
func TestAbbreviationPriority(t *testing.T) {
	// "w" is an exact alias of west, so it resolves via the exact index; verify the pure
	// prefix path too with a synthetic table where the prefix is ambiguous.
	cmds := []*Command{
		{Name: "west", Run: func(*Context) error { return nil }},
		{Name: "whisper", Run: func(*Context) error { return nil }},
	}
	tbl := newCommandTable(cmds)
	if c, ok := tbl.resolve("w"); !ok || c.Name != "west" {
		t.Fatalf(`resolve("w") = %v, want west (earlier registration wins)`, c)
	}
	// Reverse the registration order: now whisper wins the "w" prefix.
	tbl2 := newCommandTable([]*Command{cmds[1], cmds[0]})
	if c, ok := tbl2.resolve("w"); !ok || c.Name != "whisper" {
		t.Fatalf(`resolve("w") with reversed order = %v, want whisper`, c)
	}
}

// --- Targeting (MUDLIB §7) ---

// addItem spawns a bare item entity with the given keywords and short name into the
// actor's room, returning it. Items have no Living component (so room-item scope sees
// them) and no session.
func addItem(z *Zone, room *Entity, short string, keywords ...string) *Entity {
	e := z.newEntity(ProtoRef("test:obj:" + short))
	e.short = short
	e.keywords = keywords
	Move(e, room)
	return e
}

func TestTargetingGrammar(t *testing.T) {
	z := NewDemoShard().Zone()
	actor := newTestPlayerEntity(z, "Tester")
	room := z.rooms[z.startRoom]
	Move(actor.entity, room)

	sword1 := addItem(z, room, "a long sword", "long", "sword")
	sword2 := addItem(z, room, "a short sword", "short", "sword")
	addItem(z, room, "a gold coin", "gold", "coin")
	addItem(z, room, "a silver coin", "silver", "coin")
	addItem(z, room, "a copper coin", "copper", "coin")

	resolve := func(arg string) []*Entity {
		return z.Resolve(actor.entity, parseTargetSpec(arg), ScopeRoomItems)
	}

	// Unqualified keyword -> first match.
	if got := resolve("sword"); len(got) != 1 || got[0] != sword1 {
		t.Errorf("`sword` = %v, want [sword1]", names(got))
	}
	// N.keyword -> the Nth match.
	if got := resolve("2.sword"); len(got) != 1 || got[0] != sword2 {
		t.Errorf("`2.sword` = %v, want [sword2]", names(got))
	}
	// all.keyword -> every match.
	if got := resolve("all.coin"); len(got) != 3 {
		t.Errorf("`all.coin` = %v, want 3 coins", names(got))
	}
	// bare all -> everything in scope.
	if got := resolve("all"); len(got) != 5 {
		t.Errorf("`all` = %v, want 5 items", names(got))
	}
	// multi-word isname: both words must prefix a keyword.
	if got := resolve("long sword"); len(got) != 1 || got[0] != sword1 {
		t.Errorf("`long sword` = %v, want [sword1]", names(got))
	}
	if got := resolve("sho sw"); len(got) != 1 || got[0] != sword2 {
		t.Errorf("`sho sw` (prefix isname) = %v, want [sword2]", names(got))
	}
	// no match.
	if got := resolve("dragon"); got != nil {
		t.Errorf("`dragon` = %v, want nil", names(got))
	}
}

// TestTargetingEdgeCases exercises the untrusted-input edges: 0.x, a huge N, an empty
// keyword, and an out-of-range N — none may panic, all yield no match cleanly.
func TestTargetingEdgeCases(t *testing.T) {
	z := NewDemoShard().Zone()
	actor := newTestPlayerEntity(z, "Tester")
	room := z.rooms[z.startRoom]
	Move(actor.entity, room)
	addItem(z, room, "a sword", "sword")

	resolve := func(arg string) []*Entity {
		return z.Resolve(actor.entity, parseTargetSpec(arg), ScopeRoomItems)
	}

	if got := resolve("0.sword"); got != nil {
		t.Errorf("`0.sword` = %v, want nil (no zeroth match)", names(got))
	}
	if got := resolve("99.sword"); got != nil {
		t.Errorf("`99.sword` = %v, want nil (out of range)", names(got))
	}
	if got := resolve("9999999999999.sword"); got != nil {
		// Huge N: the selector fails to parse (over the digit cap) and is treated as a
		// keyword "9999999999999.sword", which matches no entity.
		t.Errorf("huge N = %v, want nil", names(got))
	}
	if got := resolve("all."); got != nil {
		// `all.` with empty keyword: bare-all path requires the literal "all" with no dot,
		// so this is all=true with an empty keyword list -> matches nothing.
		t.Errorf("`all.` = %v, want nil", names(got))
	}
	if got := resolve("2."); got != nil {
		t.Errorf("`2.` = %v, want nil", names(got))
	}
	if got := resolve(""); got != nil {
		t.Errorf("empty arg = %v, want nil", names(got))
	}
	// A dotted selector that is neither "all" nor numeric is treated literally (Diku), so
	// it simply finds no keyword "a.sword".
	if got := resolve("a.sword"); got != nil {
		t.Errorf("`a.sword` (literal) = %v, want nil", names(got))
	}
}

// TestScopeOrder verifies scopes are searched in the order the command passes them: a
// room-then-inventory search returns the ground item first.
func TestScopeOrder(t *testing.T) {
	z := NewDemoShard().Zone()
	actor := newTestPlayerEntity(z, "Tester")
	room := z.rooms[z.startRoom]
	Move(actor.entity, room)

	ground := addItem(z, room, "a ground sword", "sword")
	carried := z.newEntity("test:obj:carried")
	carried.short = "a carried sword"
	carried.keywords = []string{"sword"}
	Move(carried, actor.entity) // into inventory

	got := z.Resolve(actor.entity, parseTargetSpec("sword"), ScopeRoomItems, ScopeInventory)
	if len(got) == 0 || got[0] != ground {
		t.Errorf("room-then-inventory `sword` = %v, want ground first", names(got))
	}
	got2 := z.Resolve(actor.entity, parseTargetSpec("sword"), ScopeInventory, ScopeRoomItems)
	if len(got2) == 0 || got2[0] != carried {
		t.Errorf("inventory-then-room `sword` = %v, want carried first", names(got2))
	}
}

// --- act() perspective messaging (MUDLIB §7) ---

// drainOutputs returns every Output markup currently queued on s (non-blocking).
func drainOutputs(s *session) []string {
	var out []string
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil {
				out = append(out, o.GetMarkup())
			}
		default:
			return out
		}
	}
}

func TestActPerspectives(t *testing.T) {
	z := NewDemoShard().Zone()
	room := z.rooms[z.startRoom]

	actor := newTestPlayerEntity(z, "Alice")
	observer := newTestPlayerEntity(z, "Bob")
	actor.out = make(chan *playv1.ServerFrame, 16)
	observer.out = make(chan *playv1.ServerFrame, 16)
	Move(actor.entity, room)
	Move(observer.entity, room)

	// Actor variant: "You ...".
	z.act("You say, '$t'", actor.entity, nil, nil, "hi", "", ToActor)
	if got := drainOutputs(actor); len(got) != 1 || got[0] != "You say, 'hi'" {
		t.Errorf("ToActor = %v, want [\"You say, 'hi'\"]", got)
	}
	if got := drainOutputs(observer); len(got) != 0 {
		t.Errorf("observer saw a ToActor message: %v", got)
	}

	// Room variant: bystander sees the actor's name; the actor sees nothing.
	z.act("$n says, '$t'", actor.entity, nil, nil, "hi", "", ToRoom)
	if got := drainOutputs(observer); len(got) != 1 || got[0] != "Alice says, 'hi'" {
		t.Errorf("ToRoom (observer) = %v, want [\"Alice says, 'hi'\"]", got)
	}
	if got := drainOutputs(actor); len(got) != 0 {
		t.Errorf("actor saw its own ToRoom message: %v", got)
	}
}

// TestActLiteralSubstitution is the security-relevant case: a name or text arg containing
// '%' or '$' must be rendered VERBATIM — never interpreted as a format directive and never
// re-scanned for act() tokens.
func TestActLiteralSubstitution(t *testing.T) {
	z := NewDemoShard().Zone()
	room := z.rooms[z.startRoom]

	// An actor whose name contains format/token metacharacters.
	actor := newTestPlayerEntity(z, "Ev%il$n")
	observer := newTestPlayerEntity(z, "Bob")
	Move(actor.entity, room)
	Move(observer.entity, room)

	// Name substitution: the observer must see the name literally, with $n inside it NOT
	// re-expanded and % NOT consumed.
	z.act("$n waves.", actor.entity, nil, nil, "", "", ToRoom)
	got := drainOutputs(observer)
	if len(got) != 1 || got[0] != "Ev%il$n waves." {
		t.Fatalf("name substitution = %v, want [\"Ev%%il$n waves.\"]", got)
	}

	// Text arg ($t) with metacharacters: rendered verbatim to the actor.
	z.act("You say, '$t'", actor.entity, nil, nil, "100% $n done", "", ToActor)
	got = drainOutputs(actor)
	if len(got) != 1 || got[0] != "You say, '100% $n done'" {
		t.Fatalf("text-arg substitution = %v, want literal", got)
	}

	// $$ renders a single literal '$'. The line's first letter is presentation-capped (Track 1): "cost" -> "Cost".
	z.act("cost is $$5", actor.entity, nil, nil, "", "", ToActor)
	got = drainOutputs(actor)
	if len(got) != 1 || got[0] != "Cost is $5" {
		t.Fatalf("$$ escape = %v, want \"Cost is $5\"", got)
	}
}

// TestActVictimAndObject covers $N (victim) and $p (object) referents and the "You" self
// perspective for a victim who is also a recipient.
func TestActVictimAndObject(t *testing.T) {
	z := NewDemoShard().Zone()
	room := z.rooms[z.startRoom]

	actor := newTestPlayerEntity(z, "Alice")
	victim := newTestPlayerEntity(z, "Bob")
	Move(actor.entity, room)
	Move(victim.entity, room)
	item := addItem(z, room, "a long sword", "long", "sword")

	// Object referent visible to the actor.
	z.act("You give $p to $N.", actor.entity, item, victim.entity, "", "", ToActor)
	if got := drainOutputs(actor); len(got) != 1 || got[0] != "You give a long sword to Bob." {
		t.Fatalf("ToActor with $p/$N = %v", got)
	}
	// Victim sees themselves as "You" via $N.
	z.act("$n gives $p to $N.", actor.entity, item, victim.entity, "", "", ToVictim)
	if got := drainOutputs(victim); len(got) != 1 || got[0] != "Alice gives a long sword to You." {
		t.Fatalf("ToVictim = %v", got)
	}
}

// names maps entities to their short names for readable assertion failures.
func names(es []*Entity) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.short)
	}
	return out
}
