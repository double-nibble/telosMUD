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

// TestTierDominates pins the flag-aware promote ceiling (#165). Rank and capability are INDEPENDENT axes, so
// the table deliberately carries three shapes the rank ceiling alone cannot separate:
//   - same rank, richer flags:  gm(30,{admin})  vs warden(30,{admin,holylight})   — the issue's case;
//   - LOWER rank, richer flags: mod(30,{admin}) vs builder(20,{holylight,builder}) — every rank unique, so no
//     duplicate-rank lint would ever help. This is the case that proves the fix is not a same-rank patch;
//   - unknown names at either end, which must fail CLOSED like the sibling predicates (Rank→0, GrantsFlag→false).
func TestTierDominates(t *testing.T) {
	l := NewTrustLadder([]TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "builder", Rank: 20, Flags: []string{FlagHolylight, FlagBuilder}},
		{Name: "gm", Rank: 30, Flags: []string{FlagAdmin}},
		{Name: "mod", Rank: 30, Flags: []string{FlagAdmin}}, // same caps as gm, distinct name
		{Name: "warden", Rank: 30, Flags: []string{FlagAdmin, FlagHolylight}},
		{Name: "admin", Rank: 40, Flags: []string{FlagHolylight, FlagBuilder, FlagAdmin}},
		// A rung naming a NON-capability flag: inert at apply time, so it must not participate in the ceiling
		// (otherwise admin — which cannot grant "songs" — could never grant bard).
		{Name: "bard", Rank: 10, Flags: []string{"songs"}},
		// wizinvis is reserved but never grantable (world.applyTierFlags filters to capabilities), so a tier
		// naming it confers nothing and it must not participate either.
		{Name: "ghost", Rank: 10, Flags: []string{FlagWizinvis}},
	})

	tests := []struct {
		name          string
		holder, other string
		want          bool
	}{
		// The #165 case: same rank, so the rank ceiling is blind; only capability separates.
		{"same-rank poorer peer does not dominate the richer one", "gm", "warden", false},
		{"same-rank richer peer dominates the poorer one", "warden", "gm", true},
		{"same rank, same caps, different names: mutual dominance", "gm", "mod", true},

		// CROSS-RANK inversion — the case a duplicate-rank lint cannot catch. mod outranks builder but holds
		// orthogonal capabilities, so it must not be able to mint one.
		{"higher-ranked tier with orthogonal caps does NOT dominate a lower-ranked richer one", "mod", "builder", false},
		{"lower-ranked richer tier does not dominate a higher-ranked one it lacks caps for", "builder", "mod", false},

		{"a tier always dominates itself", "warden", "warden", true},
		{"the top tier holds every capability", "admin", "warden", true},
		{"everything dominates the baseline", "gm", "player", true},
		{"the baseline dominates nothing capable", "player", "gm", false},
		{"non-capability flags do not participate", "admin", "bard", true},
		{"wizinvis does not participate — a tier can never confer it", "player", "ghost", true},

		// FAIL CLOSED at both ends.
		{"an undefined holder vouches for nothing, even over the baseline", "nosuchtier", "player", false},
		{"an undefined holder vouches for nothing, over anything", "nosuchtier", "warden", false},
		{"a defined holder dominates an undefined other (it grants nothing)", "player", "nosuchtier", true},
		// "" is the REAL production value for an account with no tier row (account/service.go reads it raw).
		{"a defined holder dominates the empty tier", "gm", "", true},
		{"the empty tier is not a defined holder", "", "player", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := l.TierDominates(tc.holder, tc.other); got != tc.want {
				t.Fatalf("TierDominates(%q, %q) = %v, want %v", tc.holder, tc.other, got, tc.want)
			}
		})
	}

	// MissingCapabilities must name exactly what made TierDominates false, sorted, and be empty when it is true.
	if got := l.MissingCapabilities("gm", "warden"); len(got) != 1 || got[0] != FlagHolylight {
		t.Errorf("MissingCapabilities(gm, warden) = %v, want [holylight]", got)
	}
	if got := l.MissingCapabilities("mod", "builder"); len(got) != 2 || got[0] != FlagBuilder || got[1] != FlagHolylight {
		t.Errorf("MissingCapabilities(mod, builder) = %v, want [builder holylight] (sorted)", got)
	}
	if got := l.MissingCapabilities("admin", "warden"); len(got) != 0 {
		t.Errorf("MissingCapabilities(admin, warden) = %v, want none (admin dominates)", got)
	}
	if got := l.MissingCapabilities("admin", "ghost"); len(got) != 0 {
		t.Errorf("wizinvis must never appear as a missing capability, got %v", got)
	}
}

// TestTierCapabilityFlagsAreDerived pins the DERIVATION, not a name list (#165 H-1). The safety property the
// ceiling rests on is {flags a tier may grant} ⊆ {flags the ceiling compares}. Two hand-maintained maps would
// drift silently: add a reserved flag, forget the capability map, and the ceiling never compares it — #165,
// reintroduced. Deriving capability = reserved − nonCapability makes a new reserved flag default to CAPABILITY,
// which fails closed. This test asserts the relationship, so it fails if anyone re-hardcodes either side.
func TestTierCapabilityFlagsAreDerived(t *testing.T) {
	reserved := ReservedTierFlags()
	caps := TierCapabilityFlags()

	// Every capability is reserved.
	for _, f := range caps {
		if !IsReservedTierFlag(f) {
			t.Errorf("capability %q is not reserved — content's set_flag op could set it, bypassing the tier", f)
		}
	}
	// Every reserved flag is EITHER a capability or explicitly opted out in nonCapabilityFlags.
	for _, f := range reserved {
		if !IsTierCapabilityFlag(f) && !nonCapabilityFlags[f] {
			t.Errorf("reserved flag %q is neither a capability nor an explicit non-capability — the promote "+
				"ceiling would never compare it (this is #165)", f)
		}
	}
	if len(caps)+len(nonCapabilityFlags) != len(reserved) {
		t.Errorf("caps(%v) + nonCapability(%v) must partition reserved(%v)", caps, nonCapabilityFlags, reserved)
	}
	// Pin today's partition so an accidental widening is visible in the diff.
	if !IsReservedTierFlag(FlagWizinvis) || IsTierCapabilityFlag(FlagWizinvis) {
		t.Error("wizinvis must be reserved but NOT a capability (a session concealment, never a tier grant)")
	}
	for _, f := range []string{FlagHolylight, FlagBuilder, FlagAdmin} {
		if !IsTierCapabilityFlag(f) {
			t.Errorf("%q must be a grantable capability", f)
		}
	}
	for _, f := range []string{"", "songs", "Holylight", "detect_invis"} {
		if IsReservedTierFlag(f) || IsTierCapabilityFlag(f) {
			t.Errorf("%q must be neither reserved nor a capability", f)
		}
	}
}

// TestTrustLadderBaseline: the demote target (#112) is the LOWEST-RANK rung, not the literal "player" — a pack
// may rename or omit it. Ties on rank resolve to the lexicographically-first name so the answer is total.
func TestTrustLadderBaseline(t *testing.T) {
	tests := []struct {
		name  string
		tiers []TrustTierDTO
		want  string
	}{
		{"default ladder", DefaultTrustTiers(), "player"},
		{"nil falls back to the default", nil, "player"},
		{"a renamed baseline", []TrustTierDTO{
			{Name: "mortal", Rank: 0}, {Name: "wizard", Rank: 50, Flags: []string{FlagAdmin}},
		}, "mortal"},
		{"no rank-0 rung: the lowest rank wins", []TrustTierDTO{
			{Name: "citizen", Rank: 5}, {Name: "wizard", Rank: 50},
		}, "citizen"},
		{"rank tie resolves to the first name", []TrustTierDTO{
			{Name: "peon", Rank: 0}, {Name: "alpha", Rank: 0}, {Name: "wizard", Rank: 50},
		}, "alpha"},
		{"negative ranks are honored", []TrustTierDTO{
			{Name: "banned", Rank: -10}, {Name: "player", Rank: 0},
		}, "banned"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewTrustLadder(tc.tiers).Baseline(); got != tc.want {
				t.Fatalf("Baseline() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNamelessRungIsDropped: "" is the sentinel every fail-safe path degrades to — an unknown tier, an account
// with no tier row, a session whose tier did not cross a handoff. A content ladder with a nameless rung (a YAML
// entry that omits `name:`) must NOT make that degradation resolve to a defined, flag-granting tier: `rank("")`
// would then pass every MinRank gate. The rung is dropped, and Rank("") short-circuits before the lookup so the
// guarantee does not depend on the constructor having done so (#165 F5).
func TestNamelessRungIsDropped(t *testing.T) {
	l := NewTrustLadder([]TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "", Rank: 99, Flags: []string{FlagAdmin, FlagHolylight}}, // the footgun
		{Name: "admin", Rank: 40, Flags: []string{FlagAdmin}},
	})
	if l.Has("") {
		t.Error(`Has("") must be false — "" is the fail-safe sentinel, never a defined rung`)
	}
	if r := l.Rank(""); r != 0 {
		t.Errorf(`Rank("") = %d, want 0 (the fail-safe baseline); a nameless rung must not read as elevation`, r)
	}
	if l.GrantsFlag("", FlagAdmin) || l.GrantsFlag("", FlagHolylight) {
		t.Error(`GrantsFlag("", …) must be false — a nameless rung must never confer a capability`)
	}
	if l.TierDominates("", "player") {
		t.Error(`TierDominates("", …) must be false — "" is not a defined holder`)
	}
	if names := l.Names(); len(names) != 2 {
		t.Errorf("the nameless rung must not appear in the ladder, got %v", names)
	}
	// The real ladder still works.
	if l.Rank("admin") != 40 || !l.GrantsFlag("admin", FlagAdmin) {
		t.Error("dropping the nameless rung must not disturb the defined ones")
	}
}
