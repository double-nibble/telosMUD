package content

import "sort"

// trusttier.go — the SHARED runtime view of the content-defined trust ladder (#27/#29). The ladder DTOs
// (TrustTierDTO) are authored as content and persisted like any def; this file gives both consumers — the
// world (rank + reserved-flag derivation, command gating) and telos-account (tier validation + promote
// authz) — ONE resolved ladder implementation and ONE default, so tier rank/flag semantics never drift
// between the two services.

// Reserved trust flag names. They mirror the world's reserved trust flags (world.flagHolylight /
// flagBuilder / flagAdmin / flagWizinvis) — named here so the default ladder, the ladder lint, and
// telos-account's promote authz reference them by constant, not a bare literal. The world keeps its own
// copies (it can't take a content dependency for a 4-string set); TestReservedFlagsMatchContentVocabulary
// (world/tier_test.go) pins the two sets equal by value.
const (
	FlagHolylight = "holylight" // see-all
	FlagBuilder   = "builder"   // builder-tier command gate
	FlagAdmin     = "admin"     // manage-tiers (promote/demote) capability
	FlagWizinvis  = "wizinvis"  // staff concealment — reserved, but NOT a tier capability (see below)
)

// reservedTierFlags is the engine's reserved-flag set (== world.reservedFlags): flags content may not set via
// an effect op, persistence may not carry, and only the tier-apply path + the staff toggles may write. A
// ladder naming anything OUTSIDE this set is silently dropped at apply time — LintTrustLadder warns on that.
var reservedTierFlags = map[string]bool{
	FlagHolylight: true,
	FlagBuilder:   true,
	FlagAdmin:     true,
	FlagWizinvis:  true,
}

// nonCapabilityFlags are the reserved flags that are NOT a grantable capability — a tier may not confer them
// and world.applyTierFlags will not apply them (it grants only capability flags, clearing every other reserved
// flag). Today that is exactly FlagWizinvis: a SESSION-scoped staff concealment, dropped at every login, that
// any staff-rank session sets for itself with `wizinvis on` and whose reach is bounded by the holder's own
// rank. Because a tier can never grant it, it cannot be minted through a promote, so it is correctly outside
// the ceiling's vocabulary. LintTrustLadder warns on a ladder that names it.
var nonCapabilityFlags = map[string]bool{FlagWizinvis: true}

// tierCapabilityFlags is DERIVED — reserved minus non-capability — rather than hand-listed. That direction
// matters: a reserved flag added tomorrow and forgotten here would be a capability the promote ceiling never
// compares (i.e. #165, silently reintroduced). Deriving it makes a new reserved flag default to CAPABILITY,
// which fails closed (at worst an over-strict promote refusal, never an escalation). Opting a flag OUT is
// then a deliberate edit to nonCapabilityFlags, next to the reasoning that justifies it.
var tierCapabilityFlags = func() map[string]bool {
	caps := make(map[string]bool, len(reservedTierFlags))
	for f := range reservedTierFlags {
		if !nonCapabilityFlags[f] {
			caps[f] = true
		}
	}
	return caps
}()

// IsReservedTierFlag reports whether f is a reserved trust flag (engine-managed; content may not set it).
func IsReservedTierFlag(f string) bool { return reservedTierFlags[f] }

// IsTierCapabilityFlag reports whether f is a grantable CAPABILITY flag — the vocabulary the promote ceiling
// compares and the only flags a tier grant may confer (world.applyTierFlags).
func IsTierCapabilityFlag(f string) bool { return tierCapabilityFlags[f] }

// TierCapabilityFlags returns the capability-flag names, sorted — for the world's apply filter, the ladder
// lint's messages, and the drift tests (which must iterate the DERIVATION, not a hardcoded name list).
func TierCapabilityFlags() []string { return sortedKeys(tierCapabilityFlags) }

// ReservedTierFlags returns the reserved-flag names, sorted (same rationale as TierCapabilityFlags).
func ReservedTierFlags() []string { return sortedKeys(reservedTierFlags) }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

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
//
// A tier with an EMPTY NAME is DROPPED. "" is the value every fail-safe path in the engine degrades to — an
// unknown tier, a missing tier row, a session whose tier was not carried across a handoff — and both Rank and
// GrantsFlag are documented to read it as the un-elevated baseline. A ladder rung named "" (trivially produced
// by a YAML entry that omits `name:`) would make that degradation GRANT whatever the nameless rung grants:
// `rank("") = 99` passes every MinRank gate. Dropping it here keeps "" un-defined, so the fail-safe holds
// by construction rather than by an authoring convention. LintTrustLadder rejects such a rung loudly.
func NewTrustLadder(tiers []TrustTierDTO) *TrustLadder {
	if len(tiers) == 0 {
		tiers = DefaultTrustTiers()
	}
	l := &TrustLadder{byName: make(map[string]TrustTierDTO, len(tiers))}
	for _, t := range tiers {
		if t.Name == "" {
			continue // "" is the fail-safe baseline sentinel; it must never resolve to a defined rung
		}
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
// (a garbage tier can never read as elevation), matching the world's rank fail-safe. "" short-circuits before
// the lookup so the guarantee does not depend on NewTrustLadder having dropped a nameless rung.
func (l *TrustLadder) Rank(name string) int {
	if name == "" {
		return 0
	}
	return l.byName[name].Rank
}

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

// TierDominates reports whether tier `holder` holds every CAPABILITY flag tier `other` grants — the
// flag-aware half of the promote ceiling (#165). Read it as "holder is at least as capable as other".
//
// RANK AND CAPABILITY ARE INDEPENDENT AXES. The rank ceiling compares ordinals only, so it admits two
// distinct failures the ladder does not forbid:
//
//   - same rank, richer flags: gm(30,{admin}) vs warden(30,{admin,holylight}) — Rank(warden) > Rank(gm) is
//     false, so a gm could mint itself holylight;
//   - LOWER rank, orthogonal flags: with a mod(30,{admin}) slotted into the default ladder's 20..40 gap
//     (which DefaultTrustTiers explicitly invites), Rank(builder)=20 > Rank(mod)=30 is false, so a mod could
//     mint a builder's holylight+builder. Every rank here is unique — no duplicate-rank lint would help.
//
// So this is NOT a same-rank patch and NOT a restatement of the rank-uniqueness invariant: it is the
// independent capability check, and it must stay even once LintTrustLadder rejects duplicate ranks.
//
// Only tierCapabilityFlags participate — a non-reserved flag is inert at apply time and FlagWizinvis is never
// granted by a tier (see nonCapabilityFlags), so neither can be minted through a promote.
//
// FAILS CLOSED at both ends, matching the sibling predicates (Rank → 0, GrantsFlag → false): an unknown
// `holder` dominates nothing (it grants nothing, so it cannot vouch for any capability), and an unknown
// `other` grants nothing and is therefore dominated by any DEFINED holder. Callers must not rely on an
// unknown holder passing — it does not.
func (l *TrustLadder) TierDominates(holder, other string) bool {
	if _, ok := l.byName[holder]; !ok {
		return false // an undefined holder vouches for nothing
	}
	for _, f := range l.byName[other].Flags {
		if tierCapabilityFlags[f] && !l.GrantsFlag(holder, f) {
			return false
		}
	}
	return true
}

// MissingCapabilities returns the capability flags tier `other` grants that tier `holder` does not, sorted —
// exactly the set that made TierDominates(holder, other) false, so a refusal can name what is missing instead
// of saying "capabilities you do not hold". Empty (nil) when holder dominates other. An undefined holder is
// missing everything `other` grants, matching TierDominates's fail-closed answer.
func (l *TrustLadder) MissingCapabilities(holder, other string) []string {
	var missing []string
	for _, f := range l.byName[other].Flags {
		if tierCapabilityFlags[f] && !l.GrantsFlag(holder, f) {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	return missing
}

// Baseline returns the ladder's BASELINE tier: the lowest-rank rung — what an un-elevated account is. It is
// the demote target (#112): the edge's `demote <char>` must not hardcode the literal "player", which a pack is
// free to rename or omit, making every demote fail closed with "Unknown tier". Ties on rank resolve to the
// lexicographically-first NAME so the answer is total and deterministic for ANY ladder (LintTrustLadder
// separately rejects duplicate ranks, but this must not depend on that). NewTrustLadder guarantees a non-empty
// ladder, so "" is unreachable in practice.
func (l *TrustLadder) Baseline() string {
	best := ""
	for name, t := range l.byName {
		if best == "" || t.Rank < l.byName[best].Rank || (t.Rank == l.byName[best].Rank && name < best) {
			best = name
		}
	}
	return best
}

// Names returns the defined tier names (unordered) — for a usage/help listing.
func (l *TrustLadder) Names() []string {
	out := make([]string, 0, len(l.byName))
	for n := range l.byName {
		out = append(out, n)
	}
	return out
}
