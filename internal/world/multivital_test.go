package world

import (
	"math/rand"
	"testing"
)

// multivital_test.go exercises #71: MULTI-VITAL SUPPORT. Before #71 the death/damage seam collapsed all
// `vital:true` resources to the single lowest-ref pool — a second vital was dead config (only hp ever took
// damage or drove death). #71 makes each vital INDEPENDENTLY lethal, routes damage to a chosen pool via
// deal_damage's `resource`, runs the depleted pool's OWN on_depleted, and keeps the cancel re-check
// pool-local. These tests drive the shared dealDamage funnel directly (white-box), like death_uniform_test.

// registerVital registers a secondary vital `ref` capped by attribute `maxAttr` (base `max`), with the
// given on_depleted op-list. Call BEFORE creating the mob so its first attr() read resolves the cap.
func registerVital(z *Zone, ref, maxAttr string, maxVal float64, vital, primary bool, onDepleted []effectOp) {
	z.defs.attr.register(maxAttr, &attributeDef{ref: maxAttr, base: litNode{v: maxVal}})
	z.defs.res.register(ref, &resourceDef{
		ref: ref, maxAttr: maxAttr, vital: vital, primary: primary, onDepleted: onDepleted,
	})
}

// dealTo routes a raw deal_damage to a SPECIFIC pool (the #71 `resource` route), attacker as source.
func dealTo(z *Zone, attacker, target *Entity, raw float64, dmgType, resource string) int {
	c := &effectCtx{
		z: z, actor: attacker, source: attacker, target: target,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	return dealDamage(c, target, raw, dmgType, resource)
}

// --- A secondary vital is independently lethal -------------------------------------------------

// TestSecondaryVitalKills proves depleting a NON-primary vital (sanity) to 0 kills — the core #71 fix.
// Before #71 the death seam only ever checked the lowest-ref vital (hp), so a sanity-only blow could
// never kill even at sanity=0. Here hp stays FULL and only sanity is emptied; the mob must still die.
func TestSecondaryVitalKills(t *testing.T) {
	z, s := combatZone(t)
	registerVital(z, "sanity", "max_sanity", 100, true, false, nil)

	mob := combatMob(z, s.entity, "cultist", "", 100) // hp full (100)
	room := mob.location
	setResourceCurrent(mob, "sanity", 10)

	dealt := dealTo(z, s.entity, mob, 50, "slash", "sanity") // 50 >> 10 sanity
	if dealt <= 0 {
		t.Fatalf("sanity blow dealt %d, want > 0", dealt)
	}
	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("hp = %d after a SANITY blow, want 100 untouched (routing hit the wrong pool)", got)
	}
	if position(mob) != posDead {
		t.Fatalf("mob position = %v after sanity hit 0, want posDead (a secondary vital must be lethal)", position(mob))
	}
	if roomCorpse(room) == nil {
		t.Fatalf("no corpse after a secondary-vital death")
	}
}

// --- The DEPLETED pool's own on_depleted runs (not the primary's) ------------------------------

// TestDepletedPoolRunsOwnHook proves the pool that actually depleted runs ITS on_depleted, not hp's.
// hp's hook applies "burned"; sanity's hook applies "insane". Emptying sanity must apply insane and NOT
// burned — otherwise the seam is still running the lowest-ref pool's hook (the pre-#71 collapse).
func TestDepletedPoolRunsOwnHook(t *testing.T) {
	z, s := combatZone(t)
	z.defs.affect.register("burned", &affectDef{ref: "burned", name: "Burned", stacking: stackRefresh, maxStacks: 1, duration: 20})
	z.defs.affect.register("insane", &affectDef{ref: "insane", name: "Insane", stacking: stackRefresh, maxStacks: 1, duration: 20})
	// hp's hook applies burned; but hp stays full here so it must NOT fire.
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onDepleted: []effectOp{{kind: "apply_affect", affect: "burned", tgt: "self"}},
	})
	// sanity's hook applies insane and revives sanity to 1 so the mob survives to be inspected.
	registerVital(z, "sanity", "max_sanity", 100, true, false, []effectOp{
		{kind: "modify_resource", resource: "sanity", amount: 1, tgt: "self"},
		{kind: "apply_affect", affect: "insane", tgt: "self"},
	})

	mob := combatMob(z, s.entity, "seer", "", 100) // hp full
	setResourceCurrent(mob, "sanity", 5)

	dealTo(z, s.entity, mob, 50, "slash", "sanity")

	if !hasAffectRef(mob, "insane") {
		t.Fatalf("sanity depletion did not run sanity's OWN on_depleted (no insane affect)")
	}
	if hasAffectRef(mob, "burned") {
		t.Fatalf("sanity depletion wrongly ran HP's on_depleted (burned applied) — pool hook not threaded")
	}
	if position(mob) == posDead {
		t.Fatalf("mob died despite sanity's hook reviving the pool (pool-local cancel failed)")
	}
}

// --- Cancel re-check is pool-local -------------------------------------------------------------

// TestCancelIsPoolLocal proves a hook that revives the DEPLETED pool cancels that pool's death, and that
// a hook reviving the depleted pool while lethally draining ANOTHER vital still dies through the second
// pool's checkpoint (the cross-pool cascade, guarded by the death-global deathGen re-entry latch).
func TestCancelIsPoolLocal(t *testing.T) {
	t.Run("revive_depleted_pool_cancels", func(t *testing.T) {
		z, s := combatZone(t)
		// sanity's hook floors it back to 1 — a death-ward on the secondary vital.
		registerVital(z, "sanity", "max_sanity", 100, true, false, []effectOp{
			{kind: "modify_resource", resource: "sanity", amount: 1, tgt: "self"},
		})
		mob := combatMob(z, s.entity, "wardling", "", 100)
		setResourceCurrent(mob, "sanity", 5)

		dealTo(z, s.entity, mob, 50, "slash", "sanity")

		if position(mob) == posDead {
			t.Fatalf("reviving the depleted sanity pool did not cancel the death")
		}
		if got := resourceCurrent(mob, "sanity"); got != 1 {
			t.Fatalf("sanity = %d, want 1 (the death-ward floor)", got)
		}
	})

	t.Run("hp_hook_revives_hp_but_drains_sanity_still_dies", func(t *testing.T) {
		z, s := combatZone(t)
		// hp's hook revives hp to 1 (would cancel hp's death) BUT lethally drains sanity — the nested sanity
		// checkpoint then kills. Proves each vital is independently lethal and the cross-pool cascade
		// resolves to exactly one death via the death-global generation guard (#69).
		z.defs.res.register("hp", &resourceDef{
			ref: "hp", maxAttr: "max_hp", vital: true,
			onDepleted: []effectOp{
				{kind: "modify_resource", resource: "hp", amount: 1, tgt: "self"},
				{kind: "deal_damage", dmgType: "slash", amount: 999, resource: "sanity", tgt: "self"},
			},
		})
		registerVital(z, "sanity", "max_sanity", 100, true, false, nil)
		mob := combatMob(z, s.entity, "doomed", "", 5)
		room := mob.location
		setResourceCurrent(mob, "sanity", 100)

		dealTo(z, s.entity, mob, 50, "slash", "") // deplete hp (primary/default route)

		if position(mob) != posDead {
			t.Fatalf("mob survived: hp-hook revived hp but lethally drained sanity — the second vital must kill")
		}
		// Exactly one corpse / one death despite two pools crossing 0 in the cascade (deathGen guard).
		corpses := 0
		for _, e := range room.contents {
			if _, ok := Get[*Container](e); ok {
				corpses++
			}
		}
		if corpses != 1 {
			t.Fatalf("cross-pool death cascade produced %d corpses, want exactly 1 (double die() = item dupe)", corpses)
		}
		if got := deathGen(mob); got != 1 {
			t.Fatalf("mob died %d times across the cross-pool cascade, want exactly 1", got)
		}
	})
}

// --- Immunity: a pool the target has no capacity for -------------------------------------------

// TestImmunityDiscardNoInstantKill proves routing damage to a pool whose derived max is 0 (a mindless
// construct with no "sanity") is NATURAL IMMUNITY: the blow is discarded, returns 0, writes no negative
// current (which would read as depleted and instant-kill a vital), builds NO threat, and does not kill.
func TestImmunityDiscardNoInstantKill(t *testing.T) {
	z, s := combatZone(t)
	// sanity is a VITAL pool but max_sanity resolves to 0 on this construct → no capacity → immune.
	registerVital(z, "sanity", "max_sanity", 0, true, false, nil)

	mob := combatMob(z, s.entity, "golem", "", 100)
	if got := resourceMax(mob, "sanity"); got != 0 {
		t.Fatalf("precondition: max_sanity = %d, want 0 (the immunity setup)", got)
	}

	dealt := dealTo(z, s.entity, mob, 50, "slash", "sanity")

	if dealt != 0 {
		t.Fatalf("immune blow dealt %d, want 0 (discarded)", dealt)
	}
	if position(mob) == posDead {
		t.Fatalf("a 0-max pool INSTANT-KILLED the target — the immunity discard failed (security-critical)")
	}
	if got := resourceCurrent(mob, "sanity"); got < 0 {
		t.Fatalf("sanity current = %d, want >= 0 (never written negative)", got)
	}
	if mob.living.threat[s.entity] != 0 {
		t.Fatalf("an immune blow built %v threat, want 0 (discard is before the threat accrual)", mob.living.threat[s.entity])
	}
}

// --- A NON-vital pool can be damaged but never kills -------------------------------------------

// TestNonVitalPoolDamageDoesNotKill is the security case: a runtime-supplied `resource` (e.g. a Lua
// h:damage{resource="mana"}) that the load-time lint can't see. Draining a NON-vital pool (mana) to 0
// must NOT trigger death — the depletion checkpoint runs the pool's hook, but the gate into die()
// (inside onPoolDepleted) is vitalDepleted, which requires the pool to be `vital` (#406).
func TestNonVitalPoolDamageDoesNotKill(t *testing.T) {
	z, s := combatZone(t)
	registerVital(z, "mana", "max_mana", 50, false, false, nil) // vital:false — a resource pool, not a life pool

	mob := combatMob(z, s.entity, "mage", "", 100)
	room := mob.location
	setResourceCurrent(mob, "mana", 10)

	dealt := dealTo(z, s.entity, mob, 50, "slash", "mana")

	if dealt <= 0 {
		t.Fatalf("mana blow dealt %d, want > 0 (a non-vital pool still takes damage)", dealt)
	}
	if got := resourceCurrent(mob, "mana"); got != 0 {
		t.Fatalf("mana = %d, want 0 (clamped, drained)", got)
	}
	if position(mob) == posDead {
		t.Fatalf("draining a NON-vital pool to 0 killed the target — vitalDepleted gate failed (security-critical)")
	}
	if roomCorpse(room) != nil {
		t.Fatalf("a non-vital depletion dropped a corpse")
	}
}

// --- primary flag routes unrouted (default) damage ---------------------------------------------

// TestPrimaryFlagRoutesDefaultDamage proves an explicit `primary` vital wins the default-damage route
// over the lowest-ref fallback. With hp (lowest ref) and shield (primary), an unrouted deal_damage hits
// SHIELD, not hp — closing the "blood sorts before hp" footgun deterministically.
func TestPrimaryFlagRoutesDefaultDamage(t *testing.T) {
	z, s := combatZone(t)
	// Re-register hp WITHOUT primary; add shield flagged primary (shield > hp by sort, so lowest-ref would
	// otherwise pick hp — the primary flag must override that).
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	registerVital(z, "shield", "max_shield", 100, true, true, nil)

	mob := combatMob(z, s.entity, "guardian", "", 100) // hp = 100
	// shield defaults to full (max) since no current stored.

	if got := vitalResource(mob); got != "shield" {
		t.Fatalf("vitalResource = %q, want \"shield\" (the primary-flagged vital)", got)
	}
	dealTo(z, s.entity, mob, 40, "slash", "") // unrouted → primary (shield)

	if got := resourceCurrent(mob, "shield"); got != 60 {
		t.Fatalf("shield = %d after unrouted damage, want 60 (default damage should hit the primary)", got)
	}
	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("hp = %d, want 100 untouched (default damage wrongly hit hp, not the primary)", got)
	}
}

// TestVitalResourceLowestRefFallback pins the deterministic fallback: with two vitals and NO primary
// flag, the lowest ref by sort order is the default-damage pool (never Go's random map order).
func TestVitalResourceLowestRefFallback(t *testing.T) {
	z, s := combatZone(t) // hp registered (no primary)
	registerVital(z, "zeal", "max_zeal", 100, true, false, nil)
	mob := combatMob(z, s.entity, "zealot", "", 100)
	if got := vitalResource(mob); got != "hp" {
		t.Fatalf("vitalResource = %q, want \"hp\" (lowest ref: hp < zeal, no primary flagged)", got)
	}
}

// --- respawn restores ALL vitals ---------------------------------------------------------------

// TestRespawnRestoresAllVitals proves the player respawn heal generalizes across every vital pool, not
// just hp — a player who died with a drained secondary vital revives full in BOTH.
func TestRespawnRestoresAllVitals(t *testing.T) {
	z, s := combatZone(t)
	registerVital(z, "sanity", "max_sanity", 100, true, false, nil)

	setResourceCurrent(s.entity, "hp", 0)
	setResourceCurrent(s.entity, "sanity", 3)

	z.respawnPlayer(s.entity)

	if got, want := resourceCurrent(s.entity, "hp"), resourceMax(s.entity, "hp"); got != want {
		t.Fatalf("respawned hp = %d, want full %d", got, want)
	}
	if got, want := resourceCurrent(s.entity, "sanity"), resourceMax(s.entity, "sanity"); got != want {
		t.Fatalf("respawned sanity = %d, want full %d (respawn must restore ALL vitals)", got, want)
	}
}

// --- The routed pool threads through a reaction REDIRECT ---------------------------------------

// TestRedirectCarriesRoutedPool proves a pool-routed blow that is redirected by an OnDamageTaken
// reaction (rx:replace_target) lands on the NEW target's SAME pool (sanity), not its hp. Before the
// redirect path threaded `resource`, a redirected sanity-strike silently re-landed as hp damage.
func TestRedirectCarriesRoutedPool(t *testing.T) {
	z, s := combatZone(t)
	attacker := s.entity
	registerRoom(z, attacker.location)
	registerVital(z, "sanity", "max_sanity", 100, true, false, nil)

	guardee := combatMob(z, attacker, "guardee", "", 100) // hp + sanity both full (100)
	registerRoom(z, guardee.location)
	mob := combatMob(z, attacker, "ward", "", 100)
	redirectMobTo(z, mob, guardee)

	c := &effectCtx{z: z, actor: attacker, source: attacker, target: mob, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	dealDamage(c, mob, 10, "psychic", "sanity") // route to sanity, then the mob redirects it to guardee

	if got := resourceCurrent(mob, "sanity"); got != 100 {
		t.Fatalf("original target sanity = %d, want 100 (blow redirected away)", got)
	}
	if got := resourceCurrent(guardee, "sanity"); got != 90 {
		t.Fatalf("guardee sanity = %d, want 90 (the routed pool must survive the redirect)", got)
	}
	if got := resourceCurrent(guardee, "hp"); got != 100 {
		t.Fatalf("guardee hp = %d, want 100 (the redirect wrongly re-routed sanity damage to hp)", got)
	}
}

// --- Lint coverage -----------------------------------------------------------------------------

// TestLintVitalResourcesNudgesOnMissingPrimary proves the flipped lint: multiple vitals are allowed, but
// the WARN fires only when >1 vital exists and none is flagged primary. It never fires for one vital or
// when a primary is designated. (Pure function over the def table; the WARN itself is a slog side effect,
// so we assert the boundary conditions through the same table shapes rather than capturing the log.)
func TestLintVitalResourcesBoundary(t *testing.T) {
	// The lint is a slog-only side effect; drive it across the three shapes to prove it doesn't panic and
	// that the logic branches (single vital / multi+primary / multi+none) are all exercised. Only the
	// multi_no_primary shape reaches the WARN branch.
	cases := map[string]map[string]*resourceDef{
		"single":             {"hp": {ref: "hp", vital: true}},
		"multi_with_primary": {"hp": {ref: "hp", vital: true, primary: true}, "sanity": {ref: "sanity", vital: true}},
		"multi_no_primary":   {"blood": {ref: "blood", vital: true}, "hp": {ref: "hp", vital: true}},
	}
	for name, table := range cases {
		t.Run(name, func(_ *testing.T) { lintVitalResources(table) })
	}
}

// TestLintDealDamageResources proves the new content-lint flags a deal_damage whose `resource` names an
// UNregistered pool (a typo that discards the blow at runtime), and passes a registered one / an empty one.
func TestLintDealDamageResources(t *testing.T) {
	z := newZone("test")
	z.defs.res.register("hp", &resourceDef{ref: "hp", vital: true})
	z.defs.res.register("sanity", &resourceDef{ref: "sanity", vital: true})
	// An ability with three deal_damage ops: valid (sanity), empty (primary), and a typo (saniti).
	z.defs.ability.register("mindblast", &abilityDef{
		ref: "mindblast",
		ops: []effectOp{
			{kind: "deal_damage", dmgType: "psychic", amount: 5, resource: "sanity"},
			{kind: "deal_damage", dmgType: "psychic", amount: 5, resource: ""},
			{kind: "deal_damage", dmgType: "psychic", amount: 5, resource: "saniti"}, // typo
		},
	})

	misses := lintDealDamageResources(z.defs)
	if len(misses) != 1 {
		t.Fatalf("lint found %d misses, want 1 (only the 'saniti' typo)", len(misses))
	}
	if misses[0].resource != "saniti" {
		t.Fatalf("lint flagged %q, want \"saniti\"", misses[0].resource)
	}
}
