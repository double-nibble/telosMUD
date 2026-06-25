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

// parseRef splits a room ProtoRef into the zone it belongs to and the ProtoRef that
// keys it in that zone's room map. The canonical form is "zone:kind:name" (e.g.
// "midgaard:room:temple"); the zone is the leading segment and the room key is the
// whole ref (the room map is keyed by full ProtoRef, so O(1) lookup by ref). A bare ref
// with no colon is a local room with an empty zone. This is the typed successor to the
// old string parseRef and feeds the same (zone, room) routing decision in Zone.move:
// zone == "" or zone == z.id is a local move; a different zone routes cross-zone.
func parseRef(ref ProtoRef) (zone string, room ProtoRef) {
	s := string(ref)
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], ref
		}
	}
	return "", ref
}
