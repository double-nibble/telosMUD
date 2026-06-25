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
	if ts.bare {
		return true
	}
	if len(ts.keywords) == 0 {
		return false
	}
	for _, word := range ts.keywords {
		hit := false
		for _, kw := range e.keywords {
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

	switch {
	case spec.all:
		z.log.Debug("targeting: all", "actor", actor.short, "keywords", spec.keywords, "matches", len(hits))
		return hits
	case spec.index > 0:
		// N.keyword: the Nth (1-based) match. Out of range yields nothing (no panic).
		if spec.index <= len(hits) {
			z.log.Debug("targeting: nth", "actor", actor.short, "n", spec.index, "keywords", spec.keywords)
			return hits[spec.index-1 : spec.index]
		}
		z.log.Debug("targeting: nth out of range", "actor", actor.short, "n", spec.index, "have", len(hits))
		return nil
	case spec.index < 0:
		// Defensive: a negative selector (shouldn't occur — atoiBounded rejects '-') is
		// treated as no match rather than indexing oddly.
		return nil
	default:
		// Unqualified keyword: the first visible match.
		if len(hits) > 0 {
			z.log.Debug("targeting: first", "actor", actor.short, "keywords", spec.keywords)
			return hits[:1]
		}
		z.log.Debug("targeting: no match", "actor", actor.short, "keywords", spec.keywords)
		return nil
	}
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
		case ScopeEquipment, ScopeContainer:
			// Functional in slice 4; no worn items or opened containers exist yet.
		}
	}
	return out
}

// roomContents returns the contents of room, or nil when room is nil (a detached actor,
// e.g. mid-handoff). Centralizes the nil guard so every scope branch stays simple.
func roomContents(room *Entity) []*Entity {
	if room == nil {
		return nil
	}
	return room.contents
}

// canSee is the visibility filter (MUDLIB §7): an actor may only target what it can
// perceive. Slice 1 left room/entity flags as stubs (no dark/invis/hidden data yet), so
// this is the honest trivial filter: everything in scope is visible. It is the single
// chokepoint the resolver consults, so when content supplies dark/invis/hidden flags
// (Phase 5+) the rule lands here and every command inherits it — and, critically, the
// can't-see leak surface (see act.go) is gated by the SAME predicate.
func (z *Zone) canSee(actor, target *Entity) bool {
	return true
}
