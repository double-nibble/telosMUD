package world

import "github.com/double-nibble/telosmud/internal/content"

// tier.go — apply the account trust tier (#27) as entity FLAGS on fresh login, plus the reserved-flag
// denylist. The tier arrives on the session from the VERIFIED assertion (Slice 2); here it becomes the
// engine flags that gate builder/admin powers and see-all (holylight, #28). It is RECONCILED on every
// fresh login — set the flags the tier grants, clear the ones it doesn't — so a promotion AND a demotion
// each take effect on the next login (the user's chosen model).
//
// The reserved flags are PURELY TIER-DERIVED, never persisted (dumpFlags skips them) and never restored
// from a state/handoff snapshot (applyStateComponents skips them) — the security-audit H-1 fix: the state
// restore runs via the trusted setFlag (bypassing the content op guard) and the handoff snapshot is
// unauthenticated, so a persisted/injected reserved flag must not be a capability. Consequence (fail-closed):
// a cross-shard HANDOFF currently DROPS elevation (the destination session has no tier); carrying the tier
// on the SIGNED handoff snapshot to re-derive there is a tracked follow-up.

const (
	flagBuilder = "builder" // gates builder-only commands (#27); granted by the builder + admin tiers
	flagAdmin   = "admin"   // gates admin-only commands (promote/demote, Slice 4); granted by the admin tier
)

// The trust tiers, as they arrive in the signed assertion. Local string literals mirror the store's
// canonical values (store.Tier*, the migration-00019 CHECK) — kept here so the world (which receives the
// tier as a plain string) has no dependency on the store package for this small enum.
const (
	tierPlayer  = "player"
	tierBuilder = "builder"
	tierAdmin   = "admin"
)

// trustTier is one rung of the trust ladder (#29, Round 9 Slice 0): its ordinal rank and the reserved
// capability flags it grants on login. Higher rank = more trusted; gated commands compare ranks.
type trustTier struct {
	rank  int
	flags []string // reserved trust flags this tier grants (⊆ reservedFlags); applied on login
}

// trustLadder is the ordered set of trust tiers, keyed by name — the SINGLE authority for tier→rank and
// tier→granted-flags. Content defines it (TrustTierDTO, build.go); the engine ships a DEFAULT ladder that
// reproduces the round-8 hardcoded mapping, so a pack that declares no trust_tiers (and the bare engine)
// behave exactly as before. The world and telos-account load the same ladder, so tiers are one authority.
type trustLadder struct {
	byName map[string]trustTier
}

// defaultTrustLadder is the engine's built-in ladder: the round-8 player/builder/admin mapping, expressed
// as ranks + granted flags. player is the un-elevated baseline (rank 0, no flags); builder + admin carry
// holylight (see-all) so they can see what they build; admin adds the admin flag. Ranks leave gaps (10/30)
// so a pack can slot moderator/architect between them without renumbering.
func defaultTrustLadder() *trustLadder {
	return &trustLadder{byName: map[string]trustTier{
		tierPlayer:  {rank: 0},
		tierBuilder: {rank: 20, flags: []string{flagHolylight, flagBuilder}},
		tierAdmin:   {rank: 40, flags: []string{flagHolylight, flagBuilder, flagAdmin}},
	}}
}

// rank returns the ordinal rank of tier `name`. An empty, "player", or ANY unknown/drifted value maps to
// rank 0 — the fail-safe baseline (a garbage tier can never read as elevation; mirrors the Slice-2 posture).
func (l *trustLadder) rank(name string) int {
	if t, ok := l.byName[name]; ok {
		return t.rank
	}
	return 0
}

// grantedFlags returns the reserved trust flags tier `name` grants (nil for the baseline/unknown). The
// caller (applyTierFlags) filters to reservedFlags — the ladder is trusted derivation, but a non-reserved
// flag named here is still ignored so the ladder can never invent a capability the engine doesn't know.
func (l *trustLadder) grantedFlags(name string) []string {
	if t, ok := l.byName[name]; ok {
		return t.flags
	}
	return nil
}

// buildTrustLadder converts the content trust-tier DTOs into the runtime ladder (#29, Round 9 Slice 0). An
// empty/nil list returns nil so the caller (z.trustLadder) falls back to the engine default ladder. The
// loader already deduped by name; flags are carried verbatim and applyTierFlags filters them to
// reservedFlags at apply time, so a stray non-reserved flag here is inert.
func buildTrustLadder(tiers []content.TrustTierDTO) *trustLadder {
	if len(tiers) == 0 {
		return nil
	}
	l := &trustLadder{byName: make(map[string]trustTier, len(tiers))}
	for _, t := range tiers {
		l.byName[t.Name] = trustTier{rank: t.Rank, flags: t.Flags}
	}
	return l
}

// applyTierFlags reconciles the reserved tier-flags on e to match tier — SETs the reserved flags the tier
// grants (per the zone's ladder) and CLEARs every other reserved flag. Called on every fresh login
// (loginRoom), AFTER the persisted flags load, so a stale elevated flag from a since-demoted session is
// cleared. Uses setFlag directly (the TRUSTED path); content's set_flag op can't touch these (reservedFlag).
// A nil / zone-less entity is a no-op. Only RESERVED flags named by the ladder are applied — a ladder that
// names a non-reserved flag is ignored (the engine never grants an unknown capability via a tier).
func applyTierFlags(e *Entity, tier string) {
	if e == nil || e.zone == nil {
		return
	}
	granted := map[string]bool{}
	for _, f := range e.zone.trustLadder().grantedFlags(tier) {
		if reservedFlag(f) {
			granted[f] = true
		}
	}
	for f := range reservedFlags {
		setFlag(e, f, granted[f]) // set the granted reserved flags; clear the rest (demotion strips them)
	}
}

// reservedFlags are the TRUST/ELEVATION flags that ONLY the tier-application path (applyTierFlags) may set.
// Content's set_flag / clear_flag ops refuse them — this closes the #28 audit gap where a builder pack could
// grant itself see-all. NOTE: flagDetectInvis is deliberately NOT reserved — it is a bounded game mechanic
// (a detect-invisibility spell/racial), not a trust capability like see-all / builder / admin.
var reservedFlags = map[string]bool{
	flagHolylight: true,
	flagBuilder:   true,
	flagAdmin:     true,
}

// reservedFlag reports whether name is a reserved trust flag content may not set via an effect op.
func reservedFlag(name string) bool { return reservedFlags[name] }
