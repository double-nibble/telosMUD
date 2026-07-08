package gate

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/telnet"
)

// color_test.go — the edge-local `color` command (Track 1 slice 2): toggles the telnet conn's SGR rendering,
// is handled at the gate, and is NOT forwarded to the world. #23 adds cross-session PERSISTENCE via the
// account (the gate<->account seam), so an on/off toggle survives a reconnect.

// colorFakeAccount records the color-pref reads/writes and returns scripted results. It embeds the stub for
// the rest of the AccountClient surface.
type colorFakeAccount struct {
	stubAccountClient
	// GetColorPref scripting.
	getEnabled, getSet bool
	getErr             error
	getCalls           int
	// SetColorPref recording.
	setCalls    int
	lastEnabled bool
	setErr      error
}

func (f *colorFakeAccount) GetColorPref(_ context.Context, _ string) (bool, bool, error) {
	f.getCalls++
	return f.getEnabled, f.getSet, f.getErr
}

func (f *colorFakeAccount) SetColorPref(_ context.Context, _ string, enabled bool) error {
	f.setCalls++
	f.lastEnabled = enabled
	return f.setErr
}

// TestHandleColorCommand drives the toggle through its states with the terminal's starting color EXPLICIT at
// each step, so the expected persist count is unambiguous. #23's change-detection guard (the coordinator's
// production change) means a write fires ONLY on an actual state change relative to tc.ColorEnabled() — a
// redundant `color on`/`color off` must NOT persist again (the write-amplification guard the security-auditor
// asked for). The bare `color` status query never persists.
func TestHandleColorCommand(t *testing.T) {
	var out bytes.Buffer
	tc := telnet.NewReadWriter(&bytes.Buffer{}, &out) // START: default color ON
	ac := &colorFakeAccount{}
	do := func(line string) {
		out.Reset()
		if !handleColorCommand(context.Background(), tc, ac, "acct-1", line, discardLogger()) {
			t.Fatalf("%q was not intercepted", line)
		}
	}

	// STATE ON. `color on` is REDUNDANT (already on): it still confirms, but must NOT persist.
	do("color on")
	if !tc.ColorEnabled() {
		t.Fatal("`color on` from ON must leave color ON")
	}
	if ac.setCalls != 0 {
		t.Fatalf("a redundant `color on` (already ON) must not persist, got calls=%d", ac.setCalls)
	}

	// ON -> OFF: a real change, persisted once as enabled=false; the confirmation is plain.
	do("color off")
	if tc.ColorEnabled() {
		t.Fatal("color still enabled after `color off`")
	}
	if got := out.String(); !strings.Contains(got, "OFF") || strings.ContainsRune(got, 0x1b) {
		t.Fatalf("`color off` confirmation should be plain and say OFF: %q", got)
	}
	if ac.setCalls != 1 || ac.lastEnabled {
		t.Fatalf("ON->OFF should persist enabled=false once, got calls=%d enabled=%v", ac.setCalls, ac.lastEnabled)
	}

	// STATE OFF. `color off` again is REDUNDANT (already off): NO second write — the amplification guard.
	do("color off")
	if tc.ColorEnabled() {
		t.Fatal("`color off` from OFF must leave color OFF")
	}
	if ac.setCalls != 1 {
		t.Fatalf("a redundant `color off` (already OFF) must not persist a second time, got calls=%d", ac.setCalls)
	}

	// OFF -> ON: a real change, persisted once as enabled=true; the confirmation renders in color (case-insensitive).
	do("COLOR On")
	if !tc.ColorEnabled() {
		t.Fatal("color still disabled after `color on`")
	}
	if got := out.String(); !strings.Contains(got, "\x1b[32m") {
		t.Fatalf("`color on` confirmation should be colored green: %q", got)
	}
	if ac.setCalls != 2 || !ac.lastEnabled {
		t.Fatalf("OFF->ON should persist enabled=true, got calls=%d enabled=%v", ac.setCalls, ac.lastEnabled)
	}

	// STATE ON. `color on` again is REDUNDANT: no third write.
	do("color on")
	if ac.setCalls != 2 {
		t.Fatalf("a redundant `color on` (already ON) must not persist, got calls=%d", ac.setCalls)
	}

	// bare `color` reports status without changing it AND without persisting (a query, not a mutation).
	do("  color  ")
	if !tc.ColorEnabled() {
		t.Fatal("bare `color` must not change the setting")
	}
	if got := out.String(); !strings.Contains(got, "currently on") {
		t.Fatalf("status line wrong: %q", got)
	}
	if ac.setCalls != 2 {
		t.Fatalf("bare `color` status must not persist, but SetColorPref was called %d times total", ac.setCalls)
	}

	// Non-color lines are NOT intercepted (forwarded to the world) and never persist.
	for _, line := range []string{"say hello", "colorful sunset", "recolor", ""} {
		if handleColorCommand(context.Background(), tc, ac, "acct-1", line, discardLogger()) {
			t.Fatalf("%q must not be intercepted as a color command", line)
		}
	}
	if ac.setCalls != 2 {
		t.Fatalf("non-color lines must not persist; SetColorPref calls=%d", ac.setCalls)
	}
}

// TestHandleColorCommandStubNoPersist: the bare-name / dev-autoauth path carries accountID=="" — the toggle
// still applies in-session but there is NO account to persist against, so the write is skipped.
func TestHandleColorCommandStubNoPersist(t *testing.T) {
	var out bytes.Buffer
	tc := telnet.NewReadWriter(&bytes.Buffer{}, &out)
	ac := &colorFakeAccount{}

	if !handleColorCommand(context.Background(), tc, ac, "", "color off", discardLogger()) {
		t.Fatal("`color off` was not intercepted")
	}
	if tc.ColorEnabled() {
		t.Fatal("the in-session toggle must still apply on the stub path")
	}
	if ac.setCalls != 0 {
		t.Fatalf("an empty accountID must skip persistence, got calls=%d", ac.setCalls)
	}
}

// TestHandleColorCommandStubClientNoPersist: even a real accountID against the stub client is a no-op write
// (the stub has no store) — the dev fallback keeps working with the default.
func TestHandleColorCommandStubClientNoPersist(t *testing.T) {
	var out bytes.Buffer
	tc := telnet.NewReadWriter(&bytes.Buffer{}, &out)

	// The bare stub's SetColorPref is a no-op returning nil; the command still succeeds.
	if !handleColorCommand(context.Background(), tc, stubAccountClient{}, "acct-1", "color off", discardLogger()) {
		t.Fatal("`color off` was not intercepted")
	}
	if tc.ColorEnabled() {
		t.Fatal("the in-session toggle must apply even against the stub client")
	}
	// And the stub's read reports "never set" so a connect-time restore keeps the default.
	_, set, err := stubAccountClient{}.GetColorPref(context.Background(), "acct-1")
	if err != nil || set {
		t.Fatalf("stub GetColorPref must report never-set with no error, got set=%v err=%v", set, err)
	}
}

// TestHandleColorCommandPersistErrorDoesNotBreak: a persist failure is logged + swallowed — the in-session
// toggle has already applied, so the command still succeeds and is still consumed (not leaked to the world).
func TestHandleColorCommandPersistErrorDoesNotBreak(t *testing.T) {
	var out bytes.Buffer
	tc := telnet.NewReadWriter(&bytes.Buffer{}, &out)
	ac := &colorFakeAccount{setErr: errors.New("account service down")}

	handled := handleColorCommand(context.Background(), tc, ac, "acct-1", "color off", discardLogger())
	if !handled {
		t.Fatal("a persist error must not stop the command being consumed")
	}
	if tc.ColorEnabled() {
		t.Fatal("the toggle must still apply even when the persist write fails")
	}
	if ac.setCalls != 1 {
		t.Fatalf("SetColorPref should have been attempted once, got %d", ac.setCalls)
	}
	if got := out.String(); strings.Contains(got, "down") {
		t.Fatalf("the raw persist error must not leak to the player: %q", got)
	}
}

// TestColorPrefReconnectRoundTrip is the #23 intent, at the gate level: a saved `color off` preference is read
// back on the next connect and applied to the fresh terminal BEFORE the first world frame. We simulate the
// two legs (write on session A, read + apply on session B) against one shared account fake.
func TestColorPrefReconnectRoundTrip(t *testing.T) {
	ac := &colorFakeAccount{}

	// Session A: default-on terminal, player runs `color off` — persisted.
	tcA := telnet.NewReadWriter(&bytes.Buffer{}, &bytes.Buffer{})
	handleColorCommand(context.Background(), tcA, ac, "acct-1", "color off", discardLogger())
	if ac.setCalls != 1 || ac.lastEnabled {
		t.Fatalf("session A should persist color off, got calls=%d enabled=%v", ac.setCalls, ac.lastEnabled)
	}

	// Reconnect: the account now reports the stored preference.
	ac.getSet, ac.getEnabled = true, ac.lastEnabled

	// Session B: a fresh terminal starts default-ON; the connect-time restore applies the stored preference.
	tcB := telnet.NewReadWriter(&bytes.Buffer{}, &bytes.Buffer{})
	if !tcB.ColorEnabled() {
		t.Fatal("a fresh terminal should start with the default (color ON) before restore")
	}
	enabled, set, err := ac.GetColorPref(context.Background(), "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if set {
		tcB.SetColor(enabled)
	}
	if tcB.ColorEnabled() {
		t.Fatal("session B should have color OFF after restoring the saved preference")
	}
}
