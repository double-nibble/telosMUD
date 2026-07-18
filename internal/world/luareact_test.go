package world

import (
	"math/rand"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// luareact_test.go — the result-altering REACTION model tests (slice 7.9, P7-D8, T12). The §5
// done-when behaviors (Counterspell cancels a cast; Shield raises AC for the triggering swing
// only; concentration drops on a failed save) + the THREE security probes (a non-allowlisted
// modify field is a no-op; replace_target onto a non-consenting player is gate-blocked; a reaction
// loop is budget-bounded). Every reaction runs inline on the zone goroutine (single-writer).

// reactScriptedDefender gives entity e a *Scripted trigger block (so its on(kind, fn) reaction
// handlers register) and ensures its room is registered for handle resolution.
func reactScriptedDefender(z *Zone, e *Entity, src string) {
	Add(e, &Scripted{source: src})
	registerRoom(z, e.location)
}

// --- Counterspell: rx:cancel() on BeforeCastCommit (a reaction vetoes an in-flight cast) -------

// TestCounterspellCancelsCast is the headline 7.9 done-when: an OBSERVER's BeforeCastCommit
// reaction cancels the caster's in-flight cast (observe-then-recheck) — the cast pays no costs and
// resolves no ops.
func TestCounterspellCancelsCast(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	registerRoom(z, caster.entity.location)

	// A wizard observer in the room counters every cast unconditionally.
	wizard := makeMobTarget(z, caster.entity, "wizard")
	reactScriptedDefender(z, wizard, `
		on("BeforeCastCommit", function(ev, rx)
			rx:cancel()
		end)
	`)

	def := &abilityDef{
		ref: "luabolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		costs:        []resourceCost{{resource: "mana", amount: 10}},
		onResolveLua: `ctx.target:damage{amount=30, type="fire"}`,
	}
	z.defs.ability.register("luabolt", def)
	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))

	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("mob hp = %d, want 100 (the cast was COUNTERSPELLED — no damage resolved)", got)
	}
	if got := resourceCurrent(caster.entity, "mana"); got != 100 {
		t.Fatalf("caster mana = %d, want 100 (a counterspelled cast pays NO costs)", got)
	}
}

// TestCastCommitsWithoutCounter is the control: with no countering observer the cast commits
// normally (the reaction pass is inert when nobody cancels).
func TestCastCommitsWithoutCounter(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	registerRoom(z, caster.entity.location)

	def := &abilityDef{
		ref: "luabolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		costs:        []resourceCost{{resource: "mana", amount: 10}},
		onResolveLua: `ctx.target:damage{amount=30, type="fire"}`,
	}
	z.defs.ability.register("luabolt", def)
	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))

	if got := resourceCurrent(mob, "hp"); got != 70 {
		t.Fatalf("mob hp = %d, want 70 (no counter — the cast resolved)", got)
	}
}

// --- Shield: rx:modify("ac", +5) on the to-hit checkpoint (this swing only) --------------------

// shieldToHitProfile builds a to-hit whose DC is the defender's AC: 1d1 (rolls 1) vs $target.ac,
// margin>=0 -> hit, else miss. With ac=1 the swing lands (margin 0); a +5 AC bump makes it miss.
func shieldToHitProfile() *combatProfile {
	return &combatProfile{
		toHit: &checkSpec{
			label: "Attack", dice: mustDiceT("1d1"),
			vs:    checkVs{dc: an("$target.ac")},
			bands: []checkBand{{marginMin: litNode{v: 0}, label: "hit"}, {label: "miss"}},
		},
	}
}

// TestShieldRaisesACForTriggeringSwingOnly asserts a defender's to-hit reaction raises AC for the
// triggering swing (making it miss) but does NOT persist — a later swing with no reaction lands.
func TestShieldRaisesACForTriggeringSwingOnly(t *testing.T) {
	z, s := combatZone(t)
	z.defs.attr.register("ac", &attributeDef{ref: "ac", base: litNode{v: 1}})
	z.defs.combat.register("attacker", shieldToHitProfile())
	s.entity.living.combatRef = "attacker"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})

	mob := combatMob(z, s.entity, "dummy", "", 100)
	// The defender Shields exactly ONCE (state-gated), raising AC +5 on the first incoming swing.
	reactScriptedDefender(z, mob, `
		on("ToHit", function(ev, rx)
			if not state.shielded then
				state.shielded = true
				rx:modify("ac", 5)
			end
		end)
	`)
	s.entity.living.fighting = mob

	// Swing 1: Shield fires (+5 AC) -> margin -5 -> MISS (no damage).
	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("hp after Shielded swing = %d, want 100 (Shield raised AC -> miss)", got)
	}
	// Swing 2: Shield is spent (state.shielded) -> AC back to 1 -> margin 0 -> HIT (6 damage).
	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(mob, "hp"); got != 94 {
		t.Fatalf("hp after un-Shielded swing = %d, want 94 (the AC bump did NOT persist)", got)
	}
}

// --- Concentration: an affect rx:cancel()s ITSELF on a failed OnDamageTaken save ---------------

// TestConcentrationDropsOnFailedSave asserts a concentration affect drops itself (via rx:cancel)
// when its OnDamageTaken reaction body fails its save, and the affect is gone after the hit.
func TestConcentrationDropsOnFailedSave(t *testing.T) {
	z, s := combatZone(t)
	caster := s.entity

	// A concentration affect whose OnDamageTaken Lua reaction always "fails the save" and cancels
	// itself. The affect is self-applied (source = the caster).
	z.defs.affect.register("concentration", &affectDef{
		ref: "concentration", name: "Concentrating", stacking: stackIgnore, maxStacks: 1, duration: 100,
		onReactionLua: map[eventKind]string{
			evOnDamageTaken: `
				-- a failed concentration save: drop the affect (rx:cancel() drops THIS affect)
				rx:cancel()
			`,
		},
	})
	applyAffect(caster, "concentration", attachOpts{duration: 100}, nil)
	if !hasAffect(caster, "concentration") {
		t.Fatal("concentration did not attach")
	}
	registerRoom(z, caster.location)

	// A mob hits the caster; the OnDamageTaken reaction fires and concentration breaks.
	mob := combatMob(z, caster, "ogre", "", 100)
	c := &effectCtx{z: z, actor: mob, source: mob, target: caster, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	setResourceCurrent(caster, "hp", 100)
	applied := dealDamage(c, caster, 10, "slash", "")

	if applied <= 0 {
		t.Fatalf("damage applied = %d, want > 0 (concentration breaking does NOT soak the hit)", applied)
	}
	if hasAffect(caster, "concentration") {
		t.Fatal("concentration is STILL active — it should have dropped on the failed save (rx:cancel)")
	}
}

// TestConcentrationHoldsOnPassedSave is the control: a reaction body that does NOT cancel keeps
// concentration up.
func TestConcentrationHoldsOnPassedSave(t *testing.T) {
	z, s := combatZone(t)
	caster := s.entity
	z.defs.affect.register("concentration", &affectDef{
		ref: "concentration", name: "Concentrating", stacking: stackIgnore, maxStacks: 1, duration: 100,
		onReactionLua: map[eventKind]string{
			evOnDamageTaken: `-- a passed save: do nothing`,
		},
	})
	applyAffect(caster, "concentration", attachOpts{duration: 100}, nil)
	registerRoom(z, caster.location)

	mob := combatMob(z, caster, "ogre", "", 100)
	c := &effectCtx{z: z, actor: mob, source: mob, target: caster, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	setResourceCurrent(caster, "hp", 100)
	dealDamage(c, caster, 10, "slash", "")

	if !hasAffect(caster, "concentration") {
		t.Fatal("concentration dropped on a PASSED save — it should still be active")
	}
}

// --- T12 security: a non-allowlisted modify field is a SILENT no-op ----------------------------

// TestNonAllowlistedModifyFieldIsNoOp asserts rx:modify with a field NOT in the checkpoint's closed
// enum is ignored (no mutation, no error) — at to-hit only "ac" is allowed, so modify("hp", -999)
// (and any other field) does nothing to the swing or the entity.
func TestNonAllowlistedModifyFieldIsNoOp(t *testing.T) {
	z, s := combatZone(t)
	z.defs.attr.register("ac", &attributeDef{ref: "ac", base: litNode{v: 1}})
	z.defs.combat.register("attacker", shieldToHitProfile())
	s.entity.living.combatRef = "attacker"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})

	mob := combatMob(z, s.entity, "dummy", "", 100)
	// The reaction tries to modify a non-allowlisted field ("hp") AND a bogus field — both no-ops.
	// It does NOT touch "ac", so the swing still lands.
	reactScriptedDefender(z, mob, `
		on("ToHit", function(ev, rx)
			local a = rx:modify("hp", -999)        -- not allowlisted at to-hit -> false, no-op
			local b = rx:modify("bogus_field", 100) -- not allowlisted -> false, no-op
			state.mod_hp = a
			state.mod_bogus = b
		end)
	`)
	s.entity.living.fighting = mob

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())

	// The swing landed (the bogus modifies changed nothing): 6 damage.
	if got := resourceCurrent(mob, "hp"); got != 94 {
		t.Fatalf("hp = %d, want 94 (a non-allowlisted modify field is a no-op — the swing still hit for 6)", got)
	}
	// The rx:modify returned false for each non-allowlisted field (the silent-drop signal to Lua).
	es := z.lua.entityScripts[mob.rid]
	if es == nil {
		t.Fatal("the defender's reaction script did not register")
	}
	if got := es.state.RawGetString("mod_hp"); got != lua.LFalse {
		t.Fatalf("rx:modify(\"hp\",...) returned %v, want false (non-allowlisted -> dropped)", got)
	}
	if got := es.state.RawGetString("mod_bogus"); got != lua.LFalse {
		t.Fatalf("rx:modify(\"bogus_field\",...) returned %v, want false (non-allowlisted -> dropped)", got)
	}
}

// --- T12 security: a non-finite modify delta is a silent no-op (Finding B) ---------------------

// TestNonFiniteModifyDeltaIsNoOp asserts rx:modify with a NaN or Inf delta is dropped silently (no
// mutation, no error) — a non-finite delta would otherwise flow into int(...) at the seam
// (implementation-defined) and corrupt the pending result. The swing still lands normally.
func TestNonFiniteModifyDeltaIsNoOp(t *testing.T) {
	z, s := combatZone(t)
	z.defs.attr.register("ac", &attributeDef{ref: "ac", base: litNode{v: 1}})
	z.defs.combat.register("attacker", shieldToHitProfile())
	s.entity.living.combatRef = "attacker"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})

	mob := combatMob(z, s.entity, "dummy", "", 100)
	// The reaction tries an Inf and a NaN AC bump — both must be dropped (the allowlisted field "ac"
	// is correct, but the non-finite VALUE is rejected), so the swing still lands for 6.
	reactScriptedDefender(z, mob, `
		on("ToHit", function(ev, rx)
			state.inf = rx:modify("ac", 1/0)   -- +Inf -> dropped
			state.nan = rx:modify("ac", 0/0)   -- NaN  -> dropped
		end)
	`)
	s.entity.living.fighting = mob

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())

	// AC unaffected by the non-finite deltas: the swing landed (margin 0 -> hit), 6 damage.
	if got := resourceCurrent(mob, "hp"); got != 94 {
		t.Fatalf("hp = %d, want 94 (a non-finite AC delta is dropped — the swing still hit for 6)", got)
	}
	es := z.lua.entityScripts[mob.rid]
	if es == nil {
		t.Fatal("the defender's reaction script did not register")
	}
	if got := es.state.RawGetString("inf"); got != lua.LFalse {
		t.Fatalf("rx:modify(\"ac\", Inf) returned %v, want false (non-finite -> dropped)", got)
	}
	if got := es.state.RawGetString("nan"); got != lua.LFalse {
		t.Fatalf("rx:modify(\"ac\", NaN) returned %v, want false (non-finite -> dropped)", got)
	}
}

// --- combat: the "amount" damage-shield contract is REDUCE-ONLY (Finding #3) -------------------

// TestDamageShieldAmountReducesOnly asserts rx:modify("amount", -N) on OnDamageTaken soaks the
// pending blow (reduce-only) but a POSITIVE delta is DROPPED (a reaction cannot amplify damage past
// the original mitigated amount).
func TestDamageShieldAmountReducesOnly(t *testing.T) {
	z, s := combatZone(t)
	caster := s.entity
	registerRoom(z, caster.location)

	// A shield affect: reduces the incoming blow by 4 (a negative delta — honored).
	z.defs.affect.register("shieldaffect", &affectDef{
		ref: "shieldaffect", name: "Damage Shield", stacking: stackIgnore, maxStacks: 1, duration: 100,
		onReactionLua: map[eventKind]string{
			evOnDamageTaken: `rx:modify("amount", -4)`,
		},
	})
	applyAffect(caster, "shieldaffect", attachOpts{duration: 100}, nil)

	mob := combatMob(z, caster, "ogre", "", 100)
	setResourceCurrent(caster, "hp", 100)
	c := &effectCtx{z: z, actor: mob, source: mob, target: caster, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	applied := dealDamage(c, caster, 10, "slash", "")
	if applied != 6 {
		t.Fatalf("applied = %d, want 6 (10 - 4 shield; reduce-only delta honored)", applied)
	}

	// Now a "vulnerability" affect that tries to AMPLIFY (+100) — the positive delta must be DROPPED.
	z.defs.affect.register("vulnaffect", &affectDef{
		ref: "vulnaffect", name: "Vulnerability", stacking: stackIgnore, maxStacks: 1, duration: 100,
		onReactionLua: map[eventKind]string{
			evOnDamageTaken: `rx:modify("amount", 100)`,
		},
	})
	caster2 := makeRoomPlayer(z, "Victim2").entity
	registerRoom(z, caster2.location)
	applyAffect(caster2, "vulnaffect", attachOpts{duration: 100}, nil)
	mob2 := combatMob(z, caster2, "ogre2", "", 100)
	setResourceCurrent(caster2, "hp", 100)
	c2 := &effectCtx{z: z, actor: mob2, source: mob2, target: caster2, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	applied2 := dealDamage(c2, caster2, 10, "slash", "")
	if applied2 != 10 {
		t.Fatalf("applied = %d, want 10 (a POSITIVE amount delta is dropped — no amplification past the original)", applied2)
	}
}

// --- T12 security: replace_target onto a NON-CONSENTING player is gate-blocked -----------------

// TestReplaceTargetReGatesOntoNonConsentingPlayer asserts rx:replace_target re-runs guardHarmful
// against the NEW target — a retarget onto a non-consenting player in a safe room is BLOCKED (the
// redirect does not take), so the gate is not bypassed via retarget.
func TestReplaceTargetReGatesOntoNonConsentingPlayer(t *testing.T) {
	z, s := combatZone(t)
	attacker := s.entity

	// A non-consenting bystander player (no PvP consent flags => the harm gate denies harm against
	// them by default — pvpAllowed is false).
	bystander := makePlayerTargetInRoom(z, attacker, "Bystander")
	setResourceCurrent(bystander.entity, "hp", 100)
	if pvpAllowed(attacker, bystander.entity) {
		t.Fatal("test precondition: expected NO PvP consent for the bystander")
	}
	registerRoom(z, attacker.location)

	// The victim mob carries an OnDamageTaken reaction that tries to redirect the blow onto the
	// non-consenting bystander. The re-gate must BLOCK it (the bystander takes nothing).
	mob := combatMob(z, attacker, "dummy", "", 100)
	bystanderID := bystander.entity.rid
	reactScriptedDefender(z, mob, `
		on("OnDamageTaken", function(ev, rx)
			-- try to redirect the blow onto the bystander handle stored in state by the test
			if state.bystander ~= nil then
				state.allowed = rx:replace_target(state.bystander)
			end
		end)
	`)
	// Seed the bystander handle into the mob's reaction state (a handle the script can pass back).
	es := z.lua.ensureEntityScript(mob)
	es.state.RawSetString("bystander", z.lua.newHandle(bystander.entity))
	_ = bystanderID

	c := &effectCtx{z: z, actor: attacker, source: attacker, target: mob, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	setResourceCurrent(mob, "hp", 100)
	dealDamage(c, mob, 10, "slash", "")

	// The bystander took NO damage (the retarget was gate-blocked).
	if got := resourceCurrent(bystander.entity, "hp"); got != 100 {
		t.Fatalf("bystander hp = %d, want 100 (replace_target onto a non-consenting player must be gate-blocked)", got)
	}
	// rx:replace_target returned false (the re-gate denied it).
	if got := es.state.RawGetString("allowed"); got != lua.LFalse {
		t.Fatalf("rx:replace_target returned %v, want false (re-gate denied the redirect)", got)
	}
}

// --- 7.9 completion: rx:replace_target REDIRECTS the blow (re-mitigated against the new target) ---

// redirectMobTo gives mob an OnDamageTaken reaction that redirects the blow onto the handle the test
// seeds into mob's reaction state (state.guardee), recording the rx:replace_target return in
// state.redirected. The guardian/misdirection shape: the original target takes 0, the seeded target
// takes the blow re-mitigated against ITS own resistances/soak.
func redirectMobTo(z *Zone, mob, guardee *Entity) {
	reactScriptedDefender(z, mob, `
		on("OnDamageTaken", function(ev, rx)
			if state.guardee ~= nil then
				state.redirected = rx:replace_target(state.guardee)
			end
		end)
	`)
	es := z.lua.ensureEntityScript(mob)
	es.state.RawSetString("guardee", z.lua.newHandle(guardee))
}

// TestReplaceTargetRedirectsBlowReMitigatedAgainstNewTarget is the headline 7.9-completion done-when:
// a PASSED re-gate redirects the WHOLE blow onto the new target, which takes it RE-MITIGATED against
// ITS OWN soak (not the original target's), while the original target takes 0.
func TestReplaceTargetRedirectsBlowReMitigatedAgainstNewTarget(t *testing.T) {
	z, s := combatZone(t)
	attacker := s.entity
	registerRoom(z, attacker.location)

	// The guardee (a mob, so the re-gate passes — mobs always consent to harm) has a DIFFERENT
	// per-target mitigation than the original target: soak_slash 7 (the original mob has 0). A 10 blow
	// redirected onto it lands as 10 - 7 = 3 (re-mitigated against ITS soak), proving re-mitigation
	// runs against the NEW target, not the original.
	guardee := combatMob(z, attacker, "guardee", "", 100)
	setAttrBase(guardee, "soak_slash", 7)

	mob := combatMob(z, attacker, "ward", "", 100) // soak_slash 0; would take the full 10
	redirectMobTo(z, mob, guardee)

	c := &effectCtx{z: z, actor: attacker, source: attacker, target: mob, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	applied := dealDamage(c, mob, 10, "slash", "")

	// dealDamage to the ORIGINAL target returns 0 (the blow moved off it).
	if applied != 0 {
		t.Fatalf("dealDamage to original target = %d, want 0 (the blow redirected away)", applied)
	}
	// The original target took NOTHING.
	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("original target hp = %d, want 100 (the blow redirected away — it takes 0)", got)
	}
	// The guardee took the blow RE-MITIGATED against ITS soak: 10 - 7 = 3 (NOT the original's 10).
	if got := resourceCurrent(guardee, "hp"); got != 97 {
		t.Fatalf("guardee hp = %d, want 97 (10 blow re-mitigated against ITS soak_slash 7 = 3 applied)", got)
	}
	// rx:replace_target returned true (the re-gate passed; the redirect was recorded).
	es := z.lua.ensureEntityScript(mob)
	if got := es.state.RawGetString("redirected"); got != lua.LTrue {
		t.Fatalf("rx:replace_target returned %v, want true (re-gate passed; redirect recorded)", got)
	}
}

// TestReplaceTargetRedirectFiresNewTargetOwnReaction proves the redirect goes through the REAL
// pipeline: the new target's OWN OnDamageTaken reaction (a damage-shield) fires on the redirected
// blow and reduces it further — the redirect is not a raw pool-write, it re-enters dealDamage.
func TestReplaceTargetRedirectFiresNewTargetOwnReaction(t *testing.T) {
	z, s := combatZone(t)
	attacker := s.entity
	registerRoom(z, attacker.location)

	// The guardee carries its own damage-shield reaction (-4) — it must fire on the redirected blow.
	z.defs.affect.register("guardeeshield", &affectDef{
		ref: "guardeeshield", name: "Guardee Shield", stacking: stackIgnore, maxStacks: 1, duration: 100,
		onReactionLua: map[eventKind]string{evOnDamageTaken: `rx:modify("amount", -4)`},
	})
	guardee := combatMob(z, attacker, "guardee", "", 100)
	applyAffect(guardee, "guardeeshield", attachOpts{duration: 100}, nil)
	registerRoom(z, guardee.location)

	mob := combatMob(z, attacker, "ward", "", 100)
	redirectMobTo(z, mob, guardee)

	c := &effectCtx{z: z, actor: attacker, source: attacker, target: mob, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	dealDamage(c, mob, 10, "slash", "")

	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("original target hp = %d, want 100 (blow redirected away)", got)
	}
	// 10 blow, redirected, then the guardee's OWN shield reaction soaks 4 => 6 applied (100 - 6 = 94).
	if got := resourceCurrent(guardee, "hp"); got != 94 {
		t.Fatalf("guardee hp = %d, want 94 (redirected 10 - its own 4 damage-shield reaction = 6 applied — the redirect went through the real pipeline)", got)
	}
}

// TestReplaceTargetRedirectLoopTerminates is the load-bearing safety req: A redirects to B and B
// redirects back to A — a redirect LOOP. It must TERMINATE (bounded by the shared cascade budget +
// the zone-level eventCascadeDepth backstop), never hang/crash, and the zone keeps serving.
func TestReplaceTargetRedirectLoopTerminates(t *testing.T) {
	z, s := combatZone(t)
	attacker := s.entity
	registerRoom(z, attacker.location)

	// Two mobs with HUGE pools so the loop bounds on BUDGET, not on death. Each redirects the blow it
	// takes onto the other — A -> B -> A -> ... a ping-pong redirect loop.
	a := combatMob(z, attacker, "alpha", "", 1000000)
	b := combatMob(z, attacker, "beta", "", 1000000)
	setResourceCurrent(a, "hp", 1000000)
	setResourceCurrent(b, "hp", 1000000)
	redirectMobTo(z, a, b)
	redirectMobTo(z, b, a)

	c := &effectCtx{z: z, actor: attacker, source: attacker, target: a, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}

	done := make(chan struct{})
	go func() {
		dealDamage(c, a, 10, "slash", "")
		close(done)
	}()
	select {
	case <-done:
		// Terminated (bounded) — good.
	case <-timeAfter():
		t.Fatal("the redirect loop did NOT terminate — the cascade backstop failed to bound it")
	}
	// The zone still serves: a fresh harmful op against a third target still works.
	mob := combatMob(z, attacker, "ogre", "", 100)
	c2 := &effectCtx{z: z, actor: attacker, source: attacker, target: mob, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	if dealDamage(c2, mob, 5, "slash", "") <= 0 {
		t.Fatal("the zone did not keep serving after the bounded redirect loop")
	}
}

// --- Deliverable B: the 5e MULTICLASS spell-slot Lua FORMULA [G7] -------------------------------
//
// This is an INDEPENDENT escape-hatch case: a Lua formula over MULTIPLE class levels that computes
// 5e multiclass spell slots. It depends only on the 7.4 Lua-formula entry point (luaFormula), NOT
// the reaction model — kept in this file as the §5 done-when content but provable on its own.

// multiclassSlotsFormula is the 5e multiclass spell-slot table as a Lua formula. The caster level
// for slots is full-casters' levels + half (rounded down) of half-casters' + third (rounded down)
// of third-casters' — then indexed into the 5e level-1..20 slot table. This `formula(level, slot)`
// returns the number of `slot`-th-level spell slots for the combined caster level. The test calls
// it via luaFormula reading the character's per-class levels as attributes.
const multiclassSlots1Formula = `
	-- combined caster level (5e PHB multiclass rule): full + floor(half/2) + floor(third/3)
	local full  = self:attr("full_caster_levels")
	local half  = self:attr("half_caster_levels")
	local third = self:attr("third_caster_levels")
	local cl = full + math.floor(half / 2) + math.floor(third / 3)
	-- 1st-level slots by combined caster level (5e table): CL1=2, CL2=3, CL3+=4.
	if cl <= 0 then return 0 end
	if cl == 1 then return 2 end
	if cl == 2 then return 3 end
	return 4
`

func TestMulticlassSpellSlotFormula(t *testing.T) {
	z, _ := combatZone(t)
	// The per-class-level attributes the formula reads (a character's class spread).
	for _, ref := range []string{"full_caster_levels", "half_caster_levels", "third_caster_levels"} {
		z.defs.attr.register(ref, &attributeDef{ref: ref, base: litNode{v: 0}})
	}
	z.defBundle().formulas["multiclass_slots_1"] = multiclassSlots1Formula

	subject := makeRoomPlayer(z, "Multiclasser").entity
	registerRoom(z, subject.location)

	cases := []struct {
		name              string
		full, half, third float64
		wantSlots         float64
	}{
		// Pure full-caster: Wizard 1 -> combined CL 1 -> 2 first-level slots.
		{"wizard1", 1, 0, 0, 2},
		// Wizard 2 -> CL 2 -> 3 slots.
		{"wizard2", 2, 0, 0, 3},
		// Paladin 2 (half) -> floor(2/2)=1 -> CL 1 -> 2 slots (the half-caster rounding).
		{"paladin2", 0, 2, 0, 2},
		// Wizard 2 + Paladin 2 -> 2 + floor(2/2)=1 = CL 3 -> 4 slots (the multiclass COMBINE).
		{"wiz2_pal2", 2, 2, 0, 4},
		// Ranger 1 (half, floor(1/2)=0) + Fighter-EK 3 (third, floor(3/3)=1) -> CL 1 -> 2 slots.
		{"ranger1_ek3", 0, 1, 3, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setAttrBase(subject, "full_caster_levels", tc.full)
			setAttrBase(subject, "half_caster_levels", tc.half)
			setAttrBase(subject, "third_caster_levels", tc.third)
			got, ok := z.luaFormula("multiclass_slots_1", subject, nil)
			if !ok {
				t.Fatalf("luaFormula(multiclass_slots_1) returned not-ok (the formula did not compute)")
			}
			if got != tc.wantSlots {
				t.Fatalf("multiclass 1st-level slots = %v, want %v (full=%v half=%v third=%v)",
					got, tc.wantSlots, tc.full, tc.half, tc.third)
			}
		})
	}
}

// --- T12 security: a reaction loop is budget-bounded (does not wedge/crash) ---------------------

// TestReactionLoopIsBudgetBounded asserts a reaction that fires an action that fires a reaction
// (a self-perpetuating loop) terminates at the shared cascade budget instead of spinning the zone
// goroutine. The test completes (it does not hang) and the zone keeps serving.
func TestReactionLoopIsBudgetBounded(t *testing.T) {
	z, s := combatZone(t)
	caster := s.entity
	registerRoom(z, caster.location)

	// A concentration-style affect whose OnDamageTaken reaction deals MORE damage to itself — each
	// damage fires OnDamageTaken again, a reaction->damage->reaction loop. It must terminate at the
	// shared eventBudget (maxEventHandlers / maxEventDepth), not hang.
	z.defs.affect.register("loop", &affectDef{
		ref: "loop", name: "Loop", stacking: stackIgnore, maxStacks: 1, duration: 1000,
		onReactionLua: map[eventKind]string{
			evOnDamageTaken: `self:damage{amount=1, type="slash"}`,
		},
	})
	applyAffect(caster, "loop", attachOpts{duration: 1000}, nil)

	mob := combatMob(z, caster, "ogre", "", 100)
	setResourceCurrent(caster, "hp", 100000) // huge pool so the loop bounds on BUDGET, not on death
	c := &effectCtx{z: z, actor: mob, source: mob, target: caster, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}

	done := make(chan struct{})
	go func() {
		dealDamage(c, caster, 10, "slash", "")
		close(done)
	}()
	select {
	case <-done:
		// Terminated (bounded) — good. The caster took bounded damage, not an infinite cascade.
	case <-timeAfter():
		t.Fatal("the reaction loop did NOT terminate — the cascade budget failed to bound it")
	}
	// The zone still serves: a fresh harmful op against the mob still works.
	c2 := &effectCtx{z: z, actor: caster, source: caster, target: mob, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	if dealDamage(c2, mob, 5, "slash", "") <= 0 {
		t.Fatal("the zone did not keep serving after the bounded reaction loop")
	}
}
