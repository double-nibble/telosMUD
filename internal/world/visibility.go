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

	// Room darkness model (#99). A room authored with flagDark is unlit: an ordinary viewer standing in it
	// perceives nothing but itself, UNLESS the room holds a light source or the viewer can see in the dark.
	// All three tokens are open-set flags/tags content sets and the engine reads (the pillar) — no schema
	// change: flagDark rides the room's namedFlags (content_map.go), flagInfravision the viewer's Living
	// flags (a racial/effect grant), and a light source is any co-located entity that emitsLight.
	flagDark        = "dark"        // ROOM: unlit — occupants concealed from ordinary viewers (namedFlags)
	flagInfravision = "infravision" // viewer: sees in an unlit room (a racial / detect-effect grant)
	flagLight       = "light"       // light source: a Living glow flag (a light spell) OR an item ItemMeta tag

	// Hidden/sneak model (#100). flagHidden conceals a target from an ordinary viewer the same way
	// flagInvisible does, but it is the MUNDANE-stealth carrier (a hide/sneak skill), pierced by a distinct
	// sense — flagSenseHidden (a perception/spot capability) — rather than detect-invisibility. Keeping them
	// separate lets content model "I can see the invisible but still miss the sneak-thief" and vice-versa.
	// The perception CONTEST (viewer skill vs target stealth) is authored as CONTENT (a hide ability runs a
	// contested `check` op and, on success, applies flagHidden; a keen-eyed viewer carries flagSenseHidden
	// from a passive-perception affect) — the engine supplies the concealment primitive + chokepoint wiring,
	// not a hardcoded skill, exactly as it did for flagInvisible/detect_invis. A hidden mover also moves
	// SILENTLY: presence lines (act.go actConceal) suppress entirely for any viewer this predicate hides them
	// from. NOTE this is presence-LINE stealth only — like flagInvisible, it does not evade an aggressive mob's
	// aggroOnEntry (that mob still attacks; aggro-evasion, if ever wanted, is a separate deliberate choice for
	// BOTH hidden and invisible). Bystanders still see the ensuing combat as "Someone" (nameFor via canSee).
	flagHidden      = "hidden"       // target: mundanely concealed (a hide/sneak skill) — pierced by sense_hidden
	flagSenseHidden = "sense_hidden" // viewer: perceives a flagHidden target (a spot/perception capability)
)

// SECURITY POSTURE (#28, updated once the builder trust tier #27 landed): holylight is now a RESERVED trust
// flag (see reservedFlags in tier.go) — content set_flag/clear_flag refuse it, it is never persisted, and a
// forged snapshot can't inject it, so no player can self-grant see-all. detect_invis is DELIBERATELY left a
// plain open-string gameplay flag (a detect-invisibility effect/racial), which is safe because (a) canSee
// gates PERCEPTION ONLY — never harm/authz (the harm gate is guardHarmful, separate), so piercing invisibility
// is a pure information capability that cannot bypass the hostility gate; and (b) detect_invis pierces only
// flagInvisible, never a staff member's flagWizinvis (a distinct trust-rank branch in visibleTo below). See
// #27/#97. flagSenseHidden (#100) is the same safe shape as detect_invis — a plain gameplay perception flag
// that pierces only flagHidden (mundane stealth), never wizinvis; and flagHidden itself is a plain gameplay
// concealment flag (a hide/sneak skill), NOT a trust flag, so it grants no elevation and gates perception only.

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
		// No perspective, or looking at yourself — never concealed. This early return is ALSO correct for
		// room darkness (#99): a nil viewer is a system render with no one to blind, a nil target is nothing
		// to see, and you always perceive yourself regardless of light. Darkness makes its OWN decision below
		// (a co-located viewer/target reaching the dark check) rather than inheriting this return blindly.
		return true
	}
	if hasFlag(viewer, flagHolylight) {
		return true // see-all: the elevated end of the chokepoint (#28) — pierces invisibility AND darkness
	}
	// Room darkness (#99): a viewer in an unlit dark room perceives no other occupant OF THAT ROOM. Checked
	// here, at the chokepoint, so every co-located canSee consumer (targeting, act() messaging, lookRoom, GMCP
	// occupants) inherits it uniformly. Darkness is a PER-ROOM property, so it is gated on co-location: the
	// one zone-WIDE caller (whoLocal walks every player in the zone, not just the viewer's room) must not have
	// its whole roster blanked just because the viewer stands in the dark. infravision (cheap flag, checked
	// first) and holylight (above) are the two ways to see without light.
	if !hasFlag(viewer, flagInfravision) && viewer.location == target.location && roomIsDark(viewer.location) {
		return false
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
	// Mundane stealth (#100): a hidden target is concealed unless the viewer can spot the hidden. Distinct
	// from invisibility (magical) and its detect — a separate sense so content can grant one without the other.
	if hasFlag(target, flagHidden) && !hasFlag(viewer, flagSenseHidden) {
		return false
	}
	return true
}

// roomIsDark reports whether room is currently UNLIT (#99): it carries the authored flagDark AND no light
// source is present to dispel it. A non-room entity, a room without flagDark, or a dark room holding a light
// source is not dark. This is the room-level counterpart to the entity-level concealment flags above — the
// perception locale's own decision (visibleTo consults it for the VIEWER's room), never inherited from an
// early return. A nil room (a viewer with no location — mid-handoff, a container) is treated as not dark:
// darkness is a room property and there is no room to be dark.
func roomIsDark(room *Entity) bool {
	if room == nil || !roomFlag(room, flagDark) {
		return false
	}
	return !roomIsLit(room)
}

// roomIsLit reports whether any entity in room emits light (#99). It scans the room's immediate contents —
// each occupant (a Living carrying the flagLight glow, or a light-source item lying on the ground) and each
// occupant's OWN immediate contents (a carried/worn torch) — and short-circuits on the first light found.
// One level of nesting deep: a lit torch works in hand or on the ground, not buried inside a closed bag.
// Called only for a dark room with a light-blind viewer (roomIsDark gates it), so the scan is a cold path.
func roomIsLit(room *Entity) bool {
	for _, occ := range room.contents {
		if emitsLight(occ) {
			return true
		}
		for _, held := range occ.contents {
			if emitsLight(held) {
				return true
			}
		}
	}
	return false
}

// canSeeRoomContents reports whether viewer can perceive its current room at all (#99) — its description,
// exits, and occupants. False ONLY when the viewer stands in an unlit dark room with no way to see: lookRoom
// uses it to render the pitch-black notice in place of the room. holylight and infravision both see; a light
// source in the room (incl. one the viewer carries) makes roomIsDark false, so it sees too.
func canSeeRoomContents(viewer *Entity) bool {
	if viewer == nil {
		return true
	}
	if hasFlag(viewer, flagHolylight) || hasFlag(viewer, flagInfravision) {
		return true
	}
	return !roomIsDark(viewer.location)
}

// emitsLight reports whether entity e is itself a light source (#99): a Living carrying the flagLight glow
// (a light spell / luminous creature) or an item whose ItemMeta bears the flagLight tag (a torch/lantern).
// The two carriers share the one token so content authors a single "light" concept on either an effect or
// an item. It never recurses — roomIsLit handles the one level of container nesting.
func emitsLight(e *Entity) bool {
	return hasFlag(e, flagLight) || hasItemTag(e, flagLight)
}
