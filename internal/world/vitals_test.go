package world

import (
	"strings"
	"testing"
)

// vitals_test.go covers #40: the `vitals` toggle, the vitals-bearing prompt, and the live push at a
// combat-round boundary.

// TestVitalsToggleAndPrompt proves `vitals on/off` flips the pref and the prompt gains/loses the pools.
func TestVitalsToggleAndPrompt(t *testing.T) {
	z, caster := abilityTestZone(t) // defines hp (max 100) + mana (max 100)
	setResourceCurrent(caster.entity, "hp", 80)
	setResourceCurrent(caster.entity, "mana", 30)

	// Default OFF: the classic bare prompt.
	if got := z.promptMarkup(caster); got != "> " {
		t.Fatalf("default prompt = %q, want the bare %q", got, "> ")
	}

	z.dispatch(caster, "vitals on")
	if !caster.vitalsLive {
		t.Fatal("`vitals on` did not set the pref")
	}
	got := z.promptMarkup(caster)
	// Pools are sorted by ref: hp before mana.
	if !strings.HasPrefix(got, "[") || !strings.Contains(got, "hp: 80/100") || !strings.Contains(got, "mana: 30/100") || !strings.HasSuffix(got, "] > ") {
		t.Fatalf("vitals-on prompt = %q, want a [hp: 80/100 mana: 30/100] > block", got)
	}

	z.dispatch(caster, "vitals off")
	if caster.vitalsLive {
		t.Fatal("`vitals off` did not clear the pref")
	}
	if got := z.promptMarkup(caster); got != "> " {
		t.Fatalf("vitals-off prompt = %q, want the bare %q", got, "> ")
	}
}

// collectHUD drains a session's out buffer and reports which HUD GMCP packages were emitted + whether a
// vitals-bearing text prompt was sent.
func collectHUD(s *session) (pkgs map[string]bool, promptWithPools bool) {
	pkgs = map[string]bool{}
	for {
		select {
		case f := <-s.out:
			if g := f.GetGmcp(); g != nil {
				pkgs[g.GetPkg()] = true
			}
			if p := f.GetPrompt(); p != nil && strings.Contains(p.GetMarkup(), "hp:") {
				promptWithPools = true
			}
		default:
			return pkgs, promptWithPools
		}
	}
}

// TestFlushHUDNonCombatGaugesOnly pins #84 part 1: the non-combat cadence (withPrompt=false) pushes the
// live GMCP gauges when a vital moved but does NOT reprint the text prompt (that would spam a plain client).
func TestFlushHUDNonCombatGaugesOnly(t *testing.T) {
	z, s := abilityTestZone(t)
	s.vitalsLive = true
	setResourceCurrent(s.entity, "hp", 80)
	z.flushHUD(s, false) // prime the last-sent buffers
	collectHUD(s)        // drain the priming frames

	setResourceCurrent(s.entity, "hp", 90) // passive regen out of combat
	if !z.flushHUD(s, false) {
		t.Fatal("flushHUD should report it sent a HUD delta after HP moved")
	}
	pkgs, prompt := collectHUD(s)
	if !pkgs["Char.Vitals"] {
		t.Error("non-combat tick did not push the Char.Vitals gauge after HP moved")
	}
	if prompt {
		t.Error("non-combat tick reprinted the text prompt; it must push gauges only (no prompt spam)")
	}
}

// TestFlushHUDCombatSendsPrompt: the combat cadence (withPrompt=true) also reprints the vitals text prompt.
func TestFlushHUDCombatSendsPrompt(t *testing.T) {
	z, s := abilityTestZone(t)
	s.vitalsLive = true
	setResourceCurrent(s.entity, "hp", 80)
	z.flushHUD(s, true)
	collectHUD(s)

	setResourceCurrent(s.entity, "hp", 70)
	z.flushHUD(s, true)
	_, prompt := collectHUD(s)
	if !prompt {
		t.Error("combat tick did not reprint the vitals-bearing text prompt after HP moved")
	}
}

// TestFlushHUDStatusDeltaNoVitalChange pins #84 part 2: a status change with ZERO vital movement pushes the
// Char.Status delta (not only on the next command), and does NOT push Char.Vitals or reprint the prompt.
func TestFlushHUDStatusDeltaNoVitalChange(t *testing.T) {
	z, s := abilityTestZone(t)
	s.vitalsLive = true
	setResourceCurrent(s.entity, "hp", 80)
	z.flushHUD(s, true) // prime vitals + status
	collectHUD(s)

	// Change ONLY status (acquire a fighting target) — HP unchanged.
	foe := z.newEntity(ProtoRef("test:foe"))
	Add(foe, &Living{})
	foe.short = "a training dummy"
	s.entity.living.fighting = foe

	z.flushHUD(s, true)
	pkgs, prompt := collectHUD(s)
	if !pkgs["Char.Status"] {
		t.Error("a mid-combat status change with no vital movement did not push Char.Status")
	}
	if pkgs["Char.Vitals"] {
		t.Error("Char.Vitals pushed despite no vital movement")
	}
	if prompt {
		t.Error("the text prompt was reprinted despite no vital movement (it carries only the vitals gauge)")
	}
}

// TestEnsureHUDPulseArmedOnVitalsOn: `vitals on` arms the non-combat HUD cadence; it is idempotent.
func TestEnsureHUDPulseArmedOnVitalsOn(t *testing.T) {
	z, s := abilityTestZone(t)
	if z.hudPulse != nil {
		t.Fatal("HUD pulse should not be armed before any `vitals on`")
	}
	z.dispatch(s, "vitals on")
	if z.hudPulse == nil {
		t.Fatal("`vitals on` did not arm the non-combat HUD pulse")
	}
	first := z.hudPulse
	z.dispatch(s, "vitals on")
	if z.hudPulse != first {
		t.Fatal("ensureHUDPulse re-armed a second pulse; it must be idempotent")
	}
}

// TestFlushHUDSkipsMidHandoff pins the review fix: a frozen (mid-handoff) `vitals on` session is NOT
// pushed HUD frames — its frames belong to the handoff machinery, not the HUD tick.
func TestFlushHUDSkipsMidHandoff(t *testing.T) {
	z, s := abilityTestZone(t)
	s.vitalsLive = true
	setResourceCurrent(s.entity, "hp", 80)
	z.flushHUD(s, false)
	collectHUD(s)

	setResourceCurrent(s.entity, "hp", 90)
	s.frozen = true // mid cross-shard handoff
	if z.flushHUD(s, false) {
		t.Fatal("flushHUD pushed to a frozen (mid-handoff) session; it must skip it")
	}
	if pkgs, _ := collectHUD(s); len(pkgs) != 0 {
		t.Fatalf("frozen session got HUD frames: %v", pkgs)
	}
}

// TestLiveVitalsFlushOnCombatRound proves a `vitals on` player gets a change-detected Char.Vitals + a
// vitals prompt after a combat round that drained their HP — and a `vitals off` player does NOT.
func TestLiveVitalsFlushOnCombatRound(t *testing.T) {
	run := func(t *testing.T, live bool) (gmcpVitals string, promptWithPools bool, hpDropped bool) {
		z, s := combatZone(t)
		s.vitalsLive = live
		// A mob that auto-hits the player for real damage.
		z.defs.combat.register("attacker", autoHitProfile(nil))
		mob := combatMob(z, s.entity, "goblin", "attacker", 100)
		equipWeapon(mob, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"}) // 6 dmg/swing
		z.startFight(mob, s.entity)                                             // the mob attacks the player

		before := resourceCurrent(s.entity, "hp")
		drainCombat(s) // clear the arrival/prior frames
		drainGMCP(s)

		z.runCombatRound(0)

		hpDropped = resourceCurrent(s.entity, "hp") < before
		// Collect the round's frames: GMCP payloads + any prompt markup carrying the pools block.
		for {
			select {
			case f := <-s.out:
				if g := f.GetGmcp(); g != nil && g.GetPkg() == "Char.Vitals" {
					gmcpVitals = string(g.GetJson())
				}
				if p := f.GetPrompt(); p != nil && strings.Contains(p.GetMarkup(), "hp:") {
					promptWithPools = true
				}
			default:
				return gmcpVitals, promptWithPools, hpDropped
			}
		}
	}

	t.Run("vitals on -> live push", func(t *testing.T) {
		gv, prompt, dropped := run(t, true)
		if !dropped {
			t.Fatal("precondition: the player should have taken damage this round")
		}
		if gv == "" {
			t.Fatal("no Char.Vitals pushed after the combat round for a `vitals on` player")
		}
		if !prompt {
			t.Fatal("no vitals-bearing prompt pushed after the combat round for a `vitals on` player")
		}
	})

	t.Run("vitals off -> no live push", func(t *testing.T) {
		gv, prompt, dropped := run(t, false)
		if !dropped {
			t.Fatal("precondition: the player should have taken damage this round")
		}
		if gv != "" || prompt {
			t.Fatalf("a `vitals off` player got a live push (gmcp=%q prompt=%v); it must wait for their own command", gv, prompt)
		}
	})
}
