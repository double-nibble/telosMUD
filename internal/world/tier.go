package world

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

// tierFlags maps a trust tier to the reserved flags it grants via an EXPLICIT ALLOWLIST: ONLY exactly
// "builder" or "admin" elevate. "", "player", and ANY unknown/drifted value are the un-elevated baseline
// (fail-safe — a garbage tier can never be read as elevation; the Slice-2 reviews asked for this). Powers
// are cumulative (admin ⊇ builder). holylight (see-all) is granted to both so a builder can see what they
// build.
func tierFlags(tier string) (holylight, builder, admin bool) {
	switch tier {
	case tierAdmin:
		return true, true, true
	case tierBuilder:
		return true, true, false
	default: // "", "player", or anything unrecognized → no elevation
		return false, false, false
	}
}

// applyTierFlags reconciles the reserved tier-flags on e to match tier — SETs the flags the tier grants and
// CLEARs the ones it doesn't. Called on every fresh login (loginRoom), AFTER the persisted flags load, so a
// stale elevated flag from a since-demoted session is cleared. Uses setFlag directly (the TRUSTED path);
// content's set_flag op can't touch these (reservedFlag). A nil / Living-less entity is a no-op.
func applyTierFlags(e *Entity, tier string) {
	if e == nil {
		return
	}
	holylight, builder, admin := tierFlags(tier)
	setFlag(e, flagHolylight, holylight)
	setFlag(e, flagBuilder, builder)
	setFlag(e, flagAdmin, admin)
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
