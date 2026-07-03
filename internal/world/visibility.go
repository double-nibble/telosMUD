package world

// visibility.go — the visibility flags that give the canSee/nameFor chokepoint (targeting.go / act.go)
// its teeth (#28, Track 2). The chokepoint was wired from slice 1 but canSee was a trivial "everything is
// visible" stub; these flags are the first real data it consults. Open-string flags on Living.flags
// (flags.go), set by content/effects and read by the engine — the engine never invents one (the pillar).
//
// The model, deliberately minimal for this slice:
//   - flagInvisible — the target cannot be perceived by an ordinary viewer (an invisibility spell, or a
//     builder hiding from mortals — "wizinvis" is just a builder carrying this flag).
//   - flagDetectInvis — the viewer pierces invisibility (a detect-invisibility effect / racial).
//   - flagHolylight — the SEE-ALL end of the chokepoint: this viewer sees everything regardless of any
//     concealment (#28). Granted as a flag today; the builder trust tier (#27/#97) will grant it as an
//     admin power once that lands, so nothing here needs to change then.
//
// Deferred to follow-up slices (each its own mechanic): dark rooms + a light-source model + infravision;
// hidden/sneak (a perception contest); and the CROSS-SHARD `who` roster filter (needs the presence Entry
// to carry a concealment bit — this slice filters only the zone-LOCAL who + lookRoom).
const (
	flagInvisible   = "invisible"    // target: not perceivable by an ordinary viewer
	flagDetectInvis = "detect_invis" // viewer: pierces flagInvisible
	flagHolylight   = "holylight"    // viewer: sees everything (the elevated end — builders/immortals, #28)
	flagWizinvis    = "wizinvis"     // target: a STAFF member hidden from LOWER trust ranks (#30, rank-aware)
)

// SECURITY POSTURE (#28, updated once the builder trust tier #27 landed): holylight is now a RESERVED trust
// flag (see reservedFlags in tier.go) — content set_flag/clear_flag refuse it, it is never persisted, and a
// forged snapshot can't inject it, so no player can self-grant see-all. detect_invis is DELIBERATELY left a
// plain open-string gameplay flag (a detect-invisibility effect/racial), which is safe because (a) canSee
// gates PERCEPTION ONLY — never harm/authz (the harm gate is guardHarmful, separate), so piercing invisibility
// is a pure information capability that cannot bypass the hostility gate; and (b) detect_invis pierces only
// flagInvisible, never a staff member's flagWizinvis (a distinct trust-rank branch in visibleTo below). See
// #27/#97.

// visibleTo reports whether target is perceivable by viewer under the concealment flags above. It is the
// single rule canSee delegates to (kept here beside the flag names so the visibility policy lives in one
// place). Self and a nil perspective are always visible; holylight sees all; an invisible target is hidden
// unless the viewer detects invisibility.
//
// NOTE for future concealment mechanics: the blanket "return true" for a nil viewer/target is correct for
// ENTITY-level invisibility (a nil perspective is a system render with no one to conceal from). A ROOM- or
// world-level concealment (dark rooms without a light source, a hidden exit) may need to conceal even in a
// nil/absent-light case — such a mechanic must make its OWN decision here, not inherit this early return.
func visibleTo(viewer, target *Entity) bool {
	if viewer == nil || target == nil || viewer == target {
		return true // no perspective, or looking at yourself — never concealed
	}
	if hasFlag(viewer, flagHolylight) {
		return true // see-all: the elevated end of the chokepoint (#28)
	}
	// Staff wizinvis (#30): a concealed staff member is hidden from any viewer of STRICTLY LOWER trust rank
	// (resolved through the zone's content ladder). Equal/higher rank still see them (and holylight, above,
	// already saw everyone) — so a builder hides from mortals but not from an admin. A mortal target's
	// baseline rank 0 means an accidental flagWizinvis (it is reserved, so content can't set it anyway)
	// conceals from no one. Rank via the target's zone ladder; a zone-less target skips the rule.
	if hasFlag(target, flagWizinvis) {
		if z := target.zone; z != nil {
			l := z.trustLadder()
			if l.rank(entityTier(viewer)) < l.rank(entityTier(target)) {
				return false
			}
		}
	}
	if hasFlag(target, flagInvisible) && !hasFlag(viewer, flagDetectInvis) {
		return false
	}
	return true
}
