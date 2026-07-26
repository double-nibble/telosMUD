package content

import (
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
)

// chargen_roll.go — the ability-score dice for the `roll` chargen step (#518). It lives in the content
// package (unlike the world dice, which are unreachable one-directional-dep from here) because ValidateChargen
// rolls a roll-step's scores itself, server-side, at submit — never trusting a client value. The rng is
// injected by the caller (the account service), so the roll is authoritative and the roller stays testable.

// defaultRollDice is the classic 5e ability roll: 4d6, drop the lowest die, sum the top three.
const defaultRollDice = "4d6dl1"

// rollAbilityScore rolls one ability score from the spec "<N>d<S>[dl<K>]" (roll N dice of S sides, drop the
// K lowest, sum the rest) using rng. "" defaults to 4d6dl1. Returns an error for a malformed spec so a
// mis-authored flow fails loudly rather than producing a garbage score.
func rollAbilityScore(rng *rand.Rand, spec string) (int, error) {
	num, size, dropLow, err := parseRollSpec(spec)
	if err != nil {
		return 0, err
	}
	rolls := make([]int, num)
	for i := range rolls {
		rolls[i] = rng.Intn(size) + 1
	}
	sort.Ints(rolls) // ascending, so the first dropLow entries are the lowest to drop
	sum := 0
	for i := dropLow; i < num; i++ {
		sum += rolls[i]
	}
	return sum, nil
}

// parseRollSpec parses "<N>d<S>[dl<K>]" into (num, size, dropLow). Bounds keep a mis-authored spec from
// allocating a huge roll. dropLow must be < num (dropping every die is meaningless).
func parseRollSpec(spec string) (num, size, dropLow int, err error) {
	s := strings.ToLower(strings.TrimSpace(spec))
	if s == "" {
		s = defaultRollDice
	}
	if i := strings.Index(s, "dl"); i >= 0 {
		k, e := strconv.Atoi(s[i+2:])
		if e != nil || k < 0 {
			return 0, 0, 0, fmt.Errorf("chargen roll_dice %q: bad drop-lowest", spec)
		}
		dropLow = k
		s = s[:i]
	}
	parts := strings.SplitN(s, "d", 2)
	if len(parts) != 2 {
		return 0, 0, 0, fmt.Errorf("chargen roll_dice %q: want <N>d<S>[dl<K>]", spec)
	}
	num, err = strconv.Atoi(parts[0])
	if err != nil || num <= 0 || num > 100 {
		return 0, 0, 0, fmt.Errorf("chargen roll_dice %q: bad dice count", spec)
	}
	size, err = strconv.Atoi(parts[1])
	if err != nil || size <= 0 || size > 1000 {
		return 0, 0, 0, fmt.Errorf("chargen roll_dice %q: bad dice size", spec)
	}
	if dropLow >= num {
		return 0, 0, 0, fmt.Errorf("chargen roll_dice %q: drops every die", spec)
	}
	return num, size, dropLow, nil
}
