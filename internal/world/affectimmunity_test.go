package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"

	"github.com/stretchr/testify/require"
)

// affectimmunity_test.go covers incoming-affect trait immunity (#538): a target trait rejects an
// incoming affect by its IDENTITY — ref, category, or tag — BEFORE it attaches and before its on_apply
// hook fires. The premise was confirmed worse than filed: applyAffect always attached, OnApplyAffect
// fires post-attach (so on_apply side effects already ran), and the attach-then-strip workaround does
// not even work for a source-scoped affect (opRemoveAffect keys by the victim's own source, not the
// attacker's). So there was no target-side veto at all.

// immunityZone registers a charm (tagged), a poison (categorized), and immunity-granting affects.
func immunityZone(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z, caster := abilityTestZone(t)
	// A charm affect: carries tags, and an on_apply side effect we can observe did NOT run when vetoed.
	z.defs.attr.register("charmed_marker", &attributeDef{ref: "charmed_marker"})
	z.defs.affect.register("charm", &affectDef{
		ref: "charm", name: "Charmed", category: "enchantment", stacking: stackRefresh, maxStacks: 1, duration: 100,
		tags:      []string{"charm", "mind"},
		modifiers: []affectModifier{{attr: "charmed_marker", add: true, value: 1}},
	})
	z.defs.affect.register("fear", &affectDef{
		ref: "fear", name: "Frightened", category: "enchantment", stacking: stackRefresh, maxStacks: 1, duration: 100,
		tags: []string{"fear", "mind"},
	})
	z.defs.affect.register("poison", &affectDef{
		ref: "poison", name: "Poisoned", category: "poison", stacking: stackRefresh, maxStacks: 1, duration: 100,
	})
	// Immunity granters at three granularities.
	z.defs.affect.register("mind_blank", &affectDef{
		ref: "mind_blank", name: "Mind Blank", stacking: stackRefresh, maxStacks: 1, duration: 100,
		grantsImmunity: []string{"charm", "fear"}, // by TAG (charm) and by REF (fear)
	})
	z.defs.affect.register("undead", &affectDef{
		ref: "undead", name: "Undead", stacking: stackRefresh, maxStacks: 1, duration: 100,
		grantsImmunity: []string{"poison", "enchantment"}, // by CATEGORY (poison, enchantment)
	})
	return z, caster, makeMobTarget(z, caster.entity, "goblin")
}

// TestImmunityVetoesByEachGranularity is the core: immunity matches on ref, category, or tag. Each
// subtest builds a target affect whose ref, category, and tags are DISTINCT tokens, and grants
// immunity to exactly ONE of them — so each branch of immuneToAffect is pinned independently (a
// fixture where the same token is both a ref and a tag would let one branch mask another's failure).
func TestImmunityVetoesByEachGranularity(t *testing.T) {
	// target affect "hex": ref=hex, category=curse, tags=[dark, mind]. Three disjoint match tokens.
	build := func(t *testing.T) (*Zone, *Entity) {
		z, caster := abilityTestZone(t)
		z.defs.attr.register("hex_marker", &attributeDef{ref: "hex_marker"})
		z.defs.affect.register("hex", &affectDef{
			ref: "hex", name: "Hexed", category: "curse", stacking: stackRefresh, maxStacks: 1, duration: 100,
			tags:      []string{"dark", "mind"},
			modifiers: []affectModifier{{attr: "hex_marker", add: true, value: 1}},
		})
		return z, makeMobTarget(z, caster.entity, "goblin")
	}
	grant := func(z *Zone, mob *Entity, tokens ...string) {
		z.defs.affect.register("ward", &affectDef{
			ref: "ward", name: "Ward", stacking: stackRefresh, maxStacks: 1, duration: 100,
			grantsImmunity: tokens,
		})
		applyAffect(mob, "ward", attachOpts{}, nil)
	}

	t.Run("by REF only (grant hex; category/tags are not granted)", func(t *testing.T) {
		z, mob := build(t)
		grant(z, mob, "hex")
		require.Nil(t, applyAffect(mob, "hex", attachOpts{}, nil), "vetoed (returns nil)")
		require.False(t, hasAffect(mob, "hex"))
		require.Equal(t, 0.0, attr(mob, "hex_marker"), "and its modifier never took effect")
	})
	t.Run("by CATEGORY only (grant curse; ref/tags are not granted)", func(t *testing.T) {
		z, mob := build(t)
		grant(z, mob, "curse")
		require.False(t, hasAffect(mob, "hex"))
		applyAffect(mob, "hex", attachOpts{}, nil)
		require.False(t, hasAffect(mob, "hex"))
	})
	t.Run("by TAG only (grant dark; ref/category are not granted)", func(t *testing.T) {
		z, mob := build(t)
		grant(z, mob, "dark")
		applyAffect(mob, "hex", attachOpts{}, nil)
		require.False(t, hasAffect(mob, "hex"))
	})
}

// TestImmunityDoesNotBlockUnrelatedAffects pins that immunity is specific: mind_blank (charm/fear) does
// not block a poison, and the non-immune path is byte-for-byte the old behaviour.
func TestImmunityDoesNotBlockUnrelatedAffects(t *testing.T) {
	_, _, mob := immunityZone(t)
	applyAffect(mob, "mind_blank", attachOpts{}, nil)
	require.NotNil(t, applyAffect(mob, "poison", attachOpts{}, nil), "poison is not charm/fear — it lands")
	require.True(t, hasAffect(mob, "poison"))

	// With no immunity at all, a charm attaches normally (the veto is inert absent a grant).
	_, _, mob2 := immunityZone(t)
	require.NotNil(t, applyAffect(mob2, "charm", attachOpts{}, nil))
	require.True(t, hasAffect(mob2, "charm"))
}

// TestVetoSuppressesOnApplySideEffects is the "worse than filed" property: the attach-then-strip
// workaround still fired on_apply. The veto must prevent the on_apply hook (here, the modifier) from
// ever taking effect — proven by the marker attribute staying 0.
func TestVetoSuppressesOnApplySideEffects(t *testing.T) {
	_, _, mob := immunityZone(t)
	applyAffect(mob, "mind_blank", attachOpts{}, nil)
	applyAffect(mob, "charm", attachOpts{}, nil)
	require.Equal(t, 0.0, attr(mob, "charmed_marker"),
		"a vetoed affect's modifier must never apply — attach-then-strip could not guarantee this")
}

// TestImmunityUnwindsOnExpire pins the multiset: when the immunity affect expires, the veto lifts.
func TestImmunityUnwindsOnExpire(t *testing.T) {
	_, _, mob := immunityZone(t)
	applyAffect(mob, "mind_blank", attachOpts{}, nil)
	require.False(t, hasAffect(mob, "charm"))
	applyAffect(mob, "charm", attachOpts{}, nil)
	require.False(t, hasAffect(mob, "charm"), "vetoed while immune")

	a, _ := Get[*Affected](mob)
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		if inst.def.ref == "mind_blank" {
			a.expire(mob, inst, nil)
		}
	}
	require.NotNil(t, applyAffect(mob, "charm", attachOpts{}, nil), "immunity gone -> charm lands")
	require.True(t, hasAffect(mob, "charm"))
}

// TestImmunityMultisetTwoGrants pins that two overlapping grants both have to expire before the veto
// lifts — the reason it is a multiset (count), not a boolean.
func TestImmunityMultisetTwoGrants(t *testing.T) {
	z, _, mob := immunityZone(t)
	// A second, differently-keyed source of charm immunity.
	z.defs.affect.register("amulet", &affectDef{
		ref: "amulet", name: "Amulet", stacking: stackRefresh, maxStacks: 1, duration: 100,
		grantsImmunity: []string{"charm"},
	})
	applyAffect(mob, "mind_blank", attachOpts{}, nil)
	applyAffect(mob, "amulet", attachOpts{}, nil)

	a, _ := Get[*Affected](mob)
	// Expire only mind_blank; amulet still grants charm immunity.
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		if inst.def.ref == "mind_blank" {
			a.expire(mob, inst, nil)
		}
	}
	applyAffect(mob, "charm", attachOpts{}, nil)
	require.False(t, hasAffect(mob, "charm"), "amulet still grants immunity — still vetoed")
}

// TestReattachIsNotVetoed pins the deliberate load-path exemption: a persistence RE-attach installs
// saved state verbatim, so it must NOT be vetoed (else the restored set would depend on the order
// affects load in). This is why the veto sits after the reattach branch.
func TestReattachIsNotVetoed(t *testing.T) {
	_, _, mob := immunityZone(t)
	applyAffect(mob, "mind_blank", attachOpts{}, nil)
	// A live apply is vetoed...
	applyAffect(mob, "charm", attachOpts{}, nil)
	require.False(t, hasAffect(mob, "charm"))
	// ...but a REATTACH (persistence load) of the same charm is installed verbatim.
	require.NotNil(t, applyAffect(mob, "charm", attachOpts{reattach: true, stacks: 1}, nil))
	require.True(t, hasAffect(mob, "charm"), "a persistence reattach is never vetoed (deterministic load)")
}

// TestVetoFiresOnAffectBlocked pins that content can react to a veto: the OnAffectBlocked event fires
// about the immune target with the source as counterpart.
func TestVetoFiresOnAffectBlocked(t *testing.T) {
	z, caster, _ := immunityZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	// A resource on the victim whose OnAffectBlocked handler heals it, so we can observe the event fired.
	z.defs.attr.register("wardcount", &attributeDef{ref: "wardcount"})
	z.defs.res.register("wards", &resourceDef{
		ref: "wards", maxAttr: "max_wards",
		onEvent: map[eventKind][]effectOp{
			evOnAffectBlocked: {{kind: "modify_attribute_base", attr: "wardcount", amount: 1, tgt: "self"}},
		},
	})
	z.defs.attr.register("max_wards", &attributeDef{ref: "max_wards", base: litNode{v: 5}})
	setResourceCurrent(victim.entity, "wards", 5)
	applyAffect(victim.entity, "mind_blank", attachOpts{}, nil)

	applyAffect(victim.entity, "charm", attachOpts{source: caster.entity}, nil)
	require.Equal(t, 1.0, attr(victim.entity, "wardcount"),
		"the OnAffectBlocked event must fire so content can narrate/react to the ward")
}

// TestImmunityGrantIsBenign pins that a grants_immunity affect is a BUFF: it lands ungated on an ally
// (warding is protective) and survives respawn.
func TestImmunityGrantIsBenign(t *testing.T) {
	mind := &affectDef{ref: "mind_blank", grantsImmunity: []string{"charm"}}
	require.False(t, affectIsDetrimental(mind, harmPolarity{}), "granting immunity is protective, a buff")
	require.True(t, affectSurvivesRespawn(mind, harmPolarity{}), "a protective ward survives respawn")
}

// immHarmZone builds a zone with the harm-polarity sets DERIVED, so affectIsDetrimental sees the
// immune-to-a-buff classification (#538 security fix). It registers a benign HoT (regen) and a
// harmful charm, then an immune-to-regen (harmful) and immune-to-charm (benign) grant.
func immHarmZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z, caster := abilityTestZone(t)
	z.defs.affect.register("regen", &affectDef{
		ref: "regen", name: "Regeneration", category: "blessing", stacking: stackRefresh, maxStacks: 1, duration: 100,
		tags: []string{"heal"}, // beneficial: no modifiers/prevents; a HoT
	})
	z.defs.affect.register("charm", &affectDef{
		ref: "charm", name: "Charmed", category: "enchantment", stacking: stackRefresh, maxStacks: 1, duration: 100,
		tags: []string{"charm"}, prevents: []string{"act"}, // harmful: a CC
	})
	z.defs.affect.register("immune_regen", &affectDef{
		ref: "immune_regen", name: "Immune to Regen", stacking: stackRefresh, maxStacks: 1, duration: 100,
		grantsImmunity: []string{"heal"}, // blocks the benign regen -> HARM
	})
	z.defs.affect.register("immune_charm", &affectDef{
		ref: "immune_charm", name: "Immune to Charm", stacking: stackRefresh, maxStacks: 1, duration: 100,
		grantsImmunity: []string{"charm"}, // blocks the harmful charm -> a ward, BENIGN
	})
	// Derive the harm-polarity sets (defineGlobals does this at boot).
	z.defs.harm.harmfulImmunityGrants = harmfulImmunityGrantRefs(z.defs, z.defs.harm)
	return z, caster
}

// TestImmuneToBuffIsClassifiedHarm is the SECURITY fix (#538 F1). A grants_immunity affect that can
// block a BENEFICIAL affect (immune-to-heal) is a debuff — it must be gated cross-player and stripped
// on respawn. An immune-to-charm ward (blocks a harmful affect) stays a buff.
func TestImmuneToBuffIsClassifiedHarm(t *testing.T) {
	z, _ := immHarmZone(t)
	h := z.defs.harm

	require.True(t, h.harmfulImmunityGrants["immune_regen"], "immune-to-a-buff must be derived harmful")
	require.False(t, h.harmfulImmunityGrants["immune_charm"], "immune-to-a-debuff (a ward) stays benign")

	immRegen := z.defs.affect.get("immune_regen")
	immCharm := z.defs.affect.get("immune_charm")
	require.True(t, affectIsDetrimental(immRegen, h), "immune-to-heal is a debuff")
	require.False(t, affectSurvivesRespawn(immRegen, h), "...and must not survive respawn")
	require.False(t, affectIsDetrimental(immCharm, h), "immune-to-charm is a ward, a buff")
	require.True(t, affectSurvivesRespawn(immCharm, h))
}

// TestImmuneToHealIsGatedCrossPlayer is the end-to-end SECURITY property: an attacker applying
// immune-to-heal to a non-consenting player in a no-PvP room must be REFUSED — otherwise they block
// the victim's healing ungated (the reported HIGH hole).
func TestImmuneToHealIsGatedCrossPlayer(t *testing.T) {
	z, caster := immHarmZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, target: victim.entity, mag: 1, disp: dispNeutral}
	require.NoError(t, opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "immune_regen"}))
	require.False(t, hasAffect(victim.entity, "immune_regen"),
		"immune-to-heal is harm — it must be refused at a non-consenting player, not land ungated")

	// The ward (immune-to-charm) is benign, so warding an ally still lands.
	require.NoError(t, opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "immune_charm"}))
	require.True(t, hasAffect(victim.entity, "immune_charm"), "warding an ally against charm still lands")
}

// TestSelfWardCanRefresh pins the abilities-review P2 fix: an affect that grants immunity to a token it
// itself carries must not veto its OWN re-application (else it decays to a one-shot).
func TestSelfWardCanRefresh(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	z.defs.affect.register("spell_shield", &affectDef{
		ref: "spell_shield", name: "Spell Shield", stacking: stackRefresh, maxStacks: 1, duration: 10,
		tags: []string{"magic"}, grantsImmunity: []string{"magic"}, // wards magic AND is tagged magic
	})
	inst := applyAffect(e, "spell_shield", attachOpts{}, nil)
	require.NotNil(t, inst)
	inst.remaining = 3 // simulate decay
	// Re-cast: must REFRESH (not be vetoed by its own magic-immunity).
	inst2 := applyAffect(e, "spell_shield", attachOpts{}, nil)
	require.NotNil(t, inst2, "a self-ward must be able to refresh itself")
	require.Equal(t, 10, inst2.remaining, "the refresh reset the duration")

	// But it STILL wards a DIFFERENT magic affect from a different source.
	z.defs.affect.register("fireball_curse", &affectDef{
		ref: "fireball_curse", stacking: stackRefresh, maxStacks: 1, duration: 100, tags: []string{"magic"},
	})
	require.Nil(t, applyAffect(e, "fireball_curse", attachOpts{}, nil), "a different magic affect is still warded")
}

// TestImmunityDoesNotCleanseExisting pins the abilities-review P4 semantic: immunity blocks NEW
// afflictions, it does not strip one already attached.
func TestImmunityDoesNotCleanseExisting(t *testing.T) {
	_, _, mob := immunityZone(t)
	applyAffect(mob, "charm", attachOpts{}, nil)
	require.True(t, hasAffect(mob, "charm"), "charmed first")
	applyAffect(mob, "mind_blank", attachOpts{}, nil)
	require.True(t, hasAffect(mob, "charm"),
		"gaining immunity does NOT cleanse an existing charm — block-new, not cleanse")
}

// TestVetoThroughTheRealDebuffPath is the abilities-review P5 coverage: drive the veto through
// opApplyAffect -> applyDebuff -> guardHarmful -> applyAffect (the path real harmful content uses), and
// confirm applyDebuff now reports false when the affect was vetoed (the P1 contract fix).
func TestVetoThroughTheRealDebuffPath(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defs.affect.register("mind_blank", &affectDef{
		ref: "mind_blank", stacking: stackRefresh, maxStacks: 1, duration: 100, grantsImmunity: []string{"charm"},
	})
	z.defs.affect.register("charm", &affectDef{
		ref: "charm", category: "enchantment", stacking: stackRefresh, maxStacks: 1, duration: 100,
		tags: []string{"charm"}, prevents: []string{"act"},
	})
	mob := makeMobTarget(z, caster.entity, "goblin")
	applyAffect(mob, "mind_blank", attachOpts{}, nil)

	c := seededCtx(z, caster.entity, mob, dispHarmful)
	landed := applyDebuff(c, mob, "charm", attachOpts{source: caster.entity})
	require.False(t, landed, "applyDebuff must report FALSE when the affect was vetoed by immunity")
	require.False(t, hasAffect(mob, "charm"))

	// The debuff DID land on a non-immune target (control), so applyDebuff's true is real.
	other := makeMobTarget(z, caster.entity, "orc")
	require.True(t, applyDebuff(seededCtx(z, caster.entity, other, dispHarmful), other, "charm", attachOpts{}))
	require.True(t, hasAffect(other, "charm"))
}

// TestHarmfulImmunityGrantDerivation covers each match granularity of the derivation independently
// (ref/category/tag), since a fixture that only matches by tag leaves the ref/category branches
// unpinned.
func TestHarmfulImmunityGrantDerivation(t *testing.T) {
	z, _ := abilityTestZone(t)
	// A benign buff with distinct ref/category/tag tokens.
	z.defs.affect.register("blessing", &affectDef{
		ref: "blessing", category: "holy", stacking: stackRefresh, maxStacks: 1, duration: 100,
		tags: []string{"light"}, // benign: no modifiers/prevents
	})
	// Grants that block it by each granularity.
	z.defs.affect.register("g_ref", &affectDef{ref: "g_ref", grantsImmunity: []string{"blessing"}})
	z.defs.affect.register("g_cat", &affectDef{ref: "g_cat", grantsImmunity: []string{"holy"}})
	z.defs.affect.register("g_tag", &affectDef{ref: "g_tag", grantsImmunity: []string{"light"}})
	// A grant that blocks nothing benign (an unknown token).
	z.defs.affect.register("g_none", &affectDef{ref: "g_none", grantsImmunity: []string{"nonexistent"}})

	got := harmfulImmunityGrantRefs(z.defs, z.defs.harm)
	require.True(t, got["g_ref"], "blocking a benign affect by REF is harm")
	require.True(t, got["g_cat"], "blocking a benign affect by CATEGORY is harm")
	require.True(t, got["g_tag"], "blocking a benign affect by TAG is harm")
	require.False(t, got["g_none"], "granting immunity to nothing that exists is not harm")
}

// TestHarmfulImmunityGrantsPopulatedAtBuild pins the build WIRING: defineGlobals must derive and store
// the set, not leave it nil. Every other harm test builds the set by hand.
func TestHarmfulImmunityGrantsPopulatedAtBuild(t *testing.T) {
	lc := &content.LoadedContent{
		Affects: []content.AffectDTO{
			{Ref: "regen", Category: "blessing", Body: content.AffectBodyDTO{
				Duration: 100, Tags: []string{"heal"},
			}},
			{Ref: "immune_regen", Body: content.AffectBodyDTO{
				Duration: 100, GrantsImmunity: []string{"heal"},
			}},
		},
	}
	d := newDefRegistries()
	defineGlobals(d, lc)
	require.True(t, d.harm.harmfulImmunityGrants["immune_regen"],
		"defineGlobals must derive and store the harmful-immunity-grant set from the shipped affects")

	// And the derived set must reach the harm decision through the real accessor.
	imm := d.affect.get("immune_regen")
	require.True(t, affectIsDetrimental(imm, d.harm))
}
