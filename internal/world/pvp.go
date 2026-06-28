package world

// pvp.go is the PvP / hostility gate (docs/ABILITIES.md §7, docs/PHASE5-PLAN.md §7 / P5-D4) — THE
// SECURITY BOUNDARY. `pvpAllowed(actor, target)` is an ENGINE function over a CONTENT-DEFINED policy:
// the policy DATA (consent flags on players, a safe-room flag, an arena-zone flag) is content; the
// ENFORCEMENT (when each rule applies) is engine-owned and can't be edited by content. This is the
// D2 shape: automatic, can't-forget, defense-in-depth.
//
// # Where it is enforced (two choke points)
//
//  1. Lifecycle STEP 4 (ability.go): a harmful-disposition ability vs a non-consenting player is
//     blocked BEFORE costs — the whole-ability outer layer.
//  2. INSIDE every op that writes another (non-self) PLAYER's state (effect_op.go guardHarmful):
//     deal_damage (dealDamage), a derived-harmful apply_affect (applyDebuff), dispel/remove_affect on
//     another player, and ANY cross-player modify_resource all funnel through ONE shared call. The harm
//     for apply_affect is DERIVED from the affect def (affectIsDetrimental), never trusted from a
//     content label. So even a creatively-authored op, a multi-op on_resolve, a DoT tick, a mislabeled
//     affect, or a future Lua on_resolve (Phase 7) cannot harm a protected player. The in-op layer is
//     the one that survives a step-4 bypass.
//
// Against a NON-player target (a mob) the gate is a no-op (PvP only) — every harmful op against a mob
// proceeds.
//
// # The policy (data-driven; default-deny for player-vs-player)
//
// The DEFAULT is "no PvP" — two players cannot harm each other unless the policy permits it. A harm
// from actor to a player target is allowed ONLY when ALL of these hold:
//
//   - the target is NOT in a safe room (the room carries the "safe" flag);
//   - the ability is not blocked by an arena/zone rule (an "arena" room flag FORCES PvP on, an
//     override that beats individual consent — duels/arenas);
//   - BOTH actor and target have opted in (the "pvp" consent flag), OR the room is an arena.
//
// All three inputs (the safe flag, the arena flag, the pvp consent flag) are content-supplied DATA
// (flags.go) — the engine just reads them. A deployment with no flags at all gets the safe default:
// players cannot harm each other anywhere (the strictest, safest baseline). The order matters:
// safe-room is an absolute veto (even an arena room that is also safe forbids harm); arena forces
// consent; otherwise both must consent.

// pvpAllowed is the engine PvP query (§7). It returns whether `actor` may apply a harmful effect to
// `target`. Against a non-player target it is a no-op (true). Against a player it evaluates the
// content-defined policy below. Pure read of zone-owned + flag state; safe on the zone goroutine.
//
// actor==target (a harmful self-effect — rare but possible) is allowed: you may always affect
// yourself; the gate is about harming OTHERS.
func pvpAllowed(actor, target *Entity) bool {
	if target == nil {
		return false
	}
	// PvP only: a mob target is never gated.
	if !isPlayer(target) {
		return true
	}
	// PvE: a MOB harming a player is always allowed (the gate is PLAYER-vs-player only). Without this a
	// mob could never hit a player in a non-arena room unless the player had opted into PvP — i.e. PvE
	// combat would be impossible (the 6.3b player-death path could never trigger). A nil actor (an
	// orphaned DoT whose source left) is treated as non-player here, so an unattributed harm still lands
	// on a player (the kill is just unattributed); the detached-actor FAIL-CLOSED in guardHarmful covers
	// the stale-pointer case before this is reached.
	if actor != nil && !isPlayer(actor) {
		return true
	}
	// Self-harm is always allowed (the gate guards harming OTHER players).
	if actor == target {
		return true
	}

	// Safe room is an ABSOLUTE, ENGINE-ENFORCED veto: no harm lands on a player in a safe room —
	// a content Lua policy CANNOT override this (the safe-room guarantee is a security property the
	// engine owns, not delegated to content). Checked BEFORE the Lua policy so a buggy/hostile
	// policy can never open a safe room.
	if inSafeRoom(target) {
		return false
	}

	// A content Lua pvp_allowed policy (7.4f), when the pack defines one, DECIDES the genuine
	// player-vs-player case (replacing the arena/consent-flag default below). It is FAIL-CLOSED: a
	// missing/erroring policy, or any non-`true` return, DENIES harm (invokeForBool enforces this).
	// The safe-room veto above already ran, so the policy can only ever be MORE restrictive than
	// the absolute vetoes, never less. Consulted via the actor/target's zone runtime.
	if body, rt := zonePvpPolicy(actor, target); body != "" && rt != nil {
		return rt.consultPvpPolicy(body, actor, target)
	}

	// Arena forces PvP: an arena room overrides individual consent (duels). Both must be in the
	// arena context — we key it off the TARGET's room (where the harm lands).
	if inArenaRoom(target) && inArenaRoom(actor) {
		return true
	}

	// Default: both the actor and the target must have opted in (the "pvp" consent flag). With no
	// consent flags this is false — the safe default (players cannot harm each other).
	return hasFlag(actor, flagPvP) && hasFlag(target, flagPvP)
}

// zonePvpPolicy returns the pack's Lua pvp_allowed body + the zone runtime to run it on, if a
// policy is defined and a live zone is reachable from the actor/target. Empty body => use the
// engine default.
func zonePvpPolicy(actor, target *Entity) (string, *luaRuntime) {
	z := entityZone(actor)
	if z == nil {
		z = entityZone(target)
	}
	if z == nil || z.lua == nil {
		return "", nil
	}
	b := z.defBundle()
	if b == nil {
		return "", nil
	}
	return b.pvpLua, z.lua
}

// entityZone returns e's zone, or nil.
func entityZone(e *Entity) *Zone {
	if e == nil {
		return nil
	}
	return e.zone
}

// The content-defined policy flag names. They are ordinary string flags (flags.go) the engine reads;
// content sets them (a player opts in with flagPvP; a builder marks a room safe/arena). The engine
// never invents a flag — these are the names the policy CONSULTS, the open-set discipline (pillar).
const (
	flagPvP   = "pvp"   // a player consent flag: this player has opted into PvP
	flagSafe  = "safe"  // a room flag: no harm may land on a player here (an absolute veto)
	flagArena = "arena" // a room flag: PvP is forced ON here (overrides individual consent)
)

// inSafeRoom reports whether e is standing in a room flagged "safe".
func inSafeRoom(e *Entity) bool { return roomHasFlag(e, flagSafe) }

// inArenaRoom reports whether e is standing in a room flagged "arena".
func inArenaRoom(e *Entity) bool { return roomHasFlag(e, flagArena) }

// roomHasFlag reports whether the room e currently occupies carries the named flag. A detached entity
// (location nil) or a room with no flag set reports false.
func roomHasFlag(e *Entity, flag string) bool {
	if e == nil || e.location == nil {
		return false
	}
	return roomFlag(e.location, flag)
}
