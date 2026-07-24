package world

import (
	"math/rand"
	"testing"
)

// dice_fuzz_test.go property-tests the dice parser + roller as one unit.
//
// The kept-face split (#511) added a third return whose relationship to the other two is an INVARIANT
// rather than a case: for a keep spec exactly `keep` faces survive and their sum IS the magnitude; for
// every other kind nothing is discarded, so the kept set is every face. Hand-picked notations can only
// sample that; a property covers the whole grammar, including the shapes no author has written yet.
//
// It also guards the two clamps that are individually unreachable from the parser (parseKeep bounds
// keep <= num, so sumKept's own clamp never fires today) and are therefore invisible to ordinary tests
// — the class where removing BOTH at once panics the zone goroutine on `3d6kh9` even though removing
// either alone is harmless.

func FuzzDiceSpecRoll(f *testing.F) {
	for _, s := range []string{
		"1d20", "2d20kh1", "2d20kl1", "4dF", "5d6>4", "5d6>=5", "d6", "3d20kh", "0d6", "1d1",
		"4d6kh3", "3d6kh9", "100d100kl50", "999999d999999kh1", "2d6>=7", "1dF",
		"", "d", "xdy", "1d0", "-1d6", "1d20kh0", "1d20kh-1", "2d20kh99",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, notation string) {
		d, err := parseDiceSpec(notation)
		if err != nil {
			return // a rejected expression is a fine outcome; it must simply not panic
		}
		if d.num < 0 || d.num > maxDice {
			t.Fatalf("parsed num %d outside [0,%d] for %q", d.num, maxDice, notation)
		}
		if d.size < 0 || d.size > maxDice {
			t.Fatalf("parsed size %d outside [0,%d] for %q", d.size, maxDice, notation)
		}
		if d.keep < 0 || d.keep > d.num {
			t.Fatalf("parsed keep %d outside [0,num=%d] for %q", d.keep, d.num, notation)
		}

		mag, faces, kept := rollDiceSpec(&effectCtx{rng: rand.New(rand.NewSource(1))}, d)

		if len(faces) != d.num {
			t.Fatalf("rolled %d faces, spec declares %d, for %q", len(faces), d.num, notation)
		}
		switch d.kind {
		case diceKeepHigh, diceKeepLow:
			// The kept set is exactly the surviving dice, and it alone accounts for the magnitude.
			if len(kept) != d.keep {
				t.Fatalf("kept %d faces, spec keeps %d, for %q", len(kept), d.keep, notation)
			}
			sum := 0
			for _, v := range kept {
				sum += v
			}
			if sum != mag {
				t.Fatalf("kept sum %d != magnitude %d for %q (faces %v, kept %v)", sum, mag, notation, faces, kept)
			}
		default:
			// Nothing is discarded, so the kept set IS every face — the property behind the "zero change
			// for existing content" claim of the face_eq fix, held over the whole grammar rather than
			// over five hand-picked notations.
			if len(kept) != len(faces) {
				t.Fatalf("kind %v discarded nothing yet kept %d of %d faces for %q",
					d.kind, len(kept), len(faces), notation)
			}
		}
		for _, fc := range faces {
			switch d.kind {
			case diceFudge:
				if fc < -1 || fc > 1 {
					t.Fatalf("Fudge face %d outside [-1,1] for %q", fc, notation)
				}
			default:
				if d.size > 0 && (fc < 1 || fc > d.size) {
					t.Fatalf("face %d outside [1,%d] for %q", fc, d.size, notation)
				}
			}
		}
	})
}
