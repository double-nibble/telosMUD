package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// trustladder_test.go — #29 Round 9 Slice 0: the content-defined ordinal trust ladder. Covers the default
// ladder's ranks, building a content ladder (moderator/architect between the defaults), rank fail-safe for
// unknown tiers, and that applyTierFlags derives reserved flags FROM the ladder (and ignores a non-reserved
// flag a ladder names).

// TestDefaultLadderRanksAreOrdered pins the default ladder's ordinal ranks: player < builder < admin, with
// gaps so a pack can slot moderator/architect between them.
func TestDefaultLadderRanksAreOrdered(t *testing.T) {
	l := defaultTrustLadder()
	if l.rank(tierPlayer) >= l.rank(tierBuilder) || l.rank(tierBuilder) >= l.rank(tierAdmin) {
		t.Fatalf("default ranks must be ordered player<builder<admin, got %d<%d<%d",
			l.rank(tierPlayer), l.rank(tierBuilder), l.rank(tierAdmin))
	}
	if l.rank(tierPlayer) != 0 {
		t.Errorf("player must be the rank-0 baseline, got %d", l.rank(tierPlayer))
	}
}

// TestRankUnknownIsBaseline: an empty, unknown, or wrong-case tier resolves to rank 0 (fail-safe — a
// garbage tier can never read as elevation).
func TestRankUnknownIsBaseline(t *testing.T) {
	l := defaultTrustLadder()
	for _, name := range []string{"", "superuser", "ADMIN", "Builder"} {
		if r := l.rank(name); r != 0 {
			t.Errorf("rank(%q) = %d, want 0 (baseline)", name, r)
		}
	}
}

// TestBuildTrustLadderFromContent builds a 5-rung content ladder and checks ranks + granted flags. A
// moderator with no flags is a pure rank rung (can be gated on, carries no engine capability).
func TestBuildTrustLadderFromContent(t *testing.T) {
	l := buildTrustLadder([]content.TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "moderator", Rank: 10},
		{Name: "builder", Rank: 20, Flags: []string{flagHolylight, flagBuilder}},
		{Name: "architect", Rank: 30, Flags: []string{flagHolylight, flagBuilder}},
		{Name: "admin", Rank: 40, Flags: []string{flagHolylight, flagBuilder, flagAdmin}},
	})
	if l == nil {
		t.Fatal("a non-empty tier list must build a ladder")
	}
	if l.rank("moderator") != 10 || l.rank("architect") != 30 {
		t.Errorf("content ranks: moderator=%d architect=%d, want 10/30", l.rank("moderator"), l.rank("architect"))
	}
	if got := l.grantedFlags("moderator"); len(got) != 0 {
		t.Errorf("moderator is a pure rank rung, want no flags, got %v", got)
	}
	if got := l.grantedFlags("admin"); len(got) != 3 {
		t.Errorf("admin should grant 3 reserved flags, got %v", got)
	}
}

// TestEmptyTierListUsesDefault: no content trust_tiers => buildTrustLadder returns nil, and a zone with a
// nil ladder falls back to the engine default (round-8 behavior).
func TestEmptyTierListUsesDefault(t *testing.T) {
	if l := buildTrustLadder(nil); l != nil {
		t.Fatal("an empty tier list must return nil so the default ladder is used")
	}
	z := newZone("test")
	if z.defs.trust != nil {
		t.Fatal("a bare zone should have no content ladder")
	}
	if z.trustLadder().rank(tierBuilder) != 20 {
		t.Error("a zone with no content ladder must resolve ranks via the default ladder")
	}
}

// TestContentLadderDrivesFlags: applyTierFlags derives reserved flags from the ZONE's content ladder — a
// custom "wizard" tier that grants holylight sets it; a NON-reserved flag the ladder names is ignored (the
// ladder can never invent a capability the engine doesn't know).
func TestContentLadderDrivesFlags(t *testing.T) {
	z := newZone("test")
	z.defs.trust = buildTrustLadder([]content.TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "wizard", Rank: 50, Flags: []string{flagHolylight, "sparkle"}}, // sparkle is not reserved
	})
	room := z.newEntity(ProtoRef("test:room"))
	Add(room, &Room{})
	e := z.newEntity(ProtoRef(""))
	Add(e, &Living{})
	Move(e, room)

	applyTierFlags(e, "wizard")
	if !hasFlag(e, flagHolylight) {
		t.Error("the wizard tier's holylight (a reserved flag) must be applied from the content ladder")
	}
	if hasFlag(e, "sparkle") {
		t.Error("a non-reserved flag named by the ladder must be ignored (no invented capabilities)")
	}
	// Reconcile to player: holylight cleared (demotion strips it).
	applyTierFlags(e, "player")
	if hasFlag(e, flagHolylight) {
		t.Error("demotion to player must clear the content-granted holylight")
	}
}
