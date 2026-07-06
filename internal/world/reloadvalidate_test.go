package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// TestValidateChannelsDemoClean pins that the SHIPPED demo pack validates clean under the new channel gate
// — the no-false-positive guard against real content (the same "demo pack asserted clean" discipline the
// content lints use).
func TestValidateChannelsDemoClean(t *testing.T) {
	pack, found, err := content.LoadPack(content.DemoPack)
	if err != nil || !found {
		t.Fatalf("load demo pack: found=%v err=%v", found, err)
	}
	if p := validatePacks([]content.Pack{pack}); len(p) != 0 {
		t.Fatalf("demo pack flagged by the pre-publish gate: %v", p)
	}
}

// TestValidateRoomExits covers the #197 slice-2 dangling-exit gate: a resolving intra-zone exit validates;
// an empty target and a dangling INTRA-zone target are rejected; and — the no-false-positive invariant —
// a cross-zone target (whose zone may be out of a scoped reload's scope) is deliberately NOT judged here,
// left to the full-graph slice-3 check.
func TestValidateRoomExits(t *testing.T) {
	room := func(ref string, exits map[string]string) content.RoomDTO {
		return content.RoomDTO{Ref: ref, Exits: exits}
	}

	// Intra-zone exits that resolve validate cleanly (a cross-zone exit is simply not judged here).
	good := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{Ref: "mid", Rooms: []content.RoomDTO{
			room("mid:room:1", map[string]string{"north": "mid:room:2", "east": "wood:room:1"}),
			room("mid:room:2", nil),
		}},
		{Ref: "wood", Rooms: []content.RoomDTO{room("wood:room:1", nil)}},
	}}}
	if p := validateRoomExits(good); len(p) != 0 {
		t.Fatalf("sound room graph flagged: %v", p)
	}

	// An empty exit target is always a dead exit.
	empty := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{Ref: "mid", Rooms: []content.RoomDTO{room("mid:room:1", map[string]string{"down": "  "})}},
	}}}
	if p := validateRoomExits(empty); len(p) != 1 {
		t.Fatalf("empty exit target: want 1 problem, got %v", p)
	}

	// An INTRA-zone target that is absent from its (fully-loaded) zone => definitively dangling.
	dangling := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{Ref: "mid", Rooms: []content.RoomDTO{room("mid:room:1", map[string]string{"north": "mid:room:99"})}},
	}}}
	if p := validateRoomExits(dangling); len(p) != 1 {
		t.Fatalf("dangling intra-zone exit: want 1 problem, got %v", p)
	}

	// NO FALSE POSITIVE: a cross-zone target is NOT judged here — its zone may be a pack outside a scoped
	// reload's scope, so even a target absent from a co-loaded zone is left to the full-graph check.
	crossZone := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{Ref: "mid", Rooms: []content.RoomDTO{room("mid:room:1", map[string]string{"west": "wood:room:99"})}},
		{Ref: "wood", Rooms: []content.RoomDTO{room("wood:room:1", nil)}},
	}}}
	if p := validateRoomExits(crossZone); len(p) != 0 {
		t.Fatalf("cross-zone exit wrongly judged: %v", p)
	}

	// The gate rides validatePacks, so a dangling intra-zone exit blocks a publish.
	if p := validatePacks(dangling); len(p) != 1 {
		t.Fatalf("validatePacks did not surface the dangling exit: %v", p)
	}
}

// TestValidatePacks covers the #192 pre-publish gate: a clean attribute graph validates, a malformed base
// formula and an attribute reference cycle are both reported (so republish blocks the publish). It reuses
// the SAME boot functions (parseAttributeBase + lintAttributeCycles), so "validated" == what boot builds.
func TestValidatePacks(t *testing.T) {
	lit := func(v float64) content.BaseSpecDTO { return content.BaseSpecDTO{Lit: &v} }
	expr := func(e any) content.BaseSpecDTO { return content.BaseSpecDTO{Expr: e} }

	// Valid: a literal base + a derived attribute referencing it (no cycle).
	valid := []content.Pack{{Pack: "p", Attributes: []content.AttributeDTO{
		{Ref: "con", DefaultBase: lit(10)},
		{Ref: "hp", DefaultBase: expr([]any{"*", []any{"attr", "con"}, 10.0})},
	}}}
	if p := validatePacks(valid); len(p) != 0 {
		t.Fatalf("valid pack flagged: %v", p)
	}

	// A base formula that is not a valid node (a bare string) is a parse problem.
	bad := []content.Pack{{Pack: "p", Attributes: []content.AttributeDTO{
		{Ref: "broken", DefaultBase: expr("not-a-node")},
	}}}
	if p := validatePacks(bad); len(p) != 1 {
		t.Fatalf("bad base formula: want 1 problem, got %v", p)
	}

	// An attribute reference cycle a <-> b (would break derived-stat resolution).
	cyc := []content.Pack{{Pack: "p", Attributes: []content.AttributeDTO{
		{Ref: "a", DefaultBase: expr([]any{"attr", "b"})},
		{Ref: "b", DefaultBase: expr([]any{"attr", "a"})},
	}}}
	if p := validatePacks(cyc); len(p) == 0 {
		t.Fatal("attribute cycle not detected")
	}

	// Attributes merge across packs last-write-wins by ref, so a cycle spanning two packs is still caught.
	split := []content.Pack{
		{Pack: "a", Attributes: []content.AttributeDTO{{Ref: "a", DefaultBase: expr([]any{"attr", "b"})}}},
		{Pack: "b", Attributes: []content.AttributeDTO{{Ref: "b", DefaultBase: expr([]any{"attr", "a"})}}},
	}
	if p := validatePacks(split); len(p) == 0 {
		t.Fatal("cross-pack attribute cycle not detected")
	}
}

// TestValidateChannels covers the #197 slice-1 payload gate for channels — the FIRST propagated kind that
// actually hot-swaps on the reload path. A sound channel validates; a ref-less channel, a channel with no
// usable verb, and a format that drops the player's message ($t) are each rejected. It validates through
// the SAME build path boot uses (buildChannelDef defaults + renderChannelFormat), so a rejection means the
// content is definitively dead, not merely degraded.
func TestValidateChannels(t *testing.T) {
	// A well-formed channel: ref, a verb, and a format that carries $t (empty format => default, also OK).
	ok := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
		{Ref: "gossip", Name: "gossip", Words: []string{"gossip", "gos"}, Format: "[$channel] $name: $t"},
		{Ref: "newbie", Name: "newbie", Words: []string{"newbie"}}, // empty format defaults to one with $t
	}}}
	if p := validateChannels(ok); len(p) != 0 {
		t.Fatalf("sound channels flagged: %v", p)
	}

	// A ref-less channel can't be keyed/addressed.
	noRef := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
		{Name: "orphan", Words: []string{"orphan"}},
	}}}
	if p := validateChannels(noRef); len(p) != 1 {
		t.Fatalf("missing ref: want 1 problem, got %v", p)
	}

	// No usable verb word (blanks normalize away) => unreachable channel.
	noVerb := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
		{Ref: "quiet", Name: "quiet", Words: []string{"", "  "}},
	}}}
	if p := validateChannels(noVerb); len(p) != 1 {
		t.Fatalf("no usable verb: want 1 problem, got %v", p)
	}

	// A non-empty format with no $t silently swallows every message.
	dropMsg := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
		{Ref: "void", Name: "void", Words: []string{"void"}, Format: "[$channel] $name says something"},
	}}}
	if p := validateChannels(dropMsg); len(p) != 1 {
		t.Fatalf("message-dropping format: want 1 problem, got %v", p)
	}

	// Channels merge across packs last-write-wins by ref: a later pack repairing the verb clears the defect
	// (only the merged winner is validated).
	repaired := []content.Pack{
		{Pack: "a", Channels: []content.ChannelDTO{{Ref: "gossip", Name: "gossip", Words: []string{""}}}},
		{Pack: "b", Channels: []content.ChannelDTO{{Ref: "gossip", Name: "gossip", Words: []string{"gossip"}}}},
	}
	if p := validateChannels(repaired); len(p) != 0 {
		t.Fatalf("cross-pack repaired channel flagged: %v", p)
	}

	// The channel gate rides validatePacks, so a broken channel blocks a publish just like a bad attribute.
	if p := validatePacks(dropMsg); len(p) != 1 {
		t.Fatalf("validatePacks did not surface the channel defect: %v", p)
	}
}

// TestValidateChannelsSubjectSafety covers the ref char-safety half of the P8-A8 subject-injection
// contract: a ref that builds a malformed/unpublishable NATS subject (whitespace, control byte, wildcard,
// empty dot-token) is rejected, while legit refs — including dotted ones — pass. It also pins that the
// merge keys the RAW ref, so a dead channel can't hide behind a whitespace-variant ref that stays a
// distinct live channel.
func TestValidateChannelsSubjectSafety(t *testing.T) {
	subjectUnsafe := []string{"foo bar", "foo\tbar", "a.>", "wild*", ">", "a..b", "a.", ".b", "ctrl\x01"}
	for _, ref := range subjectUnsafe {
		pk := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
			{Ref: ref, Name: "c", Words: []string{"c"}}, // otherwise sound: verb + default format
		}}}
		if p := validateChannels(pk); len(p) == 0 {
			t.Fatalf("subject-unsafe ref %q not rejected", ref)
		}
	}

	// Legit refs — plain and dotted — must NOT be flagged (no false positives against good content).
	safe := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
		{Ref: "gossip", Name: "gossip", Words: []string{"gossip"}},
		{Ref: "guild.officer", Name: "officer", Words: []string{"officer"}},
	}}}
	if p := validateChannels(safe); len(p) != 0 {
		t.Fatalf("safe refs flagged: %v", p)
	}

	// Raw-ref keying: a dead (verb-less) "gossip" and a valid " gossip" (leading space) stay DISTINCT, so
	// the dead one is still flagged — it can't collapse onto its whitespace-variant sibling. (" gossip" is
	// itself subject-unsafe, so both are flagged; the point is the dead one is NOT silently merged away.)
	rawKey := []content.Pack{
		{Pack: "a", Channels: []content.ChannelDTO{{Ref: "gossip", Name: "gossip", Words: []string{""}}}},
		{Pack: "b", Channels: []content.ChannelDTO{{Ref: " gossip", Name: "gossip", Words: []string{"gossip"}}}},
	}
	if p := validateChannels(rawKey); len(p) < 2 {
		t.Fatalf("raw-ref keying should flag the dead channel AND the subject-unsafe sibling, got: %v", p)
	}
}
