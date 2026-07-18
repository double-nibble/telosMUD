package world

import (
	lua "github.com/yuin/gopher-lua"
)

// luainstance.go — the CONTENT surface for instanced zones (#72): mud.send_to_instance.
//
// One primitive, deliberately. Everything else about an instance is engine mechanism that content should not
// be able to reach: content does not mint, does not name, does not reap, and cannot enumerate instances. What
// content decides is the POLICY — which room is a dungeon door, who may go through it, what it costs, what
// happens on the way — and that needs exactly one verb: "send this player into a private copy of that zone".
//
// The engine/content split here is the pillar (docs/PRINCIPLES.md) applied to a mechanism that is unusually
// tempting to leak: an instance has an ID, and an id is a string, and a string is one `return` away from being
// content-visible. It is not exposed. An instance id is unguessable BY DESIGN (instance.go: a monotonic serial
// would be enumerable, and an enumerable id composes with the loot stream into a farming oracle), and handing
// content the id would make the routing predicate — which is a PRIVACY boundary between two parties' copies of
// a dungeon — depend on content discretion.

// mudSendToInstance is the entry primitive: mud.send_to_instance(target, template).
//
// FIRE-AND-FORGET from Lua's perspective, which is the honest contract over an inherently async flow. It
// returns true when the request was ACCEPTED for dispatch, not when the player has arrived — the actual mint
// happens on a shard worker over hundreds of milliseconds to seconds (a full zone build), and the crossing
// happens later still, back on this zone's goroutine. There is deliberately no callback, no future and no
// "wait for it": a Lua script must never be able to make a zone actor wait for store I/O and a zone build, and
// every failure between here and arrival is already reported to the PLAYER as a line of text (see
// instanceReady). A script that needs to know the player left can subscribe to movement like anything else.
//
// SECURITY — THIS PRIMITIVE IS SELF-ONLY IN v1. The target must BE the invoking actor; every other send is
// refused outright, before the template is even looked at.
//
// Why a blanket restriction rather than the harm gate. A forced relocation of another player is harm, and this
// is the most severe form of it the engine can express: the destination is across a zone boundary, is private,
// is chosen by the script's author, and the victim cannot be seen, reached or helped from outside it. The
// obvious control is mayRelocate — the gate h:teleport and h:recall use — but it does NOT cover this primitive's
// threat model, on two counts that were verified against the code rather than assumed:
//
//   - IT DOES NOT GATE A MOB ACTOR AT ALL. mayRelocate -> guardHarmful -> pvpAllowed, and pvpAllowed
//     short-circuits on !isPlayer(actor) BEFORE the safe-room veto. So for a mob-actor invocation — a mob tick,
//     an aggro handler, a mob-owned ability — the gate returns true UNCONDITIONALLY. A cultist standing in a
//     temple or a newbie inn could therefore send a non-consenting player into a private dungeon, and none of
//     pvp policy, safe rooms, consent or spawn protection would apply. This is inherited from hTeleport, so it
//     is not a regression of the gate; the difference is that teleport moves you within observable space and
//     this moves you OUTSIDE it, which is exactly why reusing the gate unchanged here is wrong.
//   - IT SELF-EXEMPTS. mayRelocate returns true immediately when e == rt.inv.actor, so for the majority of real
//     call sites — the self case, which is the whole idiom — it is a NO-OP, not a policy chokepoint. Do not
//     read a mayRelocate call as evidence that a path is gated.
//
// Self-only kills the entire class, and it costs nothing content actually wants: the "you step through the
// shimmering gate" idiom the design is for IS the self case, and it is unaffected.
//
// WHAT A CONSENTED PARTY-SUMMON WOULD ADDITIONALLY NEED (slice 5, if it is ever wanted). Relaxing this is not a
// matter of deleting the check. It needs, at minimum: (a) EXPLICIT TARGET CONSENT — an accepted invitation from
// the target themselves, not a pvp flag and not party membership, because being in a party is not agreement to
// be moved out of the observable world; and (b) a SAFE-ROOM REFUSAL THAT DOES NOT EXEMPT MOB ACTORS, i.e. a
// veto evaluated on the ROOM and the TARGET rather than routed through pvpAllowed's actor short-circuit.
// Neither exists today, which is why v1 does not ship the capability.
//
// The ACCOUNT the instance's caps are charged to is NOT a parameter and can never be. It is read from the
// target's session, where it was placed from a VERIFIED assertion (session.account). A Lua-supplied account
// would make the per-account concurrent cap, the mint rate limit and the global cap all self-service — a
// script could charge every mint to a different invented account and mint without bound.
//
// A refusal is a clean `false`, never an error: the caller is content reacting to a player action, and a
// raised error would abort the whole trigger (the door script's flavor text, its cost deduction, its logging)
// for what is an ordinary "not right now". The player has already been told why.
func (rt *luaRuntime) mudSendToInstance(l *lua.LState) int {
	if rt.denyInDisplay(l, "send_to_instance") {
		return 0
	}
	target := resolveHandle(l, 1)
	template := l.CheckString(2)
	if target == nil {
		l.Push(lua.LFalse)
		return 1
	}
	// THE SELF-ONLY GATE, FIRST. Before the template is validated, before the session is resolved, before
	// anything is sent to the player — so a send at anybody but the invoker is a total no-op, indistinguishable
	// from the call never having been made. In particular it must not leak whether the template exists.
	//
	// A nil invocation actor is refused too: "who is acting" is the entire input to this decision, and a caller
	// that cannot answer it has no business moving anybody. Fail closed.
	if rt.inv == nil || rt.inv.actor == nil || target != rt.inv.actor {
		rt.log.Debug("mud.send_to_instance refused: it is SELF-ONLY (v1) and the target is not the invoking actor",
			"rid", target.rid)
		l.Push(lua.LFalse)
		return 1
	}
	s, ok := sessionOf(target)
	if !ok {
		// A mob or an item. Instances are entered by PLAYERS: the whole mechanism is anchored on a session
		// (the anchor, the account, the durable location, the reconnect story), and a mob has none of it.
		// A clean false, not an error — a script sweeping a room and offering the door to everyone in it
		// should not blow up on the resident shopkeeper.
		l.Push(lua.LFalse)
		return 1
	}
	if rt.zone == nil {
		l.Push(lua.LFalse)
		return 1
	}
	// `true` for the gate decision: the SELF-ONLY check above IS that decision, made by the only code that knows
	// the invocation actor. requestInstanceEntry refuses outright when it is not supplied, so a future caller
	// cannot accidentally reach it un-gated.
	l.Push(lua.LBool(rt.zone.requestInstanceEntry(s, template, true)))
	return 1
}
