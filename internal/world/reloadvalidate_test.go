package world

import (
	"strings"
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
	if p := vPacks([]content.Pack{pack}); len(p) != 0 {
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
	if p := vRoomExits(good); len(p) != 0 {
		t.Fatalf("sound room graph flagged: %v", p)
	}

	// An empty exit target is always a dead exit.
	empty := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{Ref: "mid", Rooms: []content.RoomDTO{room("mid:room:1", map[string]string{"down": "  "})}},
	}}}
	if p := vRoomExits(empty); len(p) != 1 {
		t.Fatalf("empty exit target: want 1 problem, got %v", p)
	}

	// An INTRA-zone target that is absent from its (fully-loaded) zone => definitively dangling.
	dangling := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{Ref: "mid", Rooms: []content.RoomDTO{room("mid:room:1", map[string]string{"north": "mid:room:99"})}},
	}}}
	if p := vRoomExits(dangling); len(p) != 1 {
		t.Fatalf("dangling intra-zone exit: want 1 problem, got %v", p)
	}

	// #205: a cross-zone exit is now judged against the FULL merged graph. A target that EXISTS in another
	// zone resolves (the `good` case above has mid->wood:room:1), but a cross-zone target absent from the WHOLE
	// world is now CAUGHT (previously deferred/invisible to a scoped reload — the exact gap #205 closes).
	crossZoneDangling := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{Ref: "mid", Rooms: []content.RoomDTO{room("mid:room:1", map[string]string{"west": "wood:room:99"})}},
		{Ref: "wood", Rooms: []content.RoomDTO{room("wood:room:1", nil)}},
	}}}
	if p := vRoomExits(crossZoneDangling); len(p) != 1 {
		t.Fatalf("cross-zone exit to a nonexistent room should now be flagged (#205): %v", p)
	}

	// The gate rides validatePacks, so a dangling intra-zone exit blocks a publish.
	if p := vPacks(dangling); len(p) != 1 {
		t.Fatalf("validatePacks did not surface the dangling exit: %v", p)
	}
}

// TestValidateResets covers the #197 slice-2b reset-reference gate: a sound reset validates; an unknown op,
// an undefined/empty target room (resolved against the reset's OWN zone), and an undefined/empty intra-zone
// prototype are each rejected; and — the no-false-positive invariant — a cross-ZONE prototype (which may
// live outside a scoped reload's scope) is deferred, and a spawn into a MOB/container is not judged (the
// `into` target is a runtime lookup). Faithful to applyReset (reset.go).
func TestValidateResets(t *testing.T) {
	// A pack with a room, an item proto and a mob proto, and resets that reference them correctly.
	packWith := func(resets ...content.ResetDTO) []content.Pack {
		return []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{{
			Ref:    "mid",
			Rooms:  []content.RoomDTO{{Ref: "mid:room:1"}},
			Items:  []content.ProtoDTO{{Ref: "mid:obj:torch"}},
			Mobs:   []content.ProtoDTO{{Ref: "mid:mob:guard"}},
			Resets: resets,
		}}}}
	}

	// Sound: both a mob and an item reset resolving to this zone's room + protos.
	good := packWith(
		content.ResetDTO{Op: "spawn_mob", Proto: "mid:mob:guard", Room: "mid:room:1", Max: 1},
		content.ResetDTO{Op: "spawn_item", Proto: "mid:obj:torch", Room: "mid:room:1", Max: 2},
		content.ResetDTO{Op: "", Proto: "mid:obj:torch", Room: "mid:room:1"}, // "" == spawn, valid
	)
	if p := vResets(good); len(p) != 0 {
		t.Fatalf("sound resets flagged: %v", p)
	}

	// Unknown op => a dead reset (applyReset warns "op not understood").
	if p := vResets(packWith(content.ResetDTO{Op: "spawn_dragon", Proto: "mid:mob:guard", Room: "mid:room:1"})); len(p) != 1 {
		t.Fatalf("unknown op: want 1 problem, got %v", p)
	}

	// Target room absent from THIS zone => runtime z.rooms lookup fails.
	if p := vResets(packWith(content.ResetDTO{Op: "spawn_mob", Proto: "mid:mob:guard", Room: "mid:room:99"})); len(p) != 1 {
		t.Fatalf("unknown room: want 1 problem, got %v", p)
	}
	// Empty room.
	if p := vResets(packWith(content.ResetDTO{Op: "spawn_mob", Proto: "mid:mob:guard", Room: "  "})); len(p) != 1 {
		t.Fatalf("empty room: want 1 problem, got %v", p)
	}

	// Undefined intra-zone prototype => runtime z.spawn returns nil.
	if p := vResets(packWith(content.ResetDTO{Op: "spawn_mob", Proto: "mid:mob:ghost", Room: "mid:room:1"})); len(p) != 1 {
		t.Fatalf("undefined proto: want 1 problem, got %v", p)
	}
	// Empty prototype.
	if p := vResets(packWith(content.ResetDTO{Op: "spawn_item", Proto: "", Room: "mid:room:1"})); len(p) != 1 {
		t.Fatalf("empty proto: want 1 problem, got %v", p)
	}

	// #205: a cross-zone reset prototype is now judged against the FULL merged proto graph. One present in
	// another loaded zone resolves (twoZone / crossPack below), but a ref absent from the WHOLE world is now
	// CAUGHT (previously deferred by the ref-prefix heuristic — a gap #205 closes). The out-of-scope deferral is
	// now provenance-driven (see the dedicated #205 scoped tests), not a blanket cross-zone-prefix skip.
	if p := vResets(packWith(content.ResetDTO{Op: "spawn_mob", Proto: "other:mob:x", Room: "mid:room:1"})); len(p) != 1 {
		t.Fatalf("cross-zone proto that exists nowhere should now be flagged (#205): %v", p)
	}

	// A cross-zone proto that IS present in the loaded set (a second zone) resolves — not flagged.
	twoZone := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{
			Ref:    "mid",
			Rooms:  []content.RoomDTO{{Ref: "mid:room:1"}},
			Resets: []content.ResetDTO{{Op: "spawn_mob", Proto: "wood:mob:elf", Room: "mid:room:1"}},
		},
		{Ref: "wood", Mobs: []content.ProtoDTO{{Ref: "wood:mob:elf"}}},
	}}}
	if p := vResets(twoZone); len(p) != 0 {
		t.Fatalf("resolvable cross-zone proto flagged: %v", p)
	}

	// The `into` container target is NOT judged (runtime instance lookup) — neither an intra-zone-shaped nor
	// a garbage/cross-zone `into` blocks, proving the field is skipped regardless of form.
	for _, into := range []string{"mid:obj:nonexistent", "utter garbage", "other:obj:x"} {
		reset := packWith(content.ResetDTO{Op: "spawn_item", Proto: "mid:obj:torch", Room: "mid:room:1", Into: into})
		if p := vResets(reset); len(p) != 0 {
			t.Fatalf("into=%q should not be judged: %v", into, p)
		}
	}

	// Count/Max/Persistent are DELIBERATELY not judged (they affect the spawn ceiling / durable path, not
	// whether a ref resolves): a persistent reset with zero count/max but valid refs validates clean.
	persistent := packWith(content.ResetDTO{Op: "spawn_item", Proto: "mid:obj:torch", Room: "mid:room:1", Persistent: true, Count: 0, Max: 0})
	if p := vResets(persistent); len(p) != 0 {
		t.Fatalf("persistent/zero-count reset with valid refs flagged: %v", p)
	}

	// A reset naming a ROOM ref as its prototype is degenerate but z.spawn RESOLVES it (rooms share the
	// proto cache), so it must NOT be flagged (regression guard for the rooms-in-protoRefs fix).
	roomAsProto := packWith(content.ResetDTO{Op: "spawn_item", Proto: "mid:room:1", Room: "mid:room:1"})
	if p := vResets(roomAsProto); len(p) != 0 {
		t.Fatalf("room-ref-as-proto wrongly flagged (rooms are in the proto cache): %v", p)
	}

	// Cross-PACK proto resolution: pack A's reset references a proto defined in pack B's zone. The shared
	// cache spans packs, so protoRefs is global — it resolves, no flag.
	crossPack := []content.Pack{
		{Pack: "a", Zones: []content.ZoneDTO{{
			Ref:    "mid",
			Rooms:  []content.RoomDTO{{Ref: "mid:room:1"}},
			Resets: []content.ResetDTO{{Op: "spawn_mob", Proto: "wood:mob:elf", Room: "mid:room:1"}},
		}}},
		{Pack: "b", Zones: []content.ZoneDTO{{Ref: "wood", Mobs: []content.ProtoDTO{{Ref: "wood:mob:elf"}}}}},
	}
	if p := vResets(crossPack); len(p) != 0 {
		t.Fatalf("cross-pack proto resolution flagged: %v", p)
	}

	// Multiple defects in one reset are each surfaced — assert on CONTENT (which two), not a bare count.
	multi := vResets(packWith(content.ResetDTO{Op: "spawn_mob", Proto: "mid:mob:ghost", Room: "mid:room:99"}))
	joined := strings.Join(multi, " | ")
	if !strings.Contains(joined, "target room") || !strings.Contains(joined, "not defined") {
		t.Fatalf("multi-defect reset should surface both the room AND the proto problem, got: %v", multi)
	}

	// The gate rides validatePacks, so a broken reset blocks a publish.
	if p := vPacks(packWith(content.ResetDTO{Op: "spawn_mob", Proto: "mid:mob:ghost", Room: "mid:room:1"})); len(p) != 1 {
		t.Fatalf("validatePacks did not surface the reset defect: %v", p)
	}
}

// TestValidateProtoRefs covers the #197 slice-2c prototype-ref-collision gate: distinct refs validate; an
// empty ref, a room/item collision within a zone, and a cross-zone collision are each rejected; and — the
// no-false-positive invariant — a cross-pack WHOLE-ZONE override (both packs define the same zone) is deduped
// like content.Load, so its identical rooms are NOT seen as collisions.
func TestValidateProtoRefs(t *testing.T) {
	// All distinct refs across two zones, all three kinds — clean.
	good := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{
			Ref:   "mid",
			Rooms: []content.RoomDTO{{Ref: "mid:room:1"}, {Ref: "mid:room:2"}},
			Items: []content.ProtoDTO{{Ref: "mid:obj:torch"}},
			Mobs:  []content.ProtoDTO{{Ref: "mid:mob:guard"}},
		},
		{Ref: "wood", Rooms: []content.RoomDTO{{Ref: "wood:room:1"}}},
	}}}
	if p := vProtoRefs(good); len(p) != 0 {
		t.Fatalf("distinct refs flagged: %v", p)
	}

	// An empty ref (here a mob) is unspawnable / collides at "".
	emptyRef := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{Ref: "mid", Mobs: []content.ProtoDTO{{Ref: ""}}},
	}}}
	if p := vProtoRefs(emptyRef); len(p) != 1 {
		t.Fatalf("empty ref: want 1 problem, got %v", p)
	}

	// A room and an item sharing a ref WITHIN one zone collide (both hit the one cache).
	crossKind := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{
			Ref:   "mid",
			Rooms: []content.RoomDTO{{Ref: "mid:x"}},
			Items: []content.ProtoDTO{{Ref: "mid:x"}},
		},
	}}}
	if p := vProtoRefs(crossKind); len(p) != 1 {
		t.Fatalf("cross-kind collision: want 1 problem, got %v", p)
	}

	// Two zones defining the same ref collide in the shared cache.
	crossZone := []content.Pack{{Pack: "p", Zones: []content.ZoneDTO{
		{Ref: "mid", Rooms: []content.RoomDTO{{Ref: "shared:room:1"}}},
		{Ref: "wood", Mobs: []content.ProtoDTO{{Ref: "shared:room:1"}}},
	}}}
	if p := vProtoRefs(crossZone); len(p) != 1 {
		t.Fatalf("cross-zone collision: want 1 problem, got %v", p)
	}

	// NO FALSE POSITIVE: two packs OVERRIDING the same zone (whole-zone last-write-wins, like content.Load)
	// must not have their identical rooms read as collisions — the dedup collapses to the one built zone.
	override := []content.Pack{
		{Pack: "base", Zones: []content.ZoneDTO{{Ref: "mid", Rooms: []content.RoomDTO{{Ref: "mid:room:1"}, {Ref: "mid:room:2"}}}}},
		{Pack: "expansion", Zones: []content.ZoneDTO{{Ref: "mid", Rooms: []content.RoomDTO{{Ref: "mid:room:1"}, {Ref: "mid:room:3"}}}}},
	}
	if p := vProtoRefs(override); len(p) != 0 {
		t.Fatalf("cross-pack whole-zone override wrongly flagged as collision: %v", p)
	}

	// The gate rides validatePacks, so a collision blocks a publish.
	if p := vPacks(crossZone); len(p) != 1 {
		t.Fatalf("validatePacks did not surface the proto collision: %v", p)
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
	if p := vPacks(valid); len(p) != 0 {
		t.Fatalf("valid pack flagged: %v", p)
	}

	// A base formula that is not a valid node (a bare string) is a parse problem.
	bad := []content.Pack{{Pack: "p", Attributes: []content.AttributeDTO{
		{Ref: "broken", DefaultBase: expr("not-a-node")},
	}}}
	if p := vPacks(bad); len(p) != 1 {
		t.Fatalf("bad base formula: want 1 problem, got %v", p)
	}

	// An attribute reference cycle a <-> b (would break derived-stat resolution).
	cyc := []content.Pack{{Pack: "p", Attributes: []content.AttributeDTO{
		{Ref: "a", DefaultBase: expr([]any{"attr", "b"})},
		{Ref: "b", DefaultBase: expr([]any{"attr", "a"})},
	}}}
	if p := vPacks(cyc); len(p) == 0 {
		t.Fatal("attribute cycle not detected")
	}

	// Attributes merge across packs last-write-wins by ref, so a cycle spanning two packs is still caught.
	split := []content.Pack{
		{Pack: "a", Attributes: []content.AttributeDTO{{Ref: "a", DefaultBase: expr([]any{"attr", "b"})}}},
		{Pack: "b", Attributes: []content.AttributeDTO{{Ref: "b", DefaultBase: expr([]any{"attr", "a"})}}},
	}
	if p := vPacks(split); len(p) == 0 {
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
	if p := vChannels(ok); len(p) != 0 {
		t.Fatalf("sound channels flagged: %v", p)
	}

	// A ref-less channel can't be keyed/addressed.
	noRef := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
		{Name: "orphan", Words: []string{"orphan"}},
	}}}
	if p := vChannels(noRef); len(p) != 1 {
		t.Fatalf("missing ref: want 1 problem, got %v", p)
	}

	// No usable verb word (blanks normalize away) => unreachable channel.
	noVerb := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
		{Ref: "quiet", Name: "quiet", Words: []string{"", "  "}},
	}}}
	if p := vChannels(noVerb); len(p) != 1 {
		t.Fatalf("no usable verb: want 1 problem, got %v", p)
	}

	// A non-empty format with no $t silently swallows every message.
	dropMsg := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
		{Ref: "void", Name: "void", Words: []string{"void"}, Format: "[$channel] $name says something"},
	}}}
	if p := vChannels(dropMsg); len(p) != 1 {
		t.Fatalf("message-dropping format: want 1 problem, got %v", p)
	}

	// Channels merge across packs last-write-wins by ref: a later pack repairing the verb clears the defect
	// (only the merged winner is validated).
	repaired := []content.Pack{
		{Pack: "a", Channels: []content.ChannelDTO{{Ref: "gossip", Name: "gossip", Words: []string{""}}}},
		{Pack: "b", Channels: []content.ChannelDTO{{Ref: "gossip", Name: "gossip", Words: []string{"gossip"}}}},
	}
	if p := vChannels(repaired); len(p) != 0 {
		t.Fatalf("cross-pack repaired channel flagged: %v", p)
	}

	// The channel gate rides validatePacks, so a broken channel blocks a publish just like a bad attribute.
	if p := vPacks(dropMsg); len(p) != 1 {
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
		if p := vChannels(pk); len(p) == 0 {
			t.Fatalf("subject-unsafe ref %q not rejected", ref)
		}
	}

	// Legit refs — plain and dotted — must NOT be flagged (no false positives against good content).
	safe := []content.Pack{{Pack: "p", Channels: []content.ChannelDTO{
		{Ref: "gossip", Name: "gossip", Words: []string{"gossip"}},
		{Ref: "guild.officer", Name: "officer", Words: []string{"officer"}},
	}}}
	if p := vChannels(safe); len(p) != 0 {
		t.Fatalf("safe refs flagged: %v", p)
	}

	// Raw-ref keying: a dead (verb-less) "gossip" and a valid " gossip" (leading space) stay DISTINCT, so
	// the dead one is still flagged — it can't collapse onto its whitespace-variant sibling. (" gossip" is
	// itself subject-unsafe, so both are flagged; the point is the dead one is NOT silently merged away.)
	rawKey := []content.Pack{
		{Pack: "a", Channels: []content.ChannelDTO{{Ref: "gossip", Name: "gossip", Words: []string{""}}}},
		{Pack: "b", Channels: []content.ChannelDTO{{Ref: " gossip", Name: "gossip", Words: []string{"gossip"}}}},
	}
	if p := vChannels(rawKey); len(p) < 2 {
		t.Fatalf("raw-ref keying should flag the dead channel AND the subject-unsafe sibling, got: %v", p)
	}
}

// TestValidatePacksTrustLadderReject pins that the reload-broadcast gate HARD-REJECTS a trust-ladder mistake
// that would elevate the wrong accounts fleet-wide (#111): a baseline tier granting a capability. It also
// pins that a WARN-severity finding (an un-grantable flag) does NOT block a reload — the gate only stops the
// Reject-severity ones, matching the boot lint's warn/error split.
func TestValidatePacksTrustLadderReject(t *testing.T) {
	// A baseline ("citizen") granting admin — every un-elevated account would become admin on next login.
	baselineGrant := []content.Pack{{Pack: "evil", TrustTiers: []content.TrustTierDTO{
		{Name: "citizen", Rank: 0, Flags: []string{content.FlagAdmin}},
		{Name: "overlord", Rank: 40, Flags: []string{content.FlagAdmin}},
	}}}
	probs := vPacks(baselineGrant)
	if len(probs) == 0 {
		t.Fatal("a baseline tier granting a capability must block the reload")
	}
	joined := strings.Join(probs, "\n")
	if !strings.Contains(joined, "BASELINE") || !strings.Contains(joined, "evil") {
		t.Fatalf("the rejection must name the pack and the baseline problem, got:\n%s", joined)
	}

	// A duplicate rank also rejects.
	if p := vPacks([]content.Pack{{Pack: "dup", TrustTiers: []content.TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "a", Rank: 30, Flags: []string{content.FlagAdmin}},
		{Name: "b", Rank: 30, Flags: []string{content.FlagAdmin}},
	}}}); len(p) == 0 {
		t.Fatal("a duplicate-rank ladder must block the reload")
	}

	// A WARN-only finding (un-grantable flag on a non-baseline tier) must NOT block the reload.
	warnOnly := []content.Pack{{Pack: "typo", TrustTiers: []content.TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "wizard", Rank: 40, Flags: []string{content.FlagAdmin, "hollylight"}}, // typo, dropped at apply
	}}}
	for _, p := range vPacks(warnOnly) {
		if strings.Contains(p, "hollylight") {
			t.Fatalf("a warn-severity ladder finding must not block a reload, got: %s", p)
		}
	}

	// The demo pack's ladder (or none) must validate clean — no false positive on real content.
	if p := vPacks([]content.Pack{{Pack: "demo", TrustTiers: content.DefaultTrustTiers()}}); len(p) != 0 {
		t.Fatalf("the default ladder must not be rejected, got: %v", p)
	}
}

// TestReloadRejectsOverLongRef (#483): the reload gate hard-rejects an IN-SCOPE pack shipping an identity
// token past RefMaxLen bytes — a ref is a store btree PRIMARY KEY (a token past the ~2704-byte ceiling fails
// the import transaction at runtime) and composes NATS subjects / GMCP keys. The SAME violation in a
// not-reloaded (out-of-scope) pack must NOT block the reload (the in-scope gate, like the charset lint).
func TestReloadRejectsOverLongRef(t *testing.T) {
	longRef := strings.Repeat("a", content.RefMaxLen+1) // charset-clean, one byte over the length bound
	pack := content.Pack{
		Pack:  "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{{Ref: longRef}}}},
	}
	problems := validatePacks([]content.Pack{pack}, map[string]bool{"a": true})
	found := false
	for _, p := range problems {
		if strings.Contains(p, "bytes (max") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a length-specific rejection for the over-long room ref; got: %v", problems)
	}
	// Out-of-scope: a not-reloaded pack's pre-existing over-long ref must not block THIS reload.
	if p := validatePacks([]content.Pack{pack}, map[string]bool{"other": true}); len(p) != 0 {
		t.Fatalf("out-of-scope over-long ref must not block the reload: %v", p)
	}
}

// TestReloadOverLongRefBounded (#483): a 300KB over-long ref's rejection string is capped by the shared
// problems funnel (capProblems, #481) — the length-lint reject can't echo the raw token into a log sink.
func TestReloadOverLongRefBounded(t *testing.T) {
	pack := content.Pack{
		Pack:  "a",
		Zones: []content.ZoneDTO{{Ref: "az", Rooms: []content.RoomDTO{{Ref: strings.Repeat("a", 300*1024)}}}},
	}
	problems := validatePacks([]content.Pack{pack}, map[string]bool{"a": true})
	if len(problems) == 0 {
		t.Fatal("expected a rejection for the 300KB room ref")
	}
	for i, p := range problems {
		if len(p) > boundedFieldMax {
			t.Errorf("problem[%d] not bounded: %d bytes (echoed the raw over-long ref)", i, len(p))
		}
	}
}
