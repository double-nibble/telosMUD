package content

import "testing"

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
	bundles, attrs, reason := ValidateChargen(demoChargenFlow(), picks, allocs, demoBundleKind)
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
	if _, _, reason := ValidateChargen(flow, nil, map[string]map[string]int{"attrs": {"strength": 12, "intellect": 12}}, nil); reason != "" {
		t.Fatalf("an 8-point allocation under the 10 budget should be allowed, got %q", reason)
	}
	// 7 + 7 = 14 > 10: rejected.
	if _, _, reason := ValidateChargen(flow, nil, map[string]map[string]int{"attrs": {"strength": 14, "intellect": 14}}, nil); reason == "" {
		t.Fatal("a 14-point allocation over the 10 budget must be rejected")
	}
}

func TestValidateChargenRejections(t *testing.T) {
	flow := demoChargenFlow()
	full := map[string]map[string]int{"attrs": {"strength": 10, "intellect": 10, "constitution": 10}}

	// Missing race pick.
	if _, _, reason := ValidateChargen(flow, map[string]string{"class": "fighter"}, full, demoBundleKind); reason == "" {
		t.Fatal("a missing race pick should be rejected")
	}
	// Wrong-kind pick (a class ref where a race is required).
	if _, _, reason := ValidateChargen(flow, map[string]string{"race": "fighter", "class": "fighter"}, full, demoBundleKind); reason == "" {
		t.Fatal("a class ref in the race slot should be rejected")
	}
	// Out-of-bounds attribute.
	if _, _, reason := ValidateChargen(flow, map[string]string{"race": "elf", "class": "fighter"},
		map[string]map[string]int{"attrs": {"strength": 99, "intellect": 8, "constitution": 8}}, demoBundleKind); reason == "" {
		t.Fatal("an out-of-bounds attribute should be rejected")
	}
	// Over budget.
	if _, _, reason := ValidateChargen(flow, map[string]string{"race": "elf", "class": "fighter"},
		map[string]map[string]int{"attrs": {"strength": 15, "intellect": 15, "constitution": 15}}, demoBundleKind); reason != "" {
		t.Fatalf("27 points should be allowed, got %q", reason)
	}
}
