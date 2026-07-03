package content

import "testing"

// trusttier_test.go — the shared trust-ladder view (#27/#29 Round 9 Slice 0b). Pins the default mapping
// (so the world + telos-account agree with round-8) and the resolved-ladder predicates.

// TestDefaultTrustTiersMapping pins the built-in ladder to the exact round-8 grants: player=0/no flags,
// builder=20/holylight+builder, admin=40/+admin. A drift here would silently change elevation semantics.
func TestDefaultTrustTiersMapping(t *testing.T) {
	l := NewTrustLadder(DefaultTrustTiers())
	if l.Rank("player") != 0 || l.Rank("builder") != 20 || l.Rank("admin") != 40 {
		t.Fatalf("default ranks = player:%d builder:%d admin:%d, want 0/20/40",
			l.Rank("player"), l.Rank("builder"), l.Rank("admin"))
	}
	if l.GrantsFlag("player", FlagBuilder) || l.GrantsFlag("player", FlagHolylight) {
		t.Error("player must grant no reserved flags")
	}
	if !l.GrantsFlag("builder", FlagHolylight) || !l.GrantsFlag("builder", FlagBuilder) || l.GrantsFlag("builder", FlagAdmin) {
		t.Error("builder must grant holylight+builder, not admin")
	}
	if !l.GrantsFlag("admin", FlagAdmin) {
		t.Error("admin must grant the manage-tiers (admin) flag")
	}
}

// TestNewTrustLadderEmptyFallsBackToDefault: nil/empty tiers => the default ladder (round-8), so a caller
// with no content still resolves the three built-in tiers.
func TestNewTrustLadderEmptyFallsBackToDefault(t *testing.T) {
	for _, tiers := range [][]TrustTierDTO{nil, {}} {
		l := NewTrustLadder(tiers)
		if !l.Has("player") || !l.Has("builder") || !l.Has("admin") {
			t.Fatalf("empty tier list must fall back to the default ladder, got names %v", l.Names())
		}
	}
}

// TestTrustLadderPredicates: a custom content ladder resolves rank/flag/membership; an unknown tier is the
// fail-safe baseline (rank 0, no flags, not a member).
func TestTrustLadderPredicates(t *testing.T) {
	l := NewTrustLadder([]TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "moderator", Rank: 10},
		{Name: "architect", Rank: 30, Flags: []string{FlagHolylight, FlagBuilder, FlagAdmin}},
	})
	if l.Rank("architect") != 30 || l.Rank("moderator") != 10 {
		t.Errorf("ranks: architect=%d moderator=%d, want 30/10", l.Rank("architect"), l.Rank("moderator"))
	}
	if !l.GrantsFlag("architect", FlagAdmin) {
		t.Error("architect should grant the admin flag in this ladder")
	}
	if l.GrantsFlag("moderator", FlagAdmin) || l.GrantsFlag("moderator", FlagHolylight) {
		t.Error("moderator is a pure rank rung with no flags")
	}
	// Unknown / wrong-case tier: baseline.
	for _, name := range []string{"", "superuser", "ADMIN"} {
		if l.Has(name) || l.Rank(name) != 0 || l.GrantsFlag(name, FlagAdmin) {
			t.Errorf("unknown tier %q must be the fail-safe baseline (absent, rank 0, no flags)", name)
		}
	}
	if len(l.Names()) != 3 {
		t.Errorf("Names() should list the 3 defined tiers, got %v", l.Names())
	}
}
