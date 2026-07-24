package world

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/double-nibble/telosmud/internal/logcap"
)

// dice.go is the CONTENT dice-notation parser + roller for the check primitive (docs/PHASE6-PLAN.md
// §1.1, [G2a]). It SUPERSEDES the simple "<N>d<S>" parseDice (ability_build.go) with the richer
// notation a check spec needs — keep-highest/lowest (advantage/disadvantage), Fudge, and
// count-successes pools — while the engine stays IGNORANT of dice *shape*: a check rolls a
// content-named expression and the band classifier (check.go) reads only the resulting magnitude.
//
//	1d20        -> sum of 1d20                          (the d20 core; "d20" == "1d20")
//	2d20kh1     -> roll 2d20, keep the highest 1, sum   (5e advantage)
//	2d20kl1     -> roll 2d20, keep the lowest 1, sum    (5e disadvantage)
//	4dF         -> 4 Fudge dice, each −1/0/+1, sum      (FATE)
//	5d6>4       -> roll 5d6, COUNT the dice showing > 4 (Blades/Year-Zero-style pool successes)
//	5d6>=5      -> roll 5d6, COUNT the dice showing >= 5
//
// deal_damage keeps its own parseDice (the plain <N>d<S> sum) this slice — a later slice may route
// both through diceSpec; they share the maxDice cap so neither can spin the zone goroutine.

// diceKind is how a roll's faces collapse to a single magnitude.
type diceKind int

const (
	diceSum      diceKind = iota // sum every die (NdS, dS)
	diceKeepHigh                 // roll num, keep the highest `keep`, sum
	diceKeepLow                  // roll num, keep the lowest `keep`, sum
	diceFudge                    // num Fudge dice (each −1/0/+1), sum; size ignored
	dicePool                     // roll num dS, COUNT faces meeting the pool comparator
)

// diceSpec is a parsed dice expression. Immutable after parse (build-time), shareable across zone
// goroutines; the per-roll randomness comes from the effectCtx rng at roll time.
type diceSpec struct {
	num       int      // number of dice
	size      int      // die size (faces 1..size); 0 for Fudge
	kind      diceKind //
	keep      int      // keepHigh/keepLow: how many to keep
	threshold int      // pool: the comparator right-hand side
	poolGTE   bool     // pool: true => count faces >= threshold; false => strictly > threshold
	raw       string   // the original notation (for emission/logging)
}

// parseDiceSpec parses content dice notation into a diceSpec. An empty string is an error (a check
// must declare its dice). Both the count and the size are capped at maxDice so a runaway spec can't
// spin the single-writer zone goroutine. Build-time only.
func parseDiceSpec(s string) (diceSpec, error) {
	raw := strings.TrimSpace(s)
	t := strings.ToLower(raw)
	if t == "" {
		return diceSpec{}, fmt.Errorf("dice: empty expression")
	}

	// Fudge: NdF (e.g. "4df"). The size term is the literal "f".
	if strings.Contains(t, "df") {
		left := strings.TrimSuffix(t, "df")
		num, err := parseDiceCount(left, raw)
		if err != nil {
			return diceSpec{}, err
		}
		return diceSpec{num: capDice(num), kind: diceFudge, raw: raw}, nil
	}

	// Pool: NdS>T or NdS>=T — count successes.
	if i := strings.IndexByte(t, '>'); i >= 0 {
		base, gte, thr, err := splitPool(t[:i], t[i:], raw)
		if err != nil {
			return diceSpec{}, err
		}
		base.kind = dicePool
		base.threshold = thr
		base.poolGTE = gte
		base.raw = raw
		return base, nil
	}

	// Keep-highest / keep-lowest: NdSkhK / NdSklK (K defaults to 1).
	if i := strings.Index(t, "kh"); i >= 0 {
		return parseKeep(t[:i], t[i+2:], diceKeepHigh, raw)
	}
	if i := strings.Index(t, "kl"); i >= 0 {
		return parseKeep(t[:i], t[i+2:], diceKeepLow, raw)
	}

	// Plain NdS.
	num, size, err := parseNdS(t, raw)
	if err != nil {
		return diceSpec{}, err
	}
	return diceSpec{num: num, size: size, kind: diceSum, raw: raw}, nil
}

// parseKeep parses the "NdS" left of a kh/kl plus the keep-count right of it.
func parseKeep(left, keepStr string, kind diceKind, raw string) (diceSpec, error) {
	num, size, err := parseNdS(left, raw)
	if err != nil {
		return diceSpec{}, err
	}
	keep := 1
	if strings.TrimSpace(keepStr) != "" {
		k, err := strconv.Atoi(strings.TrimSpace(keepStr))
		if err != nil || k < 1 {
			return diceSpec{}, fmt.Errorf("dice %q: bad keep count", logcap.Value(raw))
		}
		keep = k
	}
	if keep > num {
		keep = num // keeping more than rolled == keeping all
	}
	return diceSpec{num: num, size: size, kind: kind, keep: keep, raw: raw}, nil
}

// splitPool parses the NdS base (left of '>') and the comparator (">T" or ">=T").
func splitPool(left, cmp, raw string) (diceSpec, bool, int, error) {
	num, size, err := parseNdS(left, raw)
	if err != nil {
		return diceSpec{}, false, 0, err
	}
	gte := false
	rhs := strings.TrimPrefix(cmp, ">")
	if strings.HasPrefix(rhs, "=") {
		gte = true
		rhs = strings.TrimPrefix(rhs, "=")
	}
	thr, err := strconv.Atoi(strings.TrimSpace(rhs))
	if err != nil {
		return diceSpec{}, false, 0, fmt.Errorf("dice %q: bad pool threshold", logcap.Value(raw))
	}
	return diceSpec{num: num, size: size}, gte, thr, nil
}

// parseNdS parses the core "<N>d<S>" (N defaults to 1; "d6" == "1d6"), capping both terms.
func parseNdS(s, raw string) (int, int, error) {
	parts := strings.SplitN(strings.TrimSpace(s), "d", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("dice %q: want <N>d<S>", logcap.Value(raw))
	}
	num, err := parseDiceCount(parts[0], raw)
	if err != nil {
		return 0, 0, err
	}
	size, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || size <= 0 {
		return 0, 0, fmt.Errorf("dice %q: bad size", logcap.Value(raw))
	}
	return capDice(num), capDice(size), nil
}

// parseDiceCount parses an optional leading count ("" => 1), rejecting negatives.
func parseDiceCount(s, raw string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("dice %q: bad count", logcap.Value(raw))
	}
	return n, nil
}

func capDice(n int) int {
	if n > maxDice {
		return maxDice
	}
	return n
}

// rollDiceSpec rolls d and collapses it to a single magnitude per its kind, using the ctx rng when
// present (tests inject a seeded rng for determinism; production uses the package default). It
// returns THREE things: the magnitude, ALL natural faces rolled, and the KEPT faces — the subset that
// actually contributed to the magnitude. Single-writer: zone goroutine.
//
// # Why "kept" is a separate return and not just `faces`
//
// For every kind except keepHigh/keepLow the two are identical (nothing is discarded), so the
// distinction only exists for a keep spec — but there it is the difference between a correct and an
// incorrect nat-face band. A `{face_eq: 1 -> miss}` band tested against ALL faces fires when EITHER
// die of a 2d20kh1 shows a 1 (9.36% measured), when the roll the check actually used was the OTHER,
// higher die; the same band tested against the KEPT face fires at 0.25%, which is what "the die you
// rolled was a 1" means. Emission and logging still want every face (a player wants to see both dice),
// so both are reported and the caller picks: checkBand.matches reads KEPT, emitCheck reads all.
//
// The kept slice aliases nothing the caller can mutate through — sumKept copies before sorting, and
// the non-keep kinds return the same backing array as `faces`, which no caller writes to.
func rollDiceSpec(c *effectCtx, d diceSpec) (magnitude int, faces, kept []int) {
	rollFace := func(size int) int {
		if size <= 0 {
			return 0
		}
		if c != nil && c.rng != nil {
			return c.rng.Intn(size) + 1
		}
		return randIntn(size) + 1
	}

	switch d.kind {
	case diceFudge:
		faces := make([]int, 0, d.num)
		sum := 0
		for i := 0; i < d.num; i++ {
			f := rollFace(3) - 2 // 1..3 -> −1,0,+1
			faces = append(faces, f)
			sum += f
		}
		return sum, faces, faces // nothing is discarded: every face contributed

	case dicePool:
		faces := make([]int, 0, d.num)
		count := 0
		for i := 0; i < d.num; i++ {
			f := rollFace(d.size)
			faces = append(faces, f)
			if (d.poolGTE && f >= d.threshold) || (!d.poolGTE && f > d.threshold) {
				count++
			}
		}
		// Every face is rolled AND read (a non-succeeding pool die still counted toward the tally, it just
		// contributed 0), so nothing is "discarded" in the keep sense — kept == faces. A pool's own
		// success threshold is not a keep filter; treating it as one would make face_eq on a pool blind to
		// the failures, which is not what a pool's nat-face band means.
		return count, faces, faces

	case diceKeepHigh, diceKeepLow:
		faces := make([]int, 0, d.num)
		for i := 0; i < d.num; i++ {
			faces = append(faces, rollFace(d.size))
		}
		sum, kept := sumKept(faces, d.keep, d.kind == diceKeepHigh)
		return sum, faces, kept

	default: // diceSum
		faces := make([]int, 0, d.num)
		sum := 0
		for i := 0; i < d.num; i++ {
			f := rollFace(d.size)
			faces = append(faces, f)
			sum += f
		}
		return sum, faces, faces // nothing is discarded: every face contributed
	}
}

// sumKept sums the highest (or lowest) `keep` of faces without mutating the caller's slice, and
// returns the kept faces alongside the sum. The kept slice is a fresh allocation (a sub-slice of the
// sorted COPY), so the caller can neither mutate nor observe the sort through it.
func sumKept(faces []int, keep int, high bool) (int, []int) {
	if keep <= 0 || len(faces) == 0 {
		return 0, nil
	}
	cp := make([]int, len(faces))
	copy(cp, faces)
	// Simple selection: sort ascending, then take from the right (high) or left (low). keep<=num.
	for i := 0; i < len(cp); i++ {
		for j := i + 1; j < len(cp); j++ {
			if cp[j] < cp[i] {
				cp[i], cp[j] = cp[j], cp[i]
			}
		}
	}
	if keep > len(cp) {
		keep = len(cp)
	}
	var kept []int
	if high {
		kept = cp[len(cp)-keep:]
	} else {
		kept = cp[:keep]
	}
	sum := 0
	for _, v := range kept {
		sum += v
	}
	return sum, kept
}
