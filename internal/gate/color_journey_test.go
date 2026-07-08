package gate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/directory"
)

// color_journey_test.go — #23 connect-time RESTORE, driven BLACK-BOX through the real gate handle path (not a
// simulated apply). Every other journey test embeds stubAccountClient (GetColorPref => set=false), so the
// restore hook in gate.go (`case set: tc.SetColor(enabled)`) is never actually exercised — a deleted or
// inverted hook would still pass those. Here a fake account reports a persisted preference, we connect a
// scripted client through the REAL OAuth device flow + spawn, and assert the rendered world frames reflect it
// BEFORE any input: color OFF renders the (colored) `score` sheet as PLAIN (no ESC/SGR), and a pref-read
// ERROR is non-fatal (login still succeeds, the default color ON is retained and DOES render SGR).

// colorRestoreFakeAccount runs a minimal device flow (immediately authed to acct-1 with one character) and
// reports a scriptable color preference. It embeds stubAccountClient for the rest of the surface.
type colorRestoreFakeAccount struct {
	stubAccountClient
	getEnabled bool
	getSet     bool
	getErr     error
	getCalls   int
}

func (f *colorRestoreFakeAccount) StartDeviceAuth(context.Context, string) (string, string, time.Duration, error) {
	return "DEV123", "http://localhost:8080/login/DEV123", 5 * time.Millisecond, nil
}

func (f *colorRestoreFakeAccount) PollDeviceAuth(context.Context, string) (string, string, []CharacterInfo, error) {
	return "authed", "acct-1", []CharacterInfo{{ID: "c1", Name: "Chromatic"}}, nil
}

func (f *colorRestoreFakeAccount) GetColorPref(context.Context, string) (bool, bool, error) {
	f.getCalls++
	return f.getEnabled, f.getSet, f.getErr
}

// connectAndScore runs the whole connect path (OAuth device flow -> character select -> spawn) against a gate
// wired with fake, then renders the demo pack's COLORED `score` sheet (display_defs uses {{FG_CYAN}}/
// {{FG_GREEN}}/{{FG_BLUE}}). It returns the terminal transcript through the sheet plus the fake, so the caller
// can assert on the presence/absence of SGR escapes and that the restore hook was consulted.
func connectAndScore(t *testing.T, fake *colorRestoreFakeAccount) (string, *colorRestoreFakeAccount) {
	t.Helper()
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(fake)

	term := h.dial(t)
	term.expect(t, "To sign in, open this link") // device flow, not the bare-name prompt
	term.expect(t, "Choose a character:")        // authed on the first poll
	term.send(t, "1")
	term.expect(t, "The Temple Square") // SPAWNED — the restore hook has already run by now
	// The score sheet is content-defined and COLORED, so it is the deterministic probe for whether color is on.
	term.send(t, "score")
	term.expect(t, "Health") // a plain-text label present in the sheet under BOTH color states
	term.close(t)
	return term.acc.String(), fake
}

// TestColorPrefRestoreJourneyOff: a persisted `color off` is read + applied through the real connect path
// BEFORE the first world frame, so the colored `score` sheet renders as PLAIN — no ESC (0x1b) anywhere in the
// connect-through-spawn transcript.
func TestColorPrefRestoreJourneyOff(t *testing.T) {
	transcript, fake := connectAndScore(t, &colorRestoreFakeAccount{getSet: true, getEnabled: false})

	if fake.getCalls == 0 {
		t.Fatal("the connect path must consult GetColorPref (the restore hook was not invoked)")
	}
	if strings.ContainsRune(transcript, 0x1b) {
		t.Fatalf("a restored `color off` must render every frame PLAIN, but found an ESC/SGR in the transcript:\n%q", transcript)
	}
}

// TestColorPrefRestoreJourneyReadErrorKeepsDefault: when GetColorPref ERRORS, login is NON-FATAL (gate.go's
// error branch) — the player still spawns, and the default color ON is retained, so the colored `score` sheet
// DOES render SGR. This case doubles as the positive control proving the sheet emits 0x1b under color-on, so
// the OFF assertion above is not vacuous.
func TestColorPrefRestoreJourneyReadErrorKeepsDefault(t *testing.T) {
	transcript, fake := connectAndScore(t, &colorRestoreFakeAccount{getErr: errors.New("prefs store unavailable")})

	if fake.getCalls == 0 {
		t.Fatal("the connect path must consult GetColorPref even when it will error")
	}
	// Login succeeded (connectAndScore only returns after 'The Temple Square' + the score sheet rendered), so
	// the read error was non-fatal. The default (color ON) must still be in effect: the colored sheet has SGR.
	if !strings.ContainsRune(transcript, 0x1b) {
		t.Fatalf("a pref-read error must keep the default color ON (SGR rendered), but the transcript was plain:\n%q", transcript)
	}
}
