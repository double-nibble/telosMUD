//go:build e2e

// Package e2e holds the black-box acceptance tier (per docs/TESTING.md): start the
// live stack, connect a real telnet client to the gate, drive player commands, and
// assert the player-visible output.
//
// DOUBLE-GATED so the hermetic `go test ./...` NEVER runs it:
//  1. the `e2e` build tag (above) excludes this file from the default suite entirely —
//     `make test-e2e` (and CI's e2e job) build with `-tags e2e`. A dev whose `make up`
//     stack is running still gets a hermetic default suite.
//  2. once built, helpers.E2EAddr SKIPS when the gate (TELOS_E2E_ADDR, default
//     localhost:4000) is not reachable — so `make test-e2e` with the stack down is a
//     clean SKIP, never a failure.
package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// killDeadline bounds the `kill goblin` poll. Melee resolves over PULSE_VIOLENCE-paced
// rounds (~3s/round measured live: 10 base pulses x 250ms + handler overhead). The
// worst observed fight is ~13 rounds (~40s); this caps generously at 90s so RNG
// variance + the kill landing mid-round never trips a healthy fight, while a genuinely
// wedged combat still fails (rather than hanging). We poll and return the instant the
// death message lands, so a fast kill (median ~6 rounds, ~20s) is not slowed by this.
const killDeadline = 90 * time.Second

// TestCombatDeathSequence is the end-to-end acceptance test for ROOM RENDERING and
// the COMBAT DEATH SEQUENCE. A real telnet client drives the live stack through a
// cross-shard journey to the goblin and asserts the player-visible output at each
// step. It is the regression guard for two just-fixed bugs:
//
//   - lookRoom render gap (commit 98b69a6): mobs / ground items / corpses were
//     silently skipped in `look` even though targeting resolved them. The goblin
//     long-line assertion below (step 3) FAILS if that fix is reverted — this is the
//     committed, CI-running catch for "mobs not rendering in look".
//   - the death sequence (step 4+): a fresh unarmed player melees the goblin to 0 hp;
//     it emits "is DEAD!" and leaves a lootable corpse that ALSO must render (the same
//     render gap, for the corpse case) holding its rusty knife. Runs by DEFAULT — no
//     special env, no weapon, no special kill verb — exactly what a human player does.
//
// The journey also exercises the CROSS-SHARD handoff: market -> grove crosses the
// midgaard (shard-a) -> darkwood (shard-b) boundary, so a handoff regression that
// drops input or strands the player surfaces here too. We poll for each room name
// across handoff latency rather than fixed-sleeping.
func TestCombatDeathSequence(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)

	// --- 1. log in as a FRESH, unique character (not the persisted `kurt`) ---
	// A name the gate accepts (<=20 runes, no leading digit, no '.') and unique per
	// run so a re-run never collides with a row in the volume. Auth is Phase 14; today
	// the login name IS the character, spawned at midgaard:room:temple with full stats.
	name := fmt.Sprintf("e2e%d", time.Now().UnixNano()%1_000_000_000)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"fresh character did not spawn at The Temple Square; transcript:\n%s", c.Transcript())

	// --- 2. temple -> market -> grove (CROSS-SHARD handoff) -> hollow ---
	// market --north--> grove crosses the midgaard (shard-a) -> darkwood (shard-b)
	// boundary. Poll for each destination room name across handoff latency.
	c.Send("north")
	require.Truef(t, c.Expect("Market Square", 15*time.Second),
		"north from temple did not reach Market Square; transcript:\n%s", c.Transcript())
	c.Send("north")
	require.Truef(t, c.Expect("A Moonlit Grove", 20*time.Second),
		"cross-shard handoff to A Moonlit Grove (darkwood) failed; transcript:\n%s", c.Transcript())
	c.Send("north")
	require.Truef(t, c.Expect("A Dark Hollow", 20*time.Second),
		"north from grove did not reach A Dark Hollow; transcript:\n%s", c.Transcript())

	// --- 3. look -> the goblin renders (THE lookRoom fix, commit 98b69a6) ---
	// This is the primary committed regression catch: pre-fix the goblin (a non-player
	// occupant) was silently skipped, so its long-line never appeared even though
	// `kill goblin` could target it. Scope the assertion to THIS look's output so an
	// earlier render of the line cannot produce a false positive.
	//
	// PRECONDITION: a LIVE goblin in the hollow. The demo zones set reset_secs: 90, so a
	// killed goblin repops within ~90s. CI runs against a FRESH `make up` stack (goblin
	// always present); a fast-repeated local run can RACE a not-yet-repopped goblin, so
	// space reruns by the repop stride (~90s) or restart world-darkwood to force a repop.
	// This render assertion IS the live-goblin precondition for the death phase below.
	from := c.Len()
	c.Send("look")
	require.Truef(t, c.ExpectFrom(from, "A wiry goblin bares its teeth, clutching a rusty knife.", 10*time.Second),
		"the goblin's long-line did not render in `look` (lookRoom render regression, commit 98b69a6 — "+
			"OR the goblin was killed <90s ago and has not repopped yet; wait for the reset_secs stride); transcript:\n%s",
		c.Transcript())

	// --- 4+. death sequence: kill -> corpse renders -> loot recoverable ---
	// Runs by DEFAULT via realistic melee `kill goblin` against the HOLLOW goblin (the
	// weak one — NOT the lair chief). Starter combat is tuned so a FRESH UNARMED player
	// reliably wins: unarmed swings deal real damage (content unarmed_dice 1d6) and
	// passive regen pauses in combat, so the goblin (15 hp, no soak) dies in ~6 rounds
	// (median; 3-13 over 60 seeds, zero player deaths). Measured live (5 pristine kills):
	// 4-10 rounds in ~10-25s, ~2.5-3s/round (PULSE_VIOLENCE 10 * 250ms + handler
	// overhead). No weapon, no special verb — exactly what a human player does.
	//
	// TELOS_E2E_KILL is an OPTIONAL override: set it to a faster one-shot kill verb for
	// local speed. Unset (the committed/CI path), the test runs the real melee kill.
	killCmd := os.Getenv("TELOS_E2E_KILL")
	if killCmd == "" {
		killCmd = "kill goblin"
	}
	runDeathPhase(t, c, killCmd)
	c.Send("quit")
}

// runDeathPhase drives + asserts the COMBAT DEATH SEQUENCE: issue killCmd, poll for the
// goblin's death message, then assert the corpse renders in `look` (the corpse case of
// the lookRoom render fix) and the goblin's rusty knife is recoverable from the corpse
// (the death-sequence inventory->corpse transfer). The expected output strings are
// verified against the live stack and the engine (death.go builds "the corpse of
// <victim> lies here."; the get-from-container act template is "You get $p from $P.").
func runDeathPhase(t *testing.T, c *helpers.TelnetClient, killCmd string) {
	t.Helper()

	// Kill, racing the goblin's death against the player's. ExpectAny returns the FIRST
	// observed outcome so a player-death or a wedged fight fails with a clear, named
	// message instead of hanging until the test framework times out. The player-death
	// strings cover the respawn narration ("You have been slain! You awaken at the
	// temple") and the room-broadcast death line ("You are DEAD").
	c.Send(killCmd)
	switch c.ExpectAny([]string{"is DEAD!", "You have been slain", "You are DEAD"}, killDeadline) {
	case "is DEAD!":
		// expected: the goblin died. (The room-broadcast line is "$n is DEAD!" -> "a
		// small goblin is DEAD!"; "is DEAD!" matches it and is distinct from the player
		// "You are DEAD" case above.)
	case "You have been slain", "You are DEAD":
		t.Fatalf("PLAYER died before killing the goblin via %q (starter-combat balance regression — "+
			"a fresh unarmed player should reliably win); transcript:\n%s", killCmd, c.Transcript())
	default:
		t.Fatalf("goblin did not reach 'is DEAD!' within %s via %q (death-path regression or "+
			"combat wedged); transcript:\n%s", killDeadline, killCmd, c.Transcript())
	}

	// Personal-loot resolver (#146, Phase 12.1): the hollow goblin's `goblin_loot` table gives a GUARANTEED
	// common torch to the killer, delivered DIRECTLY to them (distinct from the corpse's carried rusty knife
	// below). This is the first e2e proof the Phase-12 personal-loot resolver fires in the LIVE death path —
	// it runs untested at e2e today despite the live demo wiring it. We assert ONLY the guaranteed torch; the
	// table's 25% rare sword is nondeterministic and is deliberately not asserted (flake-free).
	require.Truef(t, c.Expect("You receive a wooden torch.", 10*time.Second),
		"the guaranteed personal-loot torch was not delivered on kill (Phase-12 loot-resolver regression); transcript:\n%s",
		c.Transcript())

	// look -> the corpse renders. This asserts BOTH that the death sequence built the
	// corpse AND that lookRoom renders it (the same render gap that hid the live goblin).
	from := c.Len()
	c.Send("look")
	require.Truef(t, c.ExpectFrom(from, "The corpse of a small goblin lies here.", 10*time.Second), // presentation initial-cap (Track 1): lookRoom caps the corpse's room-presence line
		"the goblin's corpse did not render in `look` after the kill (death-sequence or lookRoom regression); transcript:\n%s",
		c.Transcript())

	// look corpse -> the loot is visible INSIDE the corpse (the container render of the
	// dead mob's inventory). The goblin carried a rusty knife (a reset `into` the mob);
	// on death its inventory flows into the corpse, so looking in the corpse lists it.
	from = c.Len()
	c.Send("look corpse")
	require.Truef(t, c.ExpectFrom(from, "A rusty knife", 10*time.Second), // container listing lines are initial-capped (Track 1)
		"`look corpse` did not list the goblin's rusty knife (death-sequence inventory->corpse transfer regression); transcript:\n%s",
		c.Transcript())

	// get knife from corpse -> the loot is RECOVERABLE. Pins the death-sequence's
	// inventory->corpse transfer end to end (the item is really in the corpse container).
	c.Send("get knife from corpse")
	require.Truef(t, c.Expect("You get a rusty knife from the corpse of a small goblin", 10*time.Second),
		"could not recover the rusty knife from the corpse (death-sequence loot transfer regression); transcript:\n%s",
		c.Transcript())
}
