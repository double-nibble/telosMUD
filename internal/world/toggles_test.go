package world

import (
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// toggles_test.go — #30 Slice 2: the staff view toggles `holylight` and `rolls`. Covers the rank gate
// (hidden from players), the holylight tier-grant cap, and the rolls check-visibility upgrade.

// TestHolylightToggle: a staff member whose tier grants see-all can flip holylight off and back on; the
// entity's reserved flag tracks it.
func TestHolylightToggle(t *testing.T) {
	z, staff := abilityTestZone(t)
	staff.tier = tierBuilder // default ladder: builder grants holylight

	z.dispatch(staff, "holylight on")
	if !hasFlag(staff.entity, flagHolylight) {
		t.Fatal("holylight on should set the see-all flag for a granting tier")
	}
	z.dispatch(staff, "holylight off")
	if hasFlag(staff.entity, flagHolylight) {
		t.Fatal("holylight off should clear the see-all flag")
	}
}

// TestHolylightCapNoGrant: a staff tier that does NOT grant see-all (a bare "moderator" rung) can't turn
// holylight on — the cap prevents self-elevation — though the verb is visible to it (positive rank).
func TestHolylightCapNoGrant(t *testing.T) {
	z, mod := abilityTestZone(t)
	z.defs.trust = buildTrustLadder([]content.TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "moderator", Rank: 10}, // no flags => no see-all grant
	})
	mod.tier = "moderator"

	z.dispatch(mod, "holylight on")
	if hasFlag(mod.entity, flagHolylight) {
		t.Fatal("a tier that does not grant see-all must not be able to turn holylight on")
	}
	if !drainContains(t, mod, "does not grant see-all") {
		t.Fatal("holylight on for a non-granting tier should explain the refusal")
	}
}

// TestStaffTogglesHiddenFromPlayer: a player (rank 0) can neither see nor run the toggle verbs — they get
// the unknown-verb response, exactly like `stat`.
func TestStaffTogglesHiddenFromPlayer(t *testing.T) {
	z, player := abilityTestZone(t) // no tier => rank 0
	for _, verb := range []string{"holylight", "rolls on"} {
		z.dispatch(player, verb)
		if !drainContains(t, player, "Huh?") {
			t.Fatalf("a player typing %q must get the unknown-verb response (staff verb is invisible)", verb)
		}
	}
}

// TestRollsToggleUpgradesDefaultHiddenCheck: with `rolls on`, a check hidden only by the engine default
// (visInherit) surfaces its math to the roller; with rolls off it stays hidden; and an EXPLICIT content
// visHide is respected even with rolls on.
func TestRollsToggleUpgradesDefaultHiddenCheck(t *testing.T) {
	z, staff := abilityTestZone(t)
	staff.tier = tierBuilder
	mob := makeMobTarget(z, staff.entity, "goblin")
	c := checkCtx(z, staff.entity, staff.entity, mob)

	// A default-visibility check (visInherit) with rolls OFF: no roll line.
	defCheck := func() *checkSpec {
		return &checkSpec{
			label: "Climb", dice: d1(t), bonus: litNode{v: 14}, vs: checkVs{dc: litNode{v: 15}},
			bands: []checkBand{{marginMin: bn(0), label: "success"}, {label: "failure"}},
		}
	}
	drainOutputs(staff)
	resolveCheck(c, defCheck())
	if out := drainOutputs(staff); len(out) != 0 {
		t.Fatalf("rolls off: a default-hidden check should emit nothing, got %v", out)
	}

	// rolls ON: the same default-visibility check now emits its math.
	z.dispatch(staff, "rolls on")
	drainOutputs(staff)
	resolveCheck(c, defCheck())
	if out := drainOutputs(staff); len(out) == 0 || !strings.Contains(strings.Join(out, "\n"), "Climb") {
		t.Fatalf("rolls on: a default-hidden check should surface its math, got %v", out)
	}

	// An EXPLICIT content visHide is respected even with rolls on.
	hide := defCheck()
	hide.visibility = visHide
	drainOutputs(staff)
	resolveCheck(c, hide)
	if out := drainOutputs(staff); len(out) != 0 {
		t.Fatalf("rolls on must not override an explicit content visHide, got %v", out)
	}
}

// TestWizinvisHidesFromLowerRank: a staff member with wizinvis is concealed from a strictly-lower-rank
// viewer (a mortal), but visible to an equal/higher rank, to a holylight viewer, and to themselves.
func TestWizinvisHidesFromLowerRank(t *testing.T) {
	z, staff := abilityTestZone(t)
	staff.tier = tierBuilder // rank 20
	setFlag(staff.entity, flagWizinvis, true)

	mortal := makePlayerTargetInRoom(z, staff.entity, "Pleb") // tier "" => rank 0
	admin := makePlayerTargetInRoom(z, staff.entity, "Boss")
	admin.tier = tierAdmin // rank 40

	if visibleTo(mortal.entity, staff.entity) {
		t.Error("a mortal (rank 0) must not see a wizinvis builder (rank 20)")
	}
	if !visibleTo(admin.entity, staff.entity) {
		t.Error("an admin (rank 40) must still see a wizinvis builder (equal/higher rank)")
	}
	if !visibleTo(staff.entity, staff.entity) {
		t.Error("a wizinvis staffer must always see themselves")
	}
	// A holylight viewer sees everyone regardless.
	setFlag(mortal.entity, flagHolylight, true)
	if !visibleTo(mortal.entity, staff.entity) {
		t.Error("a holylight viewer must see a wizinvis staffer")
	}
}

// TestWizinvisCommandGateAndToggle: a player can neither see nor run `wizinvis`; a staff member flips it.
func TestWizinvisCommandGateAndToggle(t *testing.T) {
	z, staff := abilityTestZone(t)
	staff.tier = tierBuilder
	z.dispatch(staff, "wizinvis on")
	if !hasFlag(staff.entity, flagWizinvis) {
		t.Fatal("wizinvis on should set the flag for a staff member")
	}
	z.dispatch(staff, "wizinvis off")
	if hasFlag(staff.entity, flagWizinvis) {
		t.Fatal("wizinvis off should clear the flag")
	}

	z2, player := abilityTestZone(t)
	z2.dispatch(player, "wizinvis on")
	if !drainContains(t, player, "Huh?") {
		t.Fatal("a player typing `wizinvis` must get the unknown-verb response (staff verb is invisible)")
	}
	if hasFlag(player.entity, flagWizinvis) {
		t.Fatal("a player must not be able to set wizinvis")
	}
}

// TestWizinvisIsReserved: wizinvis is content-unsettable and CLEARED at login (session-scoped reset), like
// the elevation flags — so a relog drops it and content can never self-conceal a mortal.
func TestWizinvisIsReserved(t *testing.T) {
	if !reservedFlag(flagWizinvis) {
		t.Fatal("wizinvis must be a reserved flag")
	}
	z, caster := abilityTestZone(t)
	e := caster.entity

	// Content set_flag of wizinvis is refused (a mortal can't self-conceal).
	c := seededCtx(z, e, e, dispHelpful)
	if err := opSetFlag(c, &effectOp{flag: flagWizinvis}); err != nil {
		t.Fatalf("opSetFlag: %v", err)
	}
	if hasFlag(e, flagWizinvis) {
		t.Error("content set_flag must not set the reserved wizinvis flag")
	}

	// Set it via the trusted path, then a fresh-login reconcile clears it (no tier grants wizinvis).
	setFlag(e, flagWizinvis, true)
	applyTierFlags(e, "admin") // admin grants holylight/builder/admin — but never wizinvis
	if hasFlag(e, flagWizinvis) {
		t.Error("applyTierFlags at login must clear wizinvis (session-scoped, no tier grants it)")
	}
}
