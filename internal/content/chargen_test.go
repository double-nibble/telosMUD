package content

import (
	"math/rand"
	"testing"
)

// chargen_test.go — the pure chargen submission validator (Phase 14.8b).

func demoChargenFlow() ChargenDTO {
	return ChargenDTO{
		Ref: "t:chargen",
		Steps: []ChargenStepDTO{
			{Kind: "bundle_choice", ID: "race", BundleKind: "race", Pick: 1},
			{Kind: "bundle_choice", ID: "class", BundleKind: "class", Pick: 1},
			{
				Kind: "point_buy", ID: "attrs",
				Attributes: []string{"strength", "intellect", "constitution"},
				Points:     27, Base: 8, Min: 8, Max: 15,
				Cost: map[string]int{"8": 0, "9": 1, "10": 2, "11": 3, "12": 4, "13": 5, "14": 7, "15": 9},
			},
		},
	}
}

var demoBundleKind = map[string]string{"elf": "race", "dwarf": "race", "fighter": "class", "mage": "class"}

func TestValidateChargenValid(t *testing.T) {
	picks := map[string]string{"race": "elf", "class": "fighter"}
	allocs := map[string]map[string]int{"attrs": {"strength": 15, "intellect": 13, "constitution": 13}} // 9+5+5=19 <= 27
	bundles, attrs, reason := ValidateChargen(demoChargenFlow(), picks, allocs, demoBundleKind, nil)
	if reason != "" {
		t.Fatalf("valid submission rejected: %s", reason)
	}
	if len(bundles) != 2 || bundles[0] != "elf" || bundles[1] != "fighter" {
		t.Fatalf("bundles = %v, want [elf fighter]", bundles)
	}
	if attrs["strength"] != 15 || attrs["constitution"] != 13 {
		t.Fatalf("attrs = %v, want str 15 / con 13", attrs)
	}
}

func TestValidateChargenBudget(t *testing.T) {
	// A tighter 10-point flow so over-budget is reachable (the demo's 27 == 3*max is unreachable-to-exceed).
	flow := ChargenDTO{Steps: []ChargenStepDTO{{
		Kind: "point_buy", ID: "attrs", Attributes: []string{"strength", "intellect"},
		Points: 10, Base: 8, Min: 8, Max: 15,
		Cost: map[string]int{"8": 0, "12": 4, "14": 7, "15": 9},
	}}}
	// 4 + 4 = 8 <= 10: allowed.
	if _, _, reason := ValidateChargen(flow, nil, map[string]map[string]int{"attrs": {"strength": 12, "intellect": 12}}, nil, nil); reason != "" {
		t.Fatalf("an 8-point allocation under the 10 budget should be allowed, got %q", reason)
	}
	// 7 + 7 = 14 > 10: rejected.
	if _, _, reason := ValidateChargen(flow, nil, map[string]map[string]int{"attrs": {"strength": 14, "intellect": 14}}, nil, nil); reason == "" {
		t.Fatal("a 14-point allocation over the 10 budget must be rejected")
	}
}

func TestValidateChargenRejections(t *testing.T) {
	flow := demoChargenFlow()
	full := map[string]map[string]int{"attrs": {"strength": 10, "intellect": 10, "constitution": 10}}

	// Missing race pick.
	if _, _, reason := ValidateChargen(flow, map[string]string{"class": "fighter"}, full, demoBundleKind, nil); reason == "" {
		t.Fatal("a missing race pick should be rejected")
	}
	// Wrong-kind pick (a class ref where a race is required).
	if _, _, reason := ValidateChargen(flow, map[string]string{"race": "fighter", "class": "fighter"}, full, demoBundleKind, nil); reason == "" {
		t.Fatal("a class ref in the race slot should be rejected")
	}
	// Out-of-bounds attribute.
	if _, _, reason := ValidateChargen(flow, map[string]string{"race": "elf", "class": "fighter"},
		map[string]map[string]int{"attrs": {"strength": 99, "intellect": 8, "constitution": 8}}, demoBundleKind, nil); reason == "" {
		t.Fatal("an out-of-bounds attribute should be rejected")
	}
	// Over budget.
	if _, _, reason := ValidateChargen(flow, map[string]string{"race": "elf", "class": "fighter"},
		map[string]map[string]int{"attrs": {"strength": 15, "intellect": 15, "constitution": 15}}, demoBundleKind, nil); reason != "" {
		t.Fatalf("27 points should be allowed, got %q", reason)
	}
}

// arrayFlow is a standard-array (#518) step over three abilities.
func arrayFlow() ChargenDTO {
	return ChargenDTO{Steps: []ChargenStepDTO{{
		Kind: "array_assign", ID: "attrs",
		Attributes: []string{"strength", "intellect", "constitution"},
		Array:      []int{15, 13, 10},
	}}}
}

// TestValidateChargenArrayAssign (#518): a valid permutation of the array is accepted and applied.
func TestValidateChargenArrayAssign(t *testing.T) {
	allocs := map[string]map[string]int{"attrs": {"strength": 15, "intellect": 10, "constitution": 13}}
	_, attrs, reason := ValidateChargen(arrayFlow(), nil, allocs, nil, nil)
	if reason != "" {
		t.Fatalf("a valid array permutation was rejected: %s", reason)
	}
	if attrs["strength"] != 15 || attrs["intellect"] != 10 || attrs["constitution"] != 13 {
		t.Fatalf("array assignment applied wrong: %v", attrs)
	}
}

// TestValidateChargenArrayRejections (#518): an off-array value, a reused value, and a missing assignment
// are each rejected.
func TestValidateChargenArrayRejections(t *testing.T) {
	cases := []struct {
		name   string
		allocs map[string]int
	}{
		{"off-array value", map[string]int{"strength": 18, "intellect": 10, "constitution": 13}},
		{"reused value", map[string]int{"strength": 15, "intellect": 15, "constitution": 13}}, // 15 used twice, 10 unused
		{"missing assignment", map[string]int{"strength": 15, "intellect": 10}},               // constitution unset
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, reason := ValidateChargen(arrayFlow(), nil, map[string]map[string]int{"attrs": tc.allocs}, nil, nil)
			if reason == "" {
				t.Fatalf("%s should be rejected", tc.name)
			}
		})
	}
}

// TestValidateChargenRoll (#518): a roll step rolls each attribute server-side; every score is within the
// 4d6-drop-lowest range [3,18]; and a nil rng on a roll flow is reported, not panicked.
func TestValidateChargenRoll(t *testing.T) {
	flow := ChargenDTO{Steps: []ChargenStepDTO{{
		Kind: "roll", ID: "attrs", Attributes: []string{"strength", "intellect", "constitution"},
	}}}
	rng := rand.New(rand.NewSource(1))
	_, attrs, reason := ValidateChargen(flow, nil, nil, nil, rng)
	if reason != "" {
		t.Fatalf("roll flow rejected: %s", reason)
	}
	for _, a := range []string{"strength", "intellect", "constitution"} {
		v := attrs[a]
		if v < 3 || v > 18 {
			t.Fatalf("%s rolled %v, outside the 4d6-drop-lowest range [3,18]", a, v)
		}
	}
	// A roll flow with a nil rng is a clean refusal, not a panic.
	if _, _, reason := ValidateChargen(flow, nil, nil, nil, nil); reason == "" {
		t.Fatal("a roll flow with a nil rng must be refused")
	}
}

// TestLintChargen (#518): a mis-authored array_assign (size mismatch) and a bad roll_dice spec are flagged
// at load; a well-formed flow is clean.
func TestLintChargen(t *testing.T) {
	packs := []Pack{{
		Pack: "bad",
		Chargens: []ChargenDTO{{Ref: "bad:chargen", Steps: []ChargenStepDTO{
			{Kind: "array_assign", ID: "arr", Attributes: []string{"str", "dex", "con"}, Array: []int{15, 10}}, // 2 != 3
			{Kind: "roll", ID: "rolled", Attributes: []string{"wis"}, RollDice: "nonsense"},
			{Kind: "point_buy", ID: "pb"}, // ignored by the lint
		}}},
	}}
	got := LintChargen(packs)
	if len(got) != 2 {
		t.Fatalf("got %d violations, want 2: %+v", len(got), got)
	}
	byField := map[string]ChargenViolation{}
	for _, v := range got {
		byField[v.Field] = v
	}
	if byField["array"].Step != "arr" || byField["roll_dice"].Step != "rolled" {
		t.Fatalf("violations mis-attributed: %+v", got)
	}
	// A well-formed flow (matched array, default roll) is clean.
	good := []Pack{{Pack: "ok", Chargens: []ChargenDTO{{Ref: "ok", Steps: []ChargenStepDTO{
		{Kind: "array_assign", ID: "a", Attributes: []string{"str", "dex"}, Array: []int{15, 10}},
		{Kind: "roll", ID: "r", Attributes: []string{"con"}}, // default 4d6dl1
	}}}}}
	if v := LintChargen(good); len(v) != 0 {
		t.Fatalf("a well-formed flow flagged: %+v", v)
	}
}

// TestRollAbilityScore (#518): the dice roller respects the spec bounds and default.
func TestRollAbilityScore(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	// Default 4d6dl1 → [3,18].
	for i := 0; i < 200; i++ {
		v, err := rollAbilityScore(rng, "")
		if err != nil {
			t.Fatalf("default roll errored: %v", err)
		}
		if v < 3 || v > 18 {
			t.Fatalf("4d6dl1 rolled %d, outside [3,18]", v)
		}
	}
	// 3d6 (no drop) → [3,18], and a deterministic 1d1dl0 → exactly 1.
	if v, err := rollAbilityScore(rng, "1d1"); err != nil || v != 1 {
		t.Fatalf("1d1 = (%d,%v), want (1,nil)", v, err)
	}
	// DROP-LOWEST DIRECTION: 4d6-drop-lowest has a mean ≈ 12.24; drop-HIGHEST would be ≈ 9.76. A range-only
	// [3,18] check can't tell them apart, so pin the direction by the sample mean over many seeded rolls.
	{
		mrng := rand.New(rand.NewSource(7))
		const n = 20000
		sum := 0
		for i := 0; i < n; i++ {
			v, _ := rollAbilityScore(mrng, "4d6dl1")
			sum += v
		}
		mean := float64(sum) / n
		if mean < 11.8 || mean > 12.7 {
			t.Fatalf("4d6dl1 mean = %.3f, want ≈12.24 (drop-LOWEST); ~9.76 would mean it drops the highest", mean)
		}
	}
	// Malformed specs error — including a zero/negative SIZE, which would otherwise panic rng.Intn(0).
	for _, bad := range []string{"abc", "4x6", "0d6", "4d0", "4d-1", "4d6dl4", "4d6dl-1"} {
		if _, err := rollAbilityScore(rng, bad); err == nil {
			t.Fatalf("spec %q should be rejected", bad)
		}
	}
}
