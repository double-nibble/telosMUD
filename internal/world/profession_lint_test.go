package world

import "testing"

// TestLintLearnProfessionRefs pins the docs/REMAINING.md §4 content-lint: a learn_profession op whose
// `profession` names a kind:"profession" bundle passes; one naming a non-profession bundle, or no bundle at
// all, is reported. The lint walks nested op-lists (if/check bands), so a buried miss is also caught.
func TestLintLearnProfessionRefs(t *testing.T) {
	d := newDefRegistries()
	// A proper profession bundle (ref == membership ref by convention) and a non-profession bundle.
	d.bundle.register("leatherworking", &bundleDef{ref: "leatherworking", kind: "profession"})
	d.bundle.register("warrior", &bundleDef{ref: "warrior", kind: "class"})

	// A bundle that correctly enrolls in a profession bundle — no finding.
	d.bundle.register("learn_leatherworking", &bundleDef{
		ref:    "learn_leatherworking",
		kind:   "training",
		grants: []effectOp{{kind: "learn_profession", profession: "leatherworking"}},
	})
	// An ability that enrolls in a NON-profession bundle — one finding.
	d.ability.register("bad_train_class", &abilityDef{
		ref: "bad_train_class",
		ops: []effectOp{{kind: "learn_profession", profession: "warrior"}},
	})
	// A track step that enrolls in an UNKNOWN profession (no bundle) — one finding, nested-walk coverage
	// via an `if` wrapper to prove the recursion reaches op.then.
	d.track.register("mining", &trackDef{
		ref: "mining",
		steps: [][]effectOp{{
			{kind: "if", then: []effectOp{{kind: "learn_profession", profession: "ghost_trade"}}},
		}},
	})
	// A resource on_depleted op-list enrolling in an unknown profession — proves the on_depleted root is
	// walked (the mudlib-review gap: on_depleted / affect tickOps are separate roots from on_event).
	d.res.register("hp", &resourceDef{
		ref:        "hp",
		onDepleted: []effectOp{{kind: "learn_profession", profession: "death_trade"}},
	})

	misses := lintLearnProfessionRefs(d)
	got := map[string]string{} // profession -> owner
	for _, m := range misses {
		got[m.profession] = m.owner
	}
	if len(misses) != 3 {
		t.Fatalf("expected 3 findings, got %d: %+v", len(misses), misses)
	}
	if owner, ok := got["death_trade"]; !ok {
		t.Errorf("a learn_profession in resource on_depleted must be flagged")
	} else if owner != "resource hp on_depleted" {
		t.Errorf("owner = %q, want %q", owner, "resource hp on_depleted")
	}
	if _, ok := got["leatherworking"]; ok {
		t.Errorf("a valid kind:profession bundle ref must NOT be flagged")
	}
	if owner, ok := got["warrior"]; !ok {
		t.Errorf("a non-profession bundle ref must be flagged")
	} else if owner != "ability bad_train_class" {
		t.Errorf("owner = %q, want %q", owner, "ability bad_train_class")
	}
	if owner, ok := got["ghost_trade"]; !ok {
		t.Errorf("an unknown (no-bundle) profession ref must be flagged (nested walk)")
	} else if owner != "track mining step" {
		t.Errorf("owner = %q, want %q", owner, "track mining step")
	}
}
