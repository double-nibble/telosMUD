package content

// trusttier.go — the SHARED runtime view of the content-defined trust ladder (#27/#29). The ladder DTOs
// (TrustTierDTO) are authored as content and persisted like any def; this file gives both consumers — the
// world (rank + reserved-flag derivation, command gating) and telos-account (tier validation + promote
// authz) — ONE resolved ladder implementation and ONE default, so tier rank/flag semantics never drift
// between the two services.

// Reserved capability flag names a trust tier may grant. They mirror the world's reserved trust flags
// (world.flagHolylight / flagBuilder / flagAdmin) — named here so the default ladder and telos-account's
// promote authz reference them by constant, not a bare literal. The world keeps its own copies (it can't
// import nothing extra for a 3-string set); these must stay in sync with them by value.
const (
	FlagHolylight = "holylight" // see-all
	FlagBuilder   = "builder"   // builder-tier command gate
	FlagAdmin     = "admin"     // manage-tiers (promote/demote) capability
)

// DefaultTrustTiers is the engine's built-in ladder used when a pack declares no trust_tiers: the round-8
// player/builder/admin mapping, expressed as ranks + granted reserved flags. player is the un-elevated
// rank-0 baseline; builder + admin carry holylight so they can see what they build; admin adds the
// manage-tiers capability. Ranks leave gaps (10/30) so a pack can slot moderator/architect between them.
// Both the world and telos-account fall back to this, so they agree on the default.
func DefaultTrustTiers() []TrustTierDTO {
	return []TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "builder", Rank: 20, Flags: []string{FlagHolylight, FlagBuilder}},
		{Name: "admin", Rank: 40, Flags: []string{FlagHolylight, FlagBuilder, FlagAdmin}},
	}
}

// TrustLadder is a resolved trust ladder: tier name → rank + granted flags. Built from a pack's TrustTiers
// (or DefaultTrustTiers when the pack declares none). Read-only after construction; safe to share.
type TrustLadder struct {
	byName map[string]TrustTierDTO
}

// NewTrustLadder resolves the tier list into a ladder. An empty/nil list falls back to DefaultTrustTiers,
// so a caller with no content still gets the round-8 ladder. A duplicate name is last-write-wins (the
// content loader already deduped by name upstream).
func NewTrustLadder(tiers []TrustTierDTO) *TrustLadder {
	if len(tiers) == 0 {
		tiers = DefaultTrustTiers()
	}
	l := &TrustLadder{byName: make(map[string]TrustTierDTO, len(tiers))}
	for _, t := range tiers {
		l.byName[t.Name] = t
	}
	return l
}

// Has reports whether name is a defined tier (the validation predicate: promote refuses an unknown tier).
func (l *TrustLadder) Has(name string) bool {
	_, ok := l.byName[name]
	return ok
}

// Rank returns the tier's ordinal rank. An unknown/empty/wrong-case name maps to 0 — the fail-safe baseline
// (a garbage tier can never read as elevation), matching the world's rank fail-safe.
func (l *TrustLadder) Rank(name string) int { return l.byName[name].Rank }

// GrantsFlag reports whether tier name grants the named reserved capability flag (e.g. the manage-tiers
// authority is "does the actor's tier grant FlagAdmin"). An unknown tier grants nothing.
func (l *TrustLadder) GrantsFlag(name, flag string) bool {
	for _, f := range l.byName[name].Flags {
		if f == flag {
			return true
		}
	}
	return false
}

// Names returns the defined tier names (unordered) — for a usage/help listing.
func (l *TrustLadder) Names() []string {
	out := make([]string, 0, len(l.byName))
	for n := range l.byName {
		out = append(out, n)
	}
	return out
}
