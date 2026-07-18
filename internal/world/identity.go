package world

import "sync/atomic"

// Identity (docs/MUDLIB.md §1). Three distinct kinds of id, on purpose:
//
//   - ProtoRef   content key, authored, stable forever      ("midgaard:room:temple")
//   - RuntimeID  one running shard process; ephemeral        (a uint64 counter)
//   - PersistID  durable identity for things we save (UUID)  (players, unique items)
//
// Slice 1 (docs/PHASE3-PLAN.md) wires all three into the entity but only ProtoRef and
// RuntimeID carry weight: ProtoRef keys rooms and exits, RuntimeID is the cheap live
// handle. PersistID is plumbed-but-nil until persistence lands in Phase 4.

// ProtoRef is the stable content key for a prototype (room/mob/item template) in the
// content DB. It is canonically "zone:kind:name" — e.g. "midgaard:room:temple". A
// room's display name ("The Temple Square") is data on the entity, decoupled from its
// ref, so the name can change without breaking exits or saves (the room-identity
// separation tabled in earlier phases, MUDLIB §3). Exit destinations are ProtoRefs.
type ProtoRef string

// RuntimeID is the cheap in-memory handle for live references (target pointers, future
// aggro lists). Allocated per zone, ephemeral, never persisted, never crosses a shard
// boundary (MUDLIB §1). 0 is the zero value for an unallocated entity.
type RuntimeID uint64

// PersistID is the durable UUID carried only by entities with saved state (every
// player; items flagged persistent). Stored as a string so it can stay nil for the
// common spawned-mob/item case. Plumbed but unused in Phase 3 — real in Phase 4.
type PersistID string

// ridAllocator hands out per-zone RuntimeIDs. It lives on the Zone and is only touched
// by the zone goroutine, so the atomic is belt-and-suspenders rather than load-bearing;
// it keeps the contract honest if a future caller allocates off-goroutine.
type ridAllocator struct {
	next atomic.Uint64
}

// alloc returns the next RuntimeID for this zone (1-based; 0 stays "unallocated").
func (a *ridAllocator) alloc() RuntimeID {
	return RuntimeID(a.next.Add(1))
}

// entityByRID resolves a RuntimeID to the live *Entity in THIS zone, or nil if no entity in
// this zone carries that rid. It is the re-resolution primitive a Lua handle calls on EVERY
// method (luahandle.go, docs/PHASE7-PLAN.md §1.2 / T7): a handle never holds an *Entity, only
// a (rid, zone) pair, and re-resolves through here so a dead/departed/cross-zone rid becomes a
// safe nil — no dangling pointer, no cross-zone reach.
//
// It walks the zone's containment tree: the room entities themselves (a room handle resolves
// here too — rooms get rids via spawn) and, recursively, each room's contents (occupants,
// ground items, and items nested inside containers). A zone holds few rooms at modest
// populations, so this O(entities-in-zone) walk is fine for script method calls (not a
// combat-hot path); if a future slice needs it hot, a maintained rid->entity index can sit
// behind this same accessor without touching callers. Single-writer: zone goroutine only.
// rehomeSubtree re-homes e AND its entire contents subtree into THIS zone's identity space: each
// entity gets a freshly-allocated local rid (z.rids) and its zone pointer repointed to z. It is the
// intra-shard transfer fix (Zone.transferIn): a live *Entity object carried between co-hosted zones
// keeps the SOURCE zone's rids, which collide with this zone's own (both allocators are 1-based and
// independent), and entityByRID would then resolve a Lua handle to whichever colliding entity Go's
// randomized map iteration hit first. Reassigning local rids to the whole subtree restores the
// per-zone-unique-rid invariant entityByRID depends on. Single-writer: zone goroutine only (called
// from transferIn before the entity is placed, so no concurrent handle resolve races the reassignment).
func (z *Zone) rehomeSubtree(e *Entity) {
	if e == nil {
		return
	}
	e.rid = z.rids.alloc()
	e.zone = z
	for _, c := range e.contents {
		z.rehomeSubtree(c)
	}
}

func (z *Zone) entityByRID(rid RuntimeID) *Entity {
	if z == nil || rid == 0 {
		return nil
	}
	for _, room := range z.rooms {
		if room == nil {
			continue
		}
		if room.rid == rid {
			return room
		}
		if e := findInContents(room, rid); e != nil {
			return e
		}
	}
	return nil
}

// findInContents recursively searches container's contents subtree for rid (containers nest:
// an item in a corpse in a room). Returns the matching entity or nil.
func findInContents(container *Entity, rid RuntimeID) *Entity {
	for _, c := range container.contents {
		if c == nil {
			continue
		}
		if c.rid == rid {
			return c
		}
		if len(c.contents) > 0 {
			if e := findInContents(c, rid); e != nil {
				return e
			}
		}
	}
	return nil
}

// parseRef splits a room ProtoRef into the zone it belongs to and the ProtoRef that
// keys it in that zone's room map. The canonical form is "zone:kind:name" (e.g.
// "midgaard:room:temple"); the zone is the leading segment and the room key is the
// whole ref (the room map is keyed by full ProtoRef, so O(1) lookup by ref). A bare ref
// with no colon is a local room with an empty zone. This is the typed successor to the
// old string parseRef and feeds the same (zone, room) routing decision in Zone.move.
//
// Do NOT compare the returned zone against z.id directly — ask ownsZoneRef. A raw `== z.id` is wrong for an
// instanced zone (#72), whose rooms carry their template's authored refs; the lint test in
// identity_lint_test.go fails the build on one.
func parseRef(ref ProtoRef) (zone string, room ProtoRef) {
	s := string(ref)
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], ref
		}
	}
	return "", ref
}

// ownsZoneRef reports whether zoneID — the zone segment parseRef returned for some room ref — names content
// THIS zone hosts. True for a bare local ref (""), for our own id, and for our template, which differ only in
// an instanced zone (#72): an instance's rooms keep their AUTHORED refs, so every ref inside `crypt#7` names
// zone `crypt`. That is what lets all instances share the immutable per-shard protoCache, and it is why a raw
// `zoneID != z.id` reads every exit in an instance as leaving the zone.
//
// This is the ROUTING question — "does this ref stay inside me" — and it is the only one that may widen to the
// template. It is NOT the isolation question. Anything asking "may this actor reach that" must stay strict on
// z.id, or a script in `crypt#7` eventually resolves a handle into `crypt#9`. Two different questions that
// happen to have the same shape; do not collapse them into one helper.
//
// The rule for telling them apart: the ROOM MAP is the isolation boundary, and this zone-segment test is only
// a pre-filter in front of it. `z.rooms` is per-*Zone — two instances of one template hold different maps
// containing different entities, both keyed by the same authored refs — so widening the pre-filter changes
// only WHETHER we consult our own map, never WHICH map. No *Zone holds a pointer into another's rooms. A
// caller that reaches an entity some other way, without that lookup standing behind it, is asking the
// isolation question and must not use this.
//
// Note this only decides the inside-out direction. An exit in `town` naming `crypt:room:entrance` still
// resolves to the template zone, never to an instance — routing a player INTO an instance is a separate
// mechanism (#72's entry binding), not something this predicate can express.
func (z *Zone) ownsZoneRef(zoneID string) bool {
	return zoneID == "" || zoneID == z.id || zoneID == z.template
}

// localRoom resolves a room ref that this zone hosts, or nil if the ref names another zone or an unknown room.
// It is the chokepoint for "is this ref mine, and if so which room": parseRef + ownsZoneRef + the room map, in
// the one order that is correct for an instance.
//
// Callers that need to distinguish "not mine" from "mine but unknown" (move, which routes the first case
// cross-zone and refuses the second) must use parseRef + ownsZoneRef directly; everyone else wants this.
func (z *Zone) localRoom(ref ProtoRef) *Entity {
	zoneID, room := parseRef(ref)
	if !z.ownsZoneRef(zoneID) {
		return nil
	}
	return z.rooms[room]
}
