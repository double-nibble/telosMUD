package world

// act() — perspective messaging (docs/MUDLIB.md §7). The Diku idiom every command leans
// on: one call emits the correct variant of a message to the actor, the victim, and each
// observer in the room, doing the `$`-style substitutions per recipient. Runs on the zone
// goroutine; output reaches each player through its PlayerControlled session sink.
//
// Substitution tokens (MUDLIB §7):
//
//	$n  actor's name (the recipient sees "You" when they ARE the actor)
//	$N  victim's name
//	$p  object's name        $P second object's name
//	$t  literal text arg      $T second literal text arg
//	$$  a literal '$'
//
// Visibility: a recipient who cannot see the referenced actor/victim sees "Someone" /
// "someone" instead of the name — and, critically, NEVER the entity's keywords or the
// fact it was named. The can't-see substitution and the targeting visibility filter share
// the SAME predicate (Zone.canSee), so a command can't leak through messaging what it
// couldn't let you target.
//
// Untrusted-input discipline: substitution is a hand-written single-pass scanner, NOT
// fmt.Sprintf. Names and the $t/$T literal text args (which can be raw player input — a
// say string, a player's chosen name) are copied verbatim into the output; a '%' or '$'
// inside them is data, never a format directive or a re-scanned token. There is no path
// by which player-supplied content becomes a format string.

import "strings"

// ActTo selects which recipients an act() call reaches (MUDLIB §7).
type ActTo int

const (
	// ToActor sends only to the acting entity (its own perspective: "You ...").
	ToActor ActTo = iota
	// ToVictim sends only to the victim entity.
	ToVictim
	// ToRoom sends to every other living, session-backed occupant of the actor's room
	// (the actor and victim each get their own ToActor/ToVictim variant separately when
	// the command wants them; ToRoom is the third-person bystander view).
	ToRoom
	// ToRoomExceptActor is ToRoom that also excludes the victim's special-casing; it is
	// the bystander set (everyone but the actor). Kept distinct for the cases where the
	// actor gets no message at all.
	ToRoomExceptActor
)

// act renders a perspective template and sends it to the chosen recipients (MUDLIB §7).
// tmpl is the template with $-tokens; actor is the acting entity; obj/vict are optional
// referents ($p / $N), either may be nil; t1/t2 are literal text args ($t / $T), used for
// player-supplied strings like a say message. to selects the recipient set.
//
// This is the common 4-referent form; act2 adds the second object ($P, e.g. the container
// in "get $p from $P"). Both funnel through render.
//
// The actor's room (actor.location) defines the broadcast set for ToRoom variants. Each
// recipient gets the template rendered from ITS perspective: $n becomes "You" for the
// actor, "Someone" for an observer who can't see the actor, the name otherwise.
func (z *Zone) act(tmpl string, actor, obj, vict *Entity, t1, t2 string, to ActTo) {
	z.act2(tmpl, actor, obj, nil, vict, t1, t2, to)
}

// act2 is act with an explicit second object referent obj2 ($P) — used by the container
// verbs ("You get $p from $P.", "You put $p in $P."). Everything else is identical to act.
func (z *Zone) act2(tmpl string, actor, obj, obj2, vict *Entity, t1, t2 string, to ActTo) {
	room := actor.location
	switch to {
	case ToActor:
		if pc, ok := sessionOf(actor); ok {
			pc.send(textFrame(z.render(tmpl, actor, actor, obj, obj2, vict, t1, t2)))
		}
	case ToVictim:
		if vict != nil {
			if pc, ok := sessionOf(vict); ok {
				pc.send(textFrame(z.render(tmpl, vict, actor, obj, obj2, vict, t1, t2)))
			}
		}
	case ToRoom, ToRoomExceptActor:
		if room == nil {
			return
		}
		n := 0
		for _, occ := range room.contents {
			if occ == actor {
				continue // the actor sees the ToActor variant, not the bystander one
			}
			if to == ToRoomExceptActor && occ == vict {
				continue
			}
			pc, ok := sessionOf(occ)
			if !ok {
				continue // a mob or item: no session sink (AI/Lua hooks are a later phase)
			}
			pc.send(textFrame(z.render(tmpl, occ, actor, obj, obj2, vict, t1, t2)))
			n++
		}
		z.log.Debug("act", "room", roomRef(room), "tmpl", tmpl, "recipients", n)
	}
}

// render produces the message text for one recipient (MUDLIB §7). viewer is the entity
// reading the line (drives "You" and the can't-see filter); actor/obj/vict are the
// referents; t1/t2 are literal text args. The scan is single-pass and treats every
// non-token byte — including any '$' or '%' inside a substituted name or text arg —
// literally; substituted values are NEVER re-scanned for tokens.
func (z *Zone) render(tmpl string, viewer, actor, obj, obj2, vict *Entity, t1, t2 string) string {
	var b strings.Builder
	b.Grow(len(tmpl) + 16)
	for i := 0; i < len(tmpl); i++ {
		c := tmpl[i]
		if c != '$' || i+1 >= len(tmpl) {
			b.WriteByte(c)
			continue
		}
		i++ // consume the token letter
		switch tmpl[i] {
		case 'n':
			b.WriteString(z.nameFor(viewer, actor, false))
		case 'N':
			b.WriteString(z.nameFor(viewer, vict, false))
		case 'p':
			b.WriteString(z.nameFor(viewer, obj, false))
		case 'P':
			// Second object referent ($P), e.g. the container in "get $p from $P". Renders
			// its name (or nothing if the caller passed none) with the same can't-see guard.
			b.WriteString(z.nameFor(viewer, obj2, false))
		case 't':
			b.WriteString(t1) // literal text arg: copied verbatim, never re-scanned
		case 'T':
			b.WriteString(t2)
		case '$':
			b.WriteByte('$') // $$ -> literal '$'
		default:
			// Unknown token: emit it literally ('$' plus the letter) rather than dropping
			// it, so a stray '$' in a template surfaces in review instead of vanishing.
			b.WriteByte('$')
			b.WriteByte(tmpl[i])
		}
	}
	return b.String()
}

// nameFor returns how viewer should see ent's name in a rendered line (MUDLIB §7):
//
//   - "You" when viewer IS ent (the actor's own perspective), unless cap-less context is
//     wanted — callers that need lowercase mid-sentence pass nothing special here; the
//     verbs this slice ports never need "you" mid-sentence, so "You" is correct.
//   - "Someone" when viewer can't see ent — the visibility leak guard: the name and the
//     entity's mere presence are hidden behind the same predicate the targeter uses.
//   - ent's display name otherwise.
//
// A nil ent renders empty (a template referenced a referent the caller didn't supply).
func (z *Zone) nameFor(viewer, ent *Entity, lower bool) string {
	if ent == nil {
		return ""
	}
	if viewer == ent {
		return "You"
	}
	if !z.canSee(viewer, ent) {
		if lower {
			return "someone"
		}
		return "Someone"
	}
	return ent.Name()
}

// sessionOf returns the session sink for e, if e is a player-controlled entity with a
// live session. Centralizes the entity -> output bridge (the PlayerControlled.session
// path, MUDLIB §7) so act() and any future Sink-based fan-out share one accessor.
func sessionOf(e *Entity) (*session, bool) {
	pc, ok := Get[*PlayerControlled](e)
	if !ok || pc.session == nil {
		return nil, false
	}
	return pc.session, true
}

// roomRef returns a room entity's ProtoRef for logging, "" when nil.
func roomRef(room *Entity) ProtoRef {
	if room == nil {
		return ""
	}
	return room.proto
}
