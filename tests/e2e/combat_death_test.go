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

// killDeadline bounds the `kill goblin` poll once the death phase is enabled. It is
// generous: combat resolves over PULSE_VIOLENCE rounds (10 base pulses x 250ms =
// 2.5s/round) and the wall-clock kill time varies with the dice. We poll for the
// death message and return the instant it lands; the cap only fires if combat is
// genuinely wedged.
const killDeadline = 2 * time.Minute

// TestCombatDeathSequence is the end-to-end acceptance test for ROOM RENDERING and
// the COMBAT DEATH SEQUENCE. A real telnet client drives the live stack through a
// cross-shard journey to the goblin and asserts the player-visible output at each
// step. It is the regression guard for two just-fixed bugs:
//
//   - lookRoom render gap (commit 98b69a6): mobs / ground items / corpses were
//     silently skipped in `look` even though targeting resolved them. The goblin
//     long-line assertion below (step 3) FAILS if that fix is reverted — this is the
//     committed, CI-running catch for "mobs not rendering in look".
//   - the death sequence (step 4+): the goblin reaches 0 hp, emits "is DEAD!", and
//     leaves a lootable corpse that ALSO must render (the same render gap, for the
//     corpse case). This phase is GATED on TELOS_E2E_KILL — see the note there.
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
	// PRECONDITION: a LIVE goblin in the hollow. The demo zones set no reset_secs, so a
	// killed mob does NOT repop until the shard restarts (re-runs the boot reset). CI
	// runs against a FRESH `make up` stack, so the goblin is always present; against a
	// long-lived dev stack where someone already killed it, restart world-darkwood to
	// respawn it. The death-message branch below explains the same constraint for reruns.
	from := c.Len()
	c.Send("look")
	require.Truef(t, c.ExpectFrom(from, "A wiry goblin bares its teeth, clutching a rusty knife.", 10*time.Second),
		"the goblin's long-line did not render in `look` (lookRoom render regression, commit 98b69a6 — "+
			"OR the goblin was already killed on a non-repopping dev stack; restart world-darkwood); transcript:\n%s",
		c.Transcript())

	// --- 4+. death sequence: kill -> corpse renders -> loot recoverable ---
	// GATED on TELOS_E2E_KILL. WHY: empirically (verified against the live stack), a
	// fresh player CANNOT reliably kill the hollow goblin in a CI-reasonable time. Bare-
	// handed (strength 10 -> str_bonus 0, damroll 0) the player deals ~no damage and the
	// goblin never dies. Even after grabbing + wielding the committed Market Square steel
	// longsword (2d6), the kill ran 2.5+ minutes with ~35 landed blows and the goblin
	// (85 hp, slash-resist + soak + hp regen) was STILL alive, while it landed 35 hits +
	// 5 crits on the player (a real player-death risk on a long fight). Melee is too slow
	// and too variable to gate CI on. The death phase below is correct and ready; it runs
	// only when a DETERMINISTIC kill affordance exists — set TELOS_E2E_KILL to a command
	// that one-shots the goblin (e.g. a committed test-only spell/op decided WITH the
	// user). Until then, the committed/CI run stops at step 3, which already catches the
	// live-mob render regression. See tests/e2e/README.md.
	killCmd := os.Getenv("TELOS_E2E_KILL")
	if killCmd == "" {
		t.Log("TELOS_E2E_KILL unset: skipping the death-sequence phase (melee is not " +
			"CI-reliable against the hollow goblin — see the test doc and tests/e2e/README.md). " +
			"The render-regression catch (step 3) ran.")
		c.Send("quit")
		return
	}
	runDeathPhase(t, c, killCmd)
	c.Send("quit")
}

// runDeathPhase drives + asserts the COMBAT DEATH SEQUENCE once a deterministic kill
// is available: issue killCmd, poll for the death message, then assert the corpse
// renders in `look` (the corpse case of the lookRoom render fix) and the goblin's
// rusty knife is recoverable from the corpse (the death-sequence inventory->corpse
// transfer). The expected output strings are verified against the live stack and the
// engine (death.go builds "the corpse of <victim> lies here."; the get-from-container
// act template is "You get $p from $P.").
func runDeathPhase(t *testing.T, c *helpers.TelnetClient, killCmd string) {
	t.Helper()

	// Kill, asserting on the FIRST of two outcomes so a player-death fails with a clear
	// message instead of timing out opaquely.
	c.Send(killCmd)
	switch c.ExpectAny([]string{"is DEAD!", "You have been slain"}, killDeadline) {
	case "is DEAD!":
		// expected: the goblin died.
	case "You have been slain":
		t.Fatalf("PLAYER died before killing the goblin (the kill affordance %q is not "+
			"deterministic enough); transcript:\n%s", killCmd, c.Transcript())
	default:
		t.Fatalf("goblin did not reach 'is DEAD!' within %s via %q (death-path regression or "+
			"combat wedged); transcript:\n%s", killDeadline, killCmd, c.Transcript())
	}

	// look -> the corpse renders. This asserts BOTH that the death sequence built the
	// corpse AND that lookRoom renders it (the same render gap that hid the live goblin).
	from := c.Len()
	c.Send("look")
	require.Truef(t, c.ExpectFrom(from, "the corpse of a small goblin lies here.", 10*time.Second),
		"the goblin's corpse did not render in `look` after the kill (death-sequence or lookRoom regression); transcript:\n%s",
		c.Transcript())

	// The loot is recoverable: the goblin carried a rusty knife (a reset `into` the
	// mob); on death its inventory flows into the corpse, so `get knife from corpse`
	// recovers it. Pins the death-sequence's inventory->corpse transfer, end to end.
	c.Send("get knife from corpse")
	require.Truef(t, c.Expect("You get a rusty knife from the corpse of a small goblin", 10*time.Second),
		"could not recover the rusty knife from the corpse (death-sequence loot transfer regression); transcript:\n%s",
		c.Transcript())
}
