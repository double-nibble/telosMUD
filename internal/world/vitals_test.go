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
