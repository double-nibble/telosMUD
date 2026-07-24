package world

import (
	"testing"

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
