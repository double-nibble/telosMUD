package world

// Targeting (classic Diku grammar — docs/MUDLIB.md §7). A command names a target with a
// keyword phrase; the resolver searches a command-defined set of scopes, applies a
// visibility filter, and returns the matched entity or entities. Runs on the zone
// goroutine over zone-owned containment data; no locks, no blocking.
//
// Grammar (MUDLIB §7):
//
//	sword        -> first visible match for keyword "sword"
//	2.sword      -> the 2nd matching "sword"
//	all.coin     -> every matching "coin"
//	all          -> everything in scope
//	long sword   -> entity whose keywords include both "long" and "sword" (isname)
//
// A multi-word phrase matches an entity when EVERY typed word is a prefix of one of the
// entity's keywords (Diku isname semantics), already implemented by
// Entity.contentsByKeyword for the single-word case and generalized here.
//
// Untrusted-input discipline: the index in `N.kw` is parsed defensively — `0.x`, a huge
// N, an empty keyword, or junk before the dot never panic and never do unbounded work.

import "strings"

// Scope names a set of entities the resolver searches (MUDLIB §7). Commands pass scopes
// in priority order; the resolver concatenates the candidate lists in that order so a
// bare `sword` resolves room-floor-then-inventory (or whatever order the verb declares).
type Scope int

// Scope values: where a keyword-resolve search looks for its target.
const (
	ScopeInventory  Scope = iota // the actor's own contents (inventory)
	ScopeEquipment               // worn/wielded items (functional in slice 4)
	ScopeRoomLiving              // mobs/players in the actor's room
	ScopeRoomItems               // ground items in the actor's room
	ScopeContainer               // an opened container's contents (slice 4)
)

// TargetSpec is a parsed Diku target phrase. all selects every match; index selects the
// Nth (1-based) match (0 means "first", the unqualified form); keywords are the typed
// words each of which must prefix-match one of a candidate's keywords (isname). bare is
// the literal `all` with no keyword (everything in scope).
type TargetSpec struct {
	all      bool
	bare     bool // the literal "all" (no keyword)
	index    int  // 1-based Nth selector; 0 means first/unqualified
	keywords []string
}

// empty reports whether the spec names no target at all (the verb was given no argument).
// Such a spec matches nothing.
func (ts TargetSpec) empty() bool { return !ts.bare && len(ts.keywords) == 0 }

// parseTargetSpec parses a raw target argument into a TargetSpec (MUDLIB §7). It handles:
//
//	""           -> empty spec (matches nothing)
//	"all"        -> bare all (everything in scope)
//	"all.coin"   -> every match for "coin"
//	"2.sword"    -> the 2nd match for "sword"
//	"long sword" -> isname match on both words
//
// All numeric/edge cases are bounded and panic-free: a non-numeric or out-of-range
// prefix before the dot is treated as part of the keyword (Diku behavior), `0.x` and a
// negative or absurd N collapse to a spec that matches nothing meaningful (index<=0 with
// an explicit selector yields no match), and an empty keyword after `all.`/`N.` yields an
// empty keyword list (no match). Input length is never used to size work beyond the token
// count the player actually typed.
func parseTargetSpec(arg string) TargetSpec {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return TargetSpec{}
	}
	// A "selector.keyword" prefix (all.X or N.X) only applies to the FIRST word; the
	// remaining words are additional isname keywords.
	first, restWords := split(arg)
	var ts TargetSpec

	if dot := strings.IndexByte(first, '.'); dot >= 0 {
		sel, kw := first[:dot], first[dot+1:]
		switch {
		case strings.EqualFold(sel, "all"):
			ts.all = true
			first = kw
		default:
			if n, ok := atoiBounded(sel); ok {
				// An explicit numeric selector. n>=1 selects the Nth match; an explicit
				// "0.x" is a selector that can match nothing, so map it to -1 (the
				// "explicit-but-no-match" sentinel) to keep it distinct from an
				// unqualified keyword (index 0, "first match").
				if n == 0 {
					ts.index = -1
				} else {
					ts.index = n
				}
				first = kw
			}
			// else: "sel" is not "all" and not a number (e.g. "a.sword") — leave first
			// intact so the dot is part of the keyword (Diku treats it literally).
		}
	} else if strings.EqualFold(first, "all") && restWords == "" {
		// Bare "all": everything in scope, no keyword.
		ts.all = true
		ts.bare = true
		return ts
	}

	// Collect keywords: the (possibly selector-stripped) first word plus the rest.
	if first != "" {
		ts.keywords = append(ts.keywords, strings.ToLower(first))
	}
	for restWords != "" {
		var w string
		w, restWords = split(restWords)
		if w != "" {
			ts.keywords = append(ts.keywords, strings.ToLower(w))
		}
	}
	return ts
}

// atoiBounded parses a small non-negative decimal selector. It accepts only digits and
// caps the magnitude well below any realistic scope size, so a pasted megadigit string
// can't drive large work or overflow — it simply fails to parse and is treated as a
// keyword. Returns (n, true) on a clean parse, (0, false) otherwise.
func atoiBounded(s string) (int, bool) {
	if s == "" || len(s) > 6 { // 6 digits caps N at <1e6; no realistic scope is that big
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
	}
	return n, true
}

// matches reports whether entity e satisfies the spec's keyword phrase under isname
// semantics: every typed word must be a prefix of one of e's keywords (MUDLIB §7). A
// bare "all" (no keywords) matches everything. Case-insensitive; spec keywords are
// already lower-cased by parseTargetSpec.
func (ts TargetSpec) matches(e *Entity) bool {
	return ts.matchesKeywords(e.keywordList())
}

// matchesKeywords is the isname core (MUDLIB §7) over a raw keyword list, factored out of matches so
// NON-entity targets (a content recipe's alias tokens, #34) resolve by the SAME grammar the item/mob
// resolver uses: every typed word must prefix-match one of the candidate's keywords. A bare "all"
// matches everything; an empty spec matches nothing.
func (ts TargetSpec) matchesKeywords(kws []string) bool {
	if ts.bare {
		return true
	}
	if len(ts.keywords) == 0 {
		return false
	}
	for _, word := range ts.keywords {
		hit := false
		for _, kw := range kws {
			if len(word) <= len(kw) && strings.EqualFold(kw[:len(word)], word) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// Resolve searches the given scopes in order for entities matching spec, applies the
// visibility filter, and returns the selection per the Diku grammar (MUDLIB §7): the
// Nth match for `N.kw`, every match for `all.kw` / `all`, the first match otherwise.
//
// actor is the perceiving entity (its inventory/equipment/room are the scope sources and
// the visibility check is from its perspective). Resolution is bounded by the candidate
// count, which is the population of the searched scopes — never by the input string.
func (z *Zone) Resolve(actor *Entity, spec TargetSpec, scopes ...Scope) []*Entity {
	if spec.empty() {
		z.log.Debug("targeting: empty spec", "actor", actor.short)
		return nil
	}
	candidates := z.scopeCandidates(actor, scopes...)

	// Filter to visible matches in scope order.
	var hits []*Entity
	for _, e := range candidates {
		if !z.canSee(actor, e) {
			continue
		}
		if spec.matches(e) {
			hits = append(hits, e)
		}
	}

	z.log.Debug("targeting: resolve", "actor", actor.short, "keywords", spec.keywords,
		"all", spec.all, "index", spec.index, "candidates", len(hits))
	return selectMatches(spec, hits)
}

// scopeCandidates concatenates the entity lists for the requested scopes in order, so a
// command that searches room-then-inventory finds the floor item before the carried one
// (MUDLIB §7). The actor is never returned as a candidate for itself in room scopes.
// Equipment/Container are wired in slice 4 (no worn items / opened containers yet); they
// return nothing here so the shape is in place without inventing state.
func (z *Zone) scopeCandidates(actor *Entity, scopes ...Scope) []*Entity {
	var out []*Entity
	room := actor.location
	for _, sc := range scopes {
		switch sc {
		case ScopeInventory:
			out = append(out, actor.contents...)
		case ScopeRoomLiving:
			for _, e := range roomContents(room) {
				if e != actor && Has[*Living](e) {
					out = append(out, e)
				}
			}
		case ScopeRoomItems:
			for _, e := range roomContents(room) {
				if e != actor && !Has[*Living](e) {
					out = append(out, e)
				}
			}
		case ScopeEquipment:
			// Worn/wielded items: the actor's Wearer slot map (components.go). A worn item
			// also remains in the actor's contents (equipped is a state over a carried item),
			// so ScopeEquipment and ScopeInventory can both surface it — commands choose the
			// scope set that matches their semantics (remove searches equipment; get-from-
			// floor never does).
			if wr, ok := Get[*Wearer](actor); ok {
				for _, loc := range z.wearSlots().orderedRefs() {
					if e := wr.worn[loc]; e != nil {
						out = append(out, e)
					}
				}
			}
		case ScopeContainer:
			// A bare ScopeContainer has no container to search — `get x from y` resolves the
			// container explicitly and searches its contents via containerContents (below),
			// not through this scope. Left empty so the scope constant stays meaningful for
			// callers that pass an explicit container.
		}
	}
	return out
}

// The deterministic slot order ScopeEquipment surfaces worn items in (so `remove 2.ring`-style selection and
// the equipment list agree) is now the CONTENT wear-slot order — z.wearSlots().orderedRefs() (#35), replacing
// the old fixed-enum package var.

// resolveInContainer searches an opened container's contents for spec, applying the same
// visibility filter and Diku selection as Resolve (MUDLIB §7). It is the explicit-container
// path `get <item> from <container>` / `put <item> in <container>` use: the command resolves
// the container first, then calls this against its contents. A closed container yields
// nothing (the caller checks closed separately to message "It is closed."). Runs on the
// zone goroutine over zone-owned data.
func (z *Zone) resolveInContainer(actor, container *Entity, spec TargetSpec) []*Entity {
	if spec.empty() {
		return nil
	}
	var hits []*Entity
	for _, e := range container.contents {
		if !z.canSee(actor, e) {
			continue
		}
		if spec.matches(e) {
			hits = append(hits, e)
		}
	}
	return selectMatches(spec, hits)
}

// selectMatches applies the Diku Nth/all/first selection to an already-filtered candidate
// list (MUDLIB §7). Factored out of Resolve so the container path shares the exact selection
// semantics rather than re-deriving them.
func selectMatches(spec TargetSpec, hits []*Entity) []*Entity {
	switch {
	case spec.all:
		return hits
	case spec.index > 0:
		if spec.index <= len(hits) {
			return hits[spec.index-1 : spec.index]
		}
		return nil
	case spec.index < 0:
		return nil
	default:
		if len(hits) > 0 {
			return hits[:1]
		}
		return nil
	}
}

// roomContents returns the contents of room, or nil when room is nil (a detached actor,
// e.g. mid-handoff). Centralizes the nil guard so every scope branch stays simple.
func roomContents(room *Entity) []*Entity {
	if room == nil {
		return nil
	}
	return room.contents
}

// areaTargets resolves the set of living entities an area-scoped op ([G12], docs/PHASE6-PLAN.md §1.3)
// applies to, given the effect ctx (the actor's room is the origin) and an area selector:
//
//	"room"               -> every living target in the actor's room, MINUS the actor itself if the
//	                        effect is harmful (don't fireball yourself — disposition decides friend/foe,
//	                        matching the PvP/disposition model; the per-target guardHarmful is the real
//	                        gate, this is just the obvious self-exclusion).
//	"room_and_adjacent"  -> the actor's room plus every room ONE EXIT away that THIS zone owns. A
//	                        SAME-ZONE-ONLY containment: an exit whose destination is in another zone (or
//	                        another shard) is EXCLUDED — we never dereference a cross-zone *Entity (the
//	                        5.2/6.3a single-writer rule). A cross-zone AoE consequence is a reserved
//	                        Phase-10 director concern, a no-op here.
//
// THE CONTAINMENT IS STRUCTURAL (the #1 distsys review point): the ONLY rooms this enumerates are the
// actor's own room and rooms found via z.rooms[destRoom] — the zone's OWN room map. parseRef gives the
// exit's destination zone; if it is non-empty and != z.id, the exit is skipped BEFORE any room/entity
// is touched. There is no path here that loads, messages, or dereferences a room another zone owns.
// Single-writer: zone goroutine, reading only this zone's containment.
func areaTargets(c *effectCtx, area string) []*Entity {
	if c == nil || c.actor == nil {
		return nil
	}
	origin := c.actor.location
	if origin == nil {
		return nil
	}
	harmful := c.disp == dispHarmful
	var out []*Entity
	collect := func(room *Entity) {
		for _, e := range roomContents(room) {
			if e.living == nil {
				continue
			}
			// Self-exclusion for a harmful AoE: don't catch the caster in their own fireball. A
			// helpful/neutral area op (a room heal/buff) INCLUDES the actor.
			if e == c.actor && harmful {
				continue
			}
			out = append(out, e)
		}
	}
	collect(origin)
	if area == "room_and_adjacent" {
		z := c.z
		if z != nil && origin.room != nil {
			for _, dir := range origin.room.sortedExits() {
				ref := origin.room.exits[dir]
				destZone, destRoom := parseRef(ref)
				// SAME-ZONE CONTAINMENT: a cross-zone (or cross-shard) exit is excluded ENTIRELY. We
				// never look the room up anywhere but THIS zone's own map, so a destination this zone
				// does not own is simply absent — no cross-goroutine *Entity is ever reached.
				//
				// ownsZoneRef keeps that true for an instance (#72) without shrinking the spell: an
				// instance's authored refs name the template, so a raw `!= z.id` would exclude EVERY
				// exit and silently degrade room_and_adjacent to room-only inside every copy — the
				// same content quietly behaving differently in an instance than in the template.
				if !z.ownsZoneRef(destZone) {
					if z.log != nil {
						z.log.Debug("areaTargets: cross-zone exit excluded (same-zone containment)",
							"dir", dir, "dest_zone", destZone, "this_zone", z.id)
					}
					continue
				}
				if adj := z.rooms[destRoom]; adj != nil && adj != origin {
					collect(adj)
				}
			}
		}
	}
	return out
}

// canSee is the visibility filter (MUDLIB §7): an actor may only target what it can perceive. It is the
// single chokepoint the resolver + the act() leak surface consult, so the concealment rule lands in ONE
// place (visibleTo, visibility.go) and every command inherits it. As of #28 it honors the invisibility /
// detect-invis / holylight flags; dark rooms and hidden/sneak are follow-up slices that extend visibleTo.
func (z *Zone) canSee(viewer, target *Entity) bool {
	return visibleTo(viewer, target)
}
