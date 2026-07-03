package world

import (
	"testing"
	"time"
)

// screen_test.go — #31 Slice 4: the engine-owned `clear` verb proving the trusted raw-screen path emits a
// Screen frame carrying the raw ANSI (which the gate writes verbatim, bypassing the sanitizer).

// TestClearEmitsScreenFrame: `clear` (and its `cls` alias) emit a Screen frame whose data is the engine
// clear-screen + cursor-home sequence.
func TestClearEmitsScreenFrame(t *testing.T) {
	want := ansiClearScreen + ansiCursorHome
	for _, verb := range []string{"clear", "cls"} {
		z, player := abilityTestZone(t)
		z.dispatch(player, verb)
		if got := drainForScreen(t, player); got != want {
			t.Fatalf("%q screen data = %q, want %q", verb, got, want)
		}
	}
}

// TestClearDoesNotShadowClose: `clear` is registered low-priority, so `cl` still abbreviates to the
// container verb `close`, not `clear` (which needs `clea`).
func TestClearDoesNotShadowClose(t *testing.T) {
	clr, ok := baseTable.resolve("clear")
	if !ok || clr.Name != "clear" {
		t.Fatal("clear must be registered")
	}
	if cl, ok := baseTable.resolve("cl"); ok && cl.Name == "clear" {
		t.Errorf("`cl` must not abbreviate to clear (should resolve to a higher-priority verb like close), got %q", cl.Name)
	}
	if clea, ok := baseTable.resolve("clea"); !ok || clea.Name != "clear" {
		t.Error("`clea` should abbreviate to clear")
	}
}

// drainForScreen drains a session's out channel for up to a short deadline, returning the first Screen
// frame's data as a string, or failing.
func drainForScreen(t *testing.T, s *session) string {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case f := <-s.out:
			if sc := f.GetScreen(); sc != nil {
				return string(sc.GetData())
			}
		case <-deadline:
			t.Fatal("no Screen frame emitted")
			return ""
		}
	}
}
