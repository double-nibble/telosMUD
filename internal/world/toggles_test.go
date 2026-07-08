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

// TestDebugToggleAndEcho (#116): the `debug` staff toggle flips the session pref, and z.echoDebug fans a line
// ONLY to staff sessions in the zone that have it on. Covers the toggle, the report line, and the fan-out gate.
func TestDebugToggleAndEcho(t *testing.T) {
	z, staff := abilityTestZone(t)
	staff.tier = tierBuilder // rank 20 (staff)

	// Bare `debug` reports OFF by default; `debug on` flips it and confirms.
	z.dispatch(staff, "debug")
	if !drainContains(t, staff, "OFF") {
		t.Fatal("debug should report OFF by default")
	}
	z.dispatch(staff, "debug on")
	if !staff.debugEchoes {
		t.Fatal("`debug on` must set the session pref")
	}
	drain(staff)

	// A watching staffer with the pref on receives an echo, prefixed as out-of-band.
	z.echoDebug("something broke")
	if !drainContains(t, staff, "[debug] something broke") {
		t.Fatal("a staff session with debug on must receive the echo")
	}

	// `debug off` unsubscribes.
	z.dispatch(staff, "debug off")
	drain(staff)
	z.echoDebug("second event")
	if drainContains(t, staff, "second event") {
		t.Fatal("a staff session with debug off must NOT receive echoes")
	}
}

// TestDebugEchoGatedByRank (#116): echoDebug must skip a session that has the pref set but is no longer staff
// (demoted mid-session, the pref stale until relog) and must never reach a plain player.
func TestDebugEchoGatedByRank(t *testing.T) {
	z, staff := abilityTestZone(t)
	staff.tier = tierBuilder
	z.dispatch(staff, "debug on")
	drain(staff)

	// Demote the live session below staff rank: the stale pref must NOT keep delivering zone internals.
	staff.tier = tierPlayer
	z.echoDebug("post-demotion event")
	if drainContains(t, staff, "post-demotion event") {
		t.Fatal("a demoted session must stop receiving debug echoes even with the pref still set")
	}

	// A second, genuine player can never see the verb nor receive echoes (its pref can't be set).
	player := makeRoomPlayer(z, "Mortal")
	z.dispatch(player, "debug on")
	if !drainContains(t, player, "Huh?") {
		t.Fatal("a player must get the unknown-verb response for `debug` (staff verb is hidden)")
	}
	if player.debugEchoes {
		t.Fatal("a player must not be able to set the debug pref")
	}
}

// TestDebugEchoThroughInvoke (#116): a genuine RUNTIME error driven through the real invoke path (not a direct
// echoScriptError call) must reach a watching staff session — this guards the actual call site in invoke
// against a refactor that drops it. The raw error still goes to the ops log.
func TestDebugEchoThroughInvoke(t *testing.T) {
	z, staff := abilityTestZone(t)
	staff.tier = tierBuilder
	z.dispatch(staff, "debug on")
	drain(staff)

	ch := z.lua.chunkFor("test:onfire", `error("boom from content")`) // compiles fine, errors at runtime
	if ch == nil {
		t.Fatal("a valid script must compile")
	}
	_ = z.lua.invoke(ch, &luaInvocation{actor: staff.entity}, nil)
	if !drainContains(t, staff, "lua error [test:onfire]") {
		t.Fatal("a runtime error through invoke must be echoed to a watching staff session")
	}
}

// TestDebugEchoCompileError (#116): a COMPILE error is the highest-value builder signal (the script never
// runs, so no runtime site fires) — it must be echoed to a watching staff session.
func TestDebugEchoCompileError(t *testing.T) {
	z, staff := abilityTestZone(t)
	staff.tier = tierBuilder
	z.dispatch(staff, "debug on")
	drain(staff)

	if ch := z.lua.chunkFor("test:broken", `this is not ))) valid lua`); ch != nil {
		t.Fatal("a syntactically broken script must not compile")
	}
	if !drainContains(t, staff, "lua compile error [test:broken]") {
		t.Fatal("a compile error must be echoed to a watching staff session (the broken edit is otherwise silently inert)")
	}
}

// TestDebugEchoSkipsPendingAndSanitizes (#116): echoDebug must skip a PENDING (not-yet-activated) staff
// session even with the pref set, and must strip control characters from the content-authored diagnostic so
// an embedded newline can't spoof a second "[debug]" line.
func TestDebugEchoSkipsPendingAndSanitizes(t *testing.T) {
	z, staff := abilityTestZone(t)
	staff.tier = tierBuilder
	z.dispatch(staff, "debug on")
	drain(staff)

	staff.pending = true
	z.echoDebug("while pending")
	if drainContains(t, staff, "while pending") {
		t.Fatal("a pending session must not receive debug echoes")
	}
	staff.pending = false

	// A newline/control in the (content-authored) line must not survive to spoof a line or inject control.
	z.echoDebug("line one\nline two\x1b[2J")
	got := drainAllText(staff.out) // NOTE: this helper joins multiple frames with "\n"; the echo is ONE frame
	if strings.Count(got, "[debug]") != 1 {
		t.Fatalf("an embedded newline must not spawn a second [debug] line: %q", got)
	}
	if strings.Contains(got, "\x1b") {
		t.Fatalf("the ESC control must be stripped from the echo: %q", got)
	}
	// The embedded newline was removed, so the two halves are concatenated within the single echo frame.
	if !strings.Contains(got, "line oneline two") {
		t.Fatalf("the embedded newline must be stripped (halves concatenated), got: %q", got)
	}
}
