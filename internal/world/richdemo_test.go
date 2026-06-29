package world

import (
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
)

// richDemoCaster builds a player in the midgaard temple with full mana + a mob target in the room,
// for end-to-end cast smokes against the real demo pack.
func richDemoCaster(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z := newDemoZone("midgaard", newProtoCache())
	s := &session{character: "Caster", out: make(chan *playv1.ServerFrame, 128), epoch: 1}
	z.newPlayerEntity(s, "Caster")
	z.players["Caster"] = s
	Move(s.entity, z.rooms[z.startRoom])
	setAttrBase(s.entity, "intellect", 20) // mana headroom for repeated casts
	setAttrBase(s.entity, "wisdom", 16)    // heal/smite scaling
	setResourceCurrent(s.entity, "mana", resourceMax(s.entity, "mana"))

	mob := makeMobTarget(z, s.entity, "dummy")
	setAttrBase(mob, "dex_save", -100) // always fail saves (deterministic full effect)
	setResourceCurrent(mob, "hp", 200)
	return z, s, mob
}

// richdemo_test.go exercises the RICHER demo content pack (packs/demo.yaml) end to end: it proves the
// new abilities/channels/gear/zones/mobs the content expansion added LOAD and behave through the real
// content pipeline (newDemoZone -> defineGlobals/buildZone). It is a fixture-grade smoke for the
// expansion — every assertion here is "this content is wired and playable", not an engine-mechanism
// test (those live in ability_test/channel_test/combat_test/etc.).

// TestRichDemoAbilitiesRegister proves every new content ability registers as a command verb from the
// embedded demo pack — the whole spell list is reachable by typing the verb.
func TestRichDemoAbilitiesRegister(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	for _, verb := range []string{
		"fireball", "web", // pre-existing
		"frost", "smite", "cure", "bless", "renew", "fortify", "mute", // new declarative
		"lightning", "drain", // new Lua on_resolve
	} {
		if z.abilityForVerb(verb) == nil {
			t.Errorf("demo pack must register the %q command verb", verb)
		}
	}
}

// TestRichDemoRestrictedGuildChannel proves the RESTRICTED guild channel loads with its require_flag
// access predicate and that the predicate gates BOTH speak (canSpeak) and hear (canHear, the 8.6
// receiver filter) — a non-member is denied; granting the flag opens both.
func TestRichDemoRestrictedGuildChannel(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	def := z.channelForVerb("guild")
	if def == nil {
		t.Fatal("demo pack must register the guild channel verb")
	}

	s := newTestPlayerEntity(z, "Outsider")
	// A non-member can neither speak nor hear the restricted channel.
	if def.canSpeak(s.entity) {
		t.Fatal("a non-guildmember must NOT be able to speak on the restricted guild channel")
	}
	if def.canHear(s.entity) {
		t.Fatal("a non-guildmember must NOT be able to hear the restricted guild channel (8.6 hear-filter)")
	}
	// Grant the flag (the documented out-of-band path): both speak and hear open.
	setFlag(s.entity, "guildmember", true)
	if !def.canSpeak(s.entity) {
		t.Fatal("a guildmember must be able to speak on the guild channel after the flag is granted")
	}
	if !def.canHear(s.entity) {
		t.Fatal("a guildmember must be able to hear the guild channel after the flag is granted")
	}
}

// TestRichDemoSmithyGearLoads proves the new gear prototypes load with their components — the
// frostbrand deals COLD (a distinct damage type), the warhammer rolls bigger dice, and the armor
// pieces advertise the body/hands/feet wear slots.
func TestRichDemoSmithyGearLoads(t *testing.T) {
	protos := newProtoCache()
	_ = newDemoZone("midgaard", protos)

	wpnT := func(ref ProtoRef) *Weapon {
		t.Helper()
		p := protos.get(ref)
		if p == nil {
			t.Fatalf("%s: prototype missing", ref)
		}
		w, _ := Get[*Weapon](&Entity{comps: p.comps})
		if w == nil {
			t.Fatalf("%s: missing Weapon component", ref)
		}
		return w
	}

	if fb := wpnT("midgaard:obj:frostbrand"); fb.damageType != "cold" {
		t.Errorf("frostbrand damage type = %q, want cold", fb.damageType)
	}
	if wh := wpnT("midgaard:obj:warhammer"); wh.diceNum != 1 || wh.diceSize != 10 {
		t.Errorf("warhammer dice = %dd%d, want 1d10", wh.diceNum, wh.diceSize)
	}

	// Armor across the three body/hands/feet slots loads as wearable in the right slot.
	for ref, loc := range map[ProtoRef]WearLoc{
		"midgaard:obj:leather-vest":   WearLocBody,
		"midgaard:obj:leather-gloves": WearLocHands,
		"midgaard:obj:leather-boots":  WearLocFeet,
		"midgaard:obj:shield":         WearLocHold,
	} {
		p := protos.get(ref)
		if p == nil {
			t.Errorf("%s: prototype missing", ref)
			continue
		}
		w, _ := Get[*Wearable](&Entity{comps: p.comps})
		if w == nil || !w.canWear(loc) {
			t.Errorf("%s: does not advertise wear slot %v", ref, loc)
		}
	}
}

// TestRichDemoCryptZoneLoads proves the THIRD zone (the crypt) loads with its rooms and combat mobs —
// the multi-zone-per-shard expansion.
func TestRichDemoCryptZoneLoads(t *testing.T) {
	z := newDemoZone("crypt", newProtoCache())
	for _, ref := range []string{"crypt:room:entrance", "crypt:room:ossuary", "crypt:room:tomb"} {
		if z.rooms[ProtoRef(ref)] == nil {
			t.Errorf("crypt zone missing room %s", ref)
		}
	}
	// The crypt entrance's `up` exit returns to the midgaard guild hall (the intra-shard/cross-shard
	// stair). We assert the exit ref is authored (resolution is a shard-hosting concern).
	entrance := z.rooms["crypt:room:entrance"]
	if entrance == nil {
		t.Fatal("crypt entrance missing")
	}
	if entrance.room.exits["up"] != "midgaard:room:guildhall" {
		t.Errorf("crypt entrance up exit = %q, want midgaard:room:guildhall", entrance.room.exits["up"])
	}
}

// TestRichDemoMaxRaisingBuff proves the bear_endurance affect RAISES the hp cap (the resource-clamp
// ordering): applying it lifts max_hp (constitution +4 -> derived max_hp +40), and a full-hp creature
// is NOT clipped — its current hp can rise to the NEW max.
func TestRichDemoMaxRaisingBuff(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newTestPlayerEntity(z, "Bulwark")
	e := s.entity

	maxBefore := resourceMax(e, "hp")
	applyAffect(e, "bear_endurance", attachOpts{duration: 50}, nil)
	maxAfter := resourceMax(e, "hp")
	if maxAfter != maxBefore+40 {
		t.Fatalf("bear_endurance must raise max_hp by 40 (con +4 -> +40 via the derived formula): before %d, after %d",
			maxBefore, maxAfter)
	}
	// The hp resource can now be filled to the RAISED cap (the clamp uses the live max).
	setResourceCurrent(e, "hp", maxAfter)
	if resourceCurrent(e, "hp") != maxAfter {
		t.Fatalf("hp should fill to the raised max %d, got %d", maxAfter, resourceCurrent(e, "hp"))
	}
}

// TestRichDemoReactionMobsCarryLua proves the Counterspell/Shield reaction mobs and the greeter/spider
// load with a Lua trigger block (the per-instance script source survives the content pipeline).
func TestRichDemoReactionMobsCarryLua(t *testing.T) {
	protos := newProtoCache()
	_ = newDemoZone("darkwood", protos)
	_ = newDemoZone("midgaard", protos)
	for _, ref := range []ProtoRef{
		"darkwood:mob:archmage",      // Counterspell (BeforeCastCommit rx:cancel)
		"darkwood:mob:warden",        // Shield (ToHit rx:modify ac)
		"darkwood:mob:spider",        // room-affect web on enter
		"midgaard:mob:quartermaster", // greeter
	} {
		p := protos.get(ref)
		if p == nil {
			t.Errorf("%s: prototype missing", ref)
			continue
		}
		sc, _ := Get[*Scripted](&Entity{comps: p.comps})
		if sc == nil || sc.source == "" {
			t.Errorf("%s: expected a Lua trigger block (*Scripted), got none", ref)
		}
	}
}

// TestRichDemoAffectsAndDamageTypesLoad proves the new affects and damage types register as pack
// globals through the loader.
func TestRichDemoAffectsAndDamageTypesLoad(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	haveAffect := map[string]bool{}
	for _, a := range lc.Affects {
		haveAffect[a.Ref] = true
	}
	for _, ref := range []string{"bless", "bear_endurance", "regen_aura", "frostbite", "silence"} {
		if !haveAffect[ref] {
			t.Errorf("demo pack must define the %q affect", ref)
		}
	}
	haveDmg := map[string]bool{}
	for _, d := range lc.DamageTypes {
		haveDmg[d.Ref] = true
	}
	for _, ref := range []string{"cold", "holy"} {
		if !haveDmg[ref] {
			t.Errorf("demo pack must define the %q damage type", ref)
		}
	}
}

// TestRichDemoDamageSpellsResolve drives the new DAMAGE spells (declarative frost/smite + Lua
// lightning/drain) end to end through dispatch: each must pay mana and deal damage to the target,
// proving the on_resolve op-lists AND the on_resolve_lua bodies execute without error.
func TestRichDemoDamageSpellsResolve(t *testing.T) {
	for _, verb := range []string{"frost", "smite", "lightning", "drain"} {
		t.Run(verb, func(t *testing.T) {
			z, s, mob := richDemoCaster(t)
			manaBefore := resourceCurrent(s.entity, "mana")
			hpBefore := resourceCurrent(mob, "hp")

			z.dispatch(s, verb+" dummy")

			if resourceCurrent(s.entity, "mana") >= manaBefore {
				t.Fatalf("%s must cost mana (before %d, after %d)", verb, manaBefore, resourceCurrent(s.entity, "mana"))
			}
			if resourceCurrent(mob, "hp") >= hpBefore {
				t.Fatalf("%s must deal damage (hp before %d, after %d)", verb, hpBefore, resourceCurrent(mob, "hp"))
			}
		})
	}
}

// TestRichDemoBuffSpellsResolve drives the BUFF/heal/CC spells through dispatch and asserts the
// resulting affect attaches (or hp restores), proving the helpful-disposition apply_affect path and
// the heal op work from the demo pack.
func TestRichDemoBuffSpellsResolve(t *testing.T) {
	// bless / fortify / renew attach a beneficial affect to the caster (self/ally, no target arg).
	for _, tc := range []struct{ verb, affect string }{
		{"bless", "bless"},
		{"fortify", "bear_endurance"},
		{"renew", "regen_aura"},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			z, s, _ := richDemoCaster(t)
			z.dispatch(s, tc.verb) // no target -> self
			if !hasAffect(s.entity, tc.affect) {
				t.Fatalf("%s must apply the %q affect to the caster", tc.verb, tc.affect)
			}
		})
	}

	// cure restores hp (heal op): drop the caster's hp, then cure it back up.
	t.Run("cure", func(t *testing.T) {
		z, s, _ := richDemoCaster(t)
		setResourceCurrent(s.entity, "hp", 1)
		z.dispatch(s, "cure") // no target -> self
		if resourceCurrent(s.entity, "hp") <= 1 {
			t.Fatalf("cure must restore hp, got %d", resourceCurrent(s.entity, "hp"))
		}
	})

	// mute applies the silence CC (prevents the verbal tag) to an enemy.
	t.Run("mute", func(t *testing.T) {
		z, s, mob := richDemoCaster(t)
		manaBefore := resourceCurrent(s.entity, "mana")
		z.dispatch(s, "mute dummy")
		if resourceCurrent(s.entity, "mana") >= manaBefore {
			t.Fatalf("mute did not resolve (no mana spent: before %d, after %d) — target not found?",
				manaBefore, resourceCurrent(s.entity, "mana"))
		}
		if !hasAffect(mob, "silence") {
			t.Fatal("mute must apply the silence affect to the target")
		}
	})
}
