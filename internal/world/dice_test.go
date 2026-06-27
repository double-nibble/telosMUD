package world

import (
	"math/rand"
	"testing"
)

// dice_test.go exercises the content dice-notation parser + roller (dice.go [G2a]): the plain NdS sum,
// keep-highest/lowest (advantage/disadvantage), Fudge, and count-successes pools — plus determinism
// under a seeded rng and the maxDice cap.

func fp(f float64) *float64 { return &f }

func TestParseDiceSpec(t *testing.T) {
	cases := []struct {
		in   string
		num  int
		size int
		kind diceKind
		keep int
		thr  int
		gte  bool
	}{
		{"1d20", 1, 20, diceSum, 0, 0, false},
		{"d6", 1, 6, diceSum, 0, 0, false},
		{"8d6", 8, 6, diceSum, 0, 0, false},
		{"2d20kh1", 2, 20, diceKeepHigh, 1, 0, false},
		{"2d20kl1", 2, 20, diceKeepLow, 1, 0, false},
		{"3d20kh", 3, 20, diceKeepHigh, 1, 0, false}, // K defaults to 1
		{"4dF", 4, 0, diceFudge, 0, 0, false},
		{"dF", 1, 0, diceFudge, 0, 0, false},
		{"5d6>4", 5, 6, dicePool, 0, 4, false},
		{"6d10>=8", 6, 10, dicePool, 0, 8, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			d, err := parseDiceSpec(c.in)
			if err != nil {
				t.Fatalf("parseDiceSpec(%q): %v", c.in, err)
			}
			if d.num != c.num || d.size != c.size || d.kind != c.kind || d.keep != c.keep ||
				d.threshold != c.thr || d.poolGTE != c.gte {
				t.Fatalf("parseDiceSpec(%q) = %+v, want num=%d size=%d kind=%d keep=%d thr=%d gte=%v",
					c.in, d, c.num, c.size, c.kind, c.keep, c.thr, c.gte)
			}
		})
	}
}

func TestParseDiceSpecErrors(t *testing.T) {
	for _, in := range []string{"", "20", "d", "xdy", "2d20kh-1", "5d6>x"} {
		if _, err := parseDiceSpec(in); err == nil {
			t.Fatalf("parseDiceSpec(%q): want error, got nil", in)
		}
	}
}

func TestParseDiceSpecCaps(t *testing.T) {
	d, err := parseDiceSpec("9999d9999")
	if err != nil {
		t.Fatalf("parseDiceSpec: %v", err)
	}
	if d.num != maxDice || d.size != maxDice {
		t.Fatalf("caps not applied: num=%d size=%d, want %d/%d", d.num, d.size, maxDice, maxDice)
	}
}

// rollSpecCtx rolls notation with a freshly seeded rng (deterministic).
func rollSpecCtx(t *testing.T, notation string, seed int64) (int, []int) {
	t.Helper()
	d, err := parseDiceSpec(notation)
	if err != nil {
		t.Fatalf("parseDiceSpec(%q): %v", notation, err)
	}
	c := &effectCtx{rng: rand.New(rand.NewSource(seed))}
	return rollDiceSpec(c, d)
}

func TestRollDiceSpecD1Deterministic(t *testing.T) {
	// A d1 always shows 1: a clean deterministic anchor for the classifier tests.
	got, faces := rollSpecCtx(t, "5d1", 1)
	if got != 5 {
		t.Fatalf("5d1 sum = %d, want 5", got)
	}
	if len(faces) != 5 {
		t.Fatalf("5d1 faces = %v, want 5 faces", faces)
	}
}

func TestRollDiceSpecPool(t *testing.T) {
	// 5d1>0: every face is 1, all strictly > 0 => 5 successes.
	if got, _ := rollSpecCtx(t, "5d1>0", 1); got != 5 {
		t.Fatalf("5d1>0 = %d, want 5 successes", got)
	}
	// 5d1>=2: every face is 1, none >= 2 => 0 successes.
	if got, _ := rollSpecCtx(t, "5d1>=2", 1); got != 0 {
		t.Fatalf("5d1>=2 = %d, want 0 successes", got)
	}
}

func TestRollDiceSpecFudgeBounds(t *testing.T) {
	got, faces := rollSpecCtx(t, "4dF", 7)
	if got < -4 || got > 4 {
		t.Fatalf("4dF sum = %d, want within [-4,4]", got)
	}
	for _, f := range faces {
		if f < -1 || f > 1 {
			t.Fatalf("Fudge face = %d, want within [-1,1]", f)
		}
	}
}

func TestRollDiceSpecSeededRepeatable(t *testing.T) {
	a, _ := rollSpecCtx(t, "2d20kh1", 42)
	b, _ := rollSpecCtx(t, "2d20kh1", 42)
	if a != b {
		t.Fatalf("same seed gave %d then %d; rolls must be repeatable", a, b)
	}
}

func TestSumKept(t *testing.T) {
	faces := []int{3, 18, 9}
	if got := sumKept(faces, 1, true); got != 18 {
		t.Fatalf("keep-high 1 of %v = %d, want 18", faces, got)
	}
	if got := sumKept(faces, 1, false); got != 3 {
		t.Fatalf("keep-low 1 of %v = %d, want 3", faces, got)
	}
	if got := sumKept(faces, 2, true); got != 27 {
		t.Fatalf("keep-high 2 of %v = %d, want 27", faces, got)
	}
	// Keeping more than rolled keeps all; the source slice must not be mutated.
	if got := sumKept(faces, 9, true); got != 30 {
		t.Fatalf("keep-high 9 of %v = %d, want 30 (all)", faces, got)
	}
	if faces[0] != 3 || faces[1] != 18 || faces[2] != 9 {
		t.Fatalf("sumKept mutated its input: %v", faces)
	}
}
