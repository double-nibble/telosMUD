package content

import (
	"strings"
	"testing"
)

// trustladderlint_test.go — the trust-ladder content-lint (#111). It is a footgun guard, not the security
// control (that is TrustLadder.TierDominates), so the tests pin exactly which mistakes WARN vs REJECT and that
// the default ladder is clean.

// findingFor returns the first finding whose Detail contains sub, or a zero value + false.
func findingFor(vs []TrustLadderViolation, sub string) (TrustLadderViolation, bool) {
	for _, v := range vs {
		if strings.Contains(v.Detail, sub) {
			return v, true
		}
	}
	return TrustLadderViolation{}, false
}

// TestDefaultLadderIsLintClean: the engine default ladder must produce NO findings — the lint's baseline, and
// the reason a pack shipping no trust_tiers is skipped entirely.
func TestDefaultLadderIsLintClean(t *testing.T) {
	if vs := LintTrustLadder([]Pack{{Pack: "demo", TrustTiers: DefaultTrustTiers()}}); len(vs) != 0 {
		t.Fatalf("the default ladder must be lint-clean, got %+v", vs)
	}
	// A pack that ships NO ladder is skipped (it inherits the clean default).
	if vs := LintTrustLadder([]Pack{{Pack: "empty"}}); len(vs) != 0 {
		t.Fatalf("a pack with no trust_tiers must produce no findings, got %+v", vs)
	}
}

// TestLintBaselineGrantIsReject: the catastrophe case — the baseline tier granting a capability elevates the
// whole playerbase. REJECT.
func TestLintBaselineGrantIsReject(t *testing.T) {
	vs := LintTrustLadder([]Pack{{Pack: "bad", TrustTiers: []TrustTierDTO{
		{Name: "player", Rank: 0, Flags: []string{FlagHolylight}}, // baseline grants see-all to everyone
		{Name: "admin", Rank: 40, Flags: []string{FlagAdmin}},
	}}})
	v, ok := findingFor(vs, "BASELINE")
	if !ok {
		t.Fatalf("a baseline granting a capability must be flagged, got %+v", vs)
	}
	if v.Severity != TrustLadderReject {
		t.Errorf("a baseline capability grant must REJECT, got %s", v.Severity)
	}
	if v.Tier != "player" {
		t.Errorf("the finding should name the baseline tier, got %q", v.Tier)
	}
	// A renamed baseline is caught too (the lint uses Baseline(), not the literal "player").
	vs = LintTrustLadder([]Pack{{Pack: "bad", TrustTiers: []TrustTierDTO{
		{Name: "mortal", Rank: 0, Flags: []string{FlagAdmin}},
		{Name: "wizard", Rank: 40, Flags: []string{FlagAdmin}},
	}}})
	if v, ok := findingFor(vs, "BASELINE"); !ok || v.Tier != "mortal" {
		t.Errorf("a renamed baseline granting a capability must be caught, got %+v", vs)
	}
}

// TestLintDuplicateRankIsReject: two tiers sharing a rank leave the promote ceiling's ordering ambiguous.
func TestLintDuplicateRankIsReject(t *testing.T) {
	vs := LintTrustLadder([]Pack{{Pack: "dup", TrustTiers: []TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "gm", Rank: 30, Flags: []string{FlagAdmin}},
		{Name: "warden", Rank: 30, Flags: []string{FlagAdmin, FlagHolylight}},
		{Name: "admin", Rank: 40, Flags: []string{FlagAdmin, FlagHolylight, FlagBuilder}},
	}}})
	v, ok := findingFor(vs, "share rank 30")
	if !ok || v.Severity != TrustLadderReject {
		t.Fatalf("duplicate ranks must REJECT and name both tiers, got %+v", vs)
	}
	if !strings.Contains(v.Detail, "gm") || !strings.Contains(v.Detail, "warden") {
		t.Errorf("the finding should name both colliding tiers, got %q", v.Detail)
	}
}

// TestLintEmptyNameIsReject: a nameless rung is dropped by both ladders; the author must be told rather than
// have it silently vanish.
func TestLintEmptyNameIsReject(t *testing.T) {
	vs := LintTrustLadder([]Pack{{Pack: "noname", TrustTiers: []TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "", Rank: 99, Flags: []string{FlagAdmin}},
		{Name: "admin", Rank: 40, Flags: []string{FlagAdmin}},
	}}})
	if v, ok := findingFor(vs, "empty name"); !ok || v.Severity != TrustLadderReject {
		t.Fatalf("a nameless rung must REJECT, got %+v", vs)
	}
}

// TestLintNonCapabilityFlagIsWarn: a typo'd/invented flag — and wizinvis specifically — is dropped at apply
// time. WARN, with a wizinvis-specific hint.
func TestLintNonCapabilityFlagIsWarn(t *testing.T) {
	vs := LintTrustLadder([]Pack{{Pack: "typo", TrustTiers: []TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "wizard", Rank: 40, Flags: []string{FlagAdmin, "hollylight"}}, // typo
	}}})
	v, ok := findingFor(vs, "hollylight")
	if !ok || v.Severity != TrustLadderWarn {
		t.Fatalf("an unknown flag must WARN, got %+v", vs)
	}

	// wizinvis: reserved but never grantable — WARN with the specific hint so the author isn't confused.
	vs = LintTrustLadder([]Pack{{Pack: "wiz", TrustTiers: []TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "spook", Rank: 40, Flags: []string{FlagAdmin, FlagWizinvis}},
	}}})
	v, ok = findingFor(vs, "wizinvis")
	if !ok || v.Severity != TrustLadderWarn {
		t.Fatalf("granting wizinvis must WARN (it is never applied), got %+v", vs)
	}
	if !strings.Contains(v.Detail, "wizinvis on") {
		t.Errorf("the wizinvis finding should explain it is a self-set session concealment, got %q", v.Detail)
	}
}

// TestLintNonNestedLadderIsWarn: an admin-capable tier that cannot grant a tier beneath it (archon holds only
// admin; builder holds holylight+builder) — legal but usually a mistake. WARN.
func TestLintNonNestedLadderIsWarn(t *testing.T) {
	vs := LintTrustLadder([]Pack{{Pack: "nonnest", TrustTiers: []TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "builder", Rank: 20, Flags: []string{FlagHolylight, FlagBuilder}},
		{Name: "archon", Rank: 50, Flags: []string{FlagAdmin}}, // top rank, but not a superset of builder
	}}})
	v, ok := findingFor(vs, "never CREATE one")
	if !ok || v.Severity != TrustLadderWarn {
		t.Fatalf("a non-nested capability ladder must WARN, got %+v", vs)
	}
	if v.Tier != "archon" {
		t.Errorf("the finding should name the admin-capable tier, got %q", v.Tier)
	}
	// A properly nested ladder (admin holds everything) produces no such finding.
	vs = LintTrustLadder([]Pack{{Pack: "nested", TrustTiers: []TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "builder", Rank: 20, Flags: []string{FlagHolylight, FlagBuilder}},
		{Name: "admin", Rank: 50, Flags: []string{FlagHolylight, FlagBuilder, FlagAdmin}},
	}}})
	if _, ok := findingFor(vs, "never CREATE one"); ok {
		t.Errorf("a nested ladder must not warn about non-nesting, got %+v", vs)
	}
}

// TestLintFindingsAreDeterministic: the same packs produce the same findings in the same order across runs
// (tiers are name-sorted internally), so the reload gate's problem list does not churn.
func TestLintFindingsAreDeterministic(t *testing.T) {
	pack := Pack{Pack: "p", TrustTiers: []TrustTierDTO{
		{Name: "zebra", Rank: 30, Flags: []string{FlagAdmin}},
		{Name: "player", Rank: 0, Flags: []string{FlagHolylight}}, // baseline grant (reject)
		{Name: "alpha", Rank: 30, Flags: []string{FlagAdmin}},     // dup rank 30 with zebra (reject)
	}}
	first := LintTrustLadder([]Pack{pack})
	for i := 0; i < 5; i++ {
		got := LintTrustLadder([]Pack{pack})
		if len(got) != len(first) {
			t.Fatalf("finding count changed between runs: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("finding %d changed between runs:\n %+v\n %+v", j, got[j], first[j])
			}
		}
	}
}
