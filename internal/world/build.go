package world

import (
	"github.com/double-nibble/telosmud/internal/content"
)

// build.go is the content loader's world-side half (docs/PHASE4-PLAN.md §3): it takes the
// neutral *content.LoadedContent produced by the content package and turns it into live
// prototypes + zones, REPLACING the hand-authored newDemoZone. It is the new caller of
// protoCache.define — define stays the sole prototype-construction entry point — and of
// Zone.spawn for the reset placements. spawn and the COW model are untouched.
//
// Two stages, both run at shard construction (before any zone goroutine starts), so the
// synchronous work here never races a running zone:
//
//   1. defineContent fills the shared per-shard protoCache from every loaded zone's room,
//      item, and mob prototypes (via the DTO->component mapper, content_map.go).
//   2. buildZone wires one zone: it spawns its room singletons (sharing the immutable
//      prototypes) and runs its reset script to place ephemeral instances on the floor.

// defineContent registers every prototype in lc into the cache. Build-time only: called from
// newShard while the cache is still private to the construction goroutine. Rooms, items, and
// mobs from every loaded zone are defined into the one shared cache, so a cross-zone exit
// target resolves regardless of which zone hosts the destination room.
func defineContent(c *protoCache, lc *content.LoadedContent) {
	if lc == nil {
		return
	}
	for i := range lc.Zones {
		z := &lc.Zones[i]
		for _, r := range z.Rooms {
			c.define(ProtoRef(r.Ref), nil, r.Name, r.Long, roomComponents(r))
		}
		for _, p := range z.Items {
			c.define(ProtoRef(p.Ref), p.Keywords, p.Short, p.Long, protoComponents(p))
		}
		for _, p := range z.Mobs {
			c.define(ProtoRef(p.Ref), p.Keywords, p.Short, p.Long, protoComponents(p))
		}
	}
}

// buildZone constructs zone z (id == zoneRef) from its loaded definition: it spawns each room
// as a singleton entity, records the start room, and runs the reset script. The prototype
// cache must already be filled (defineContent). If the loaded content has no such zone, the
// zone is left EMPTY (no rooms, no start room) — the bare-engine boot: a login to an empty
// zone is rejected cleanly (Zone.join / resolveRoom guards), never a panic.
func (z *Zone) buildZone(lc *content.LoadedContent) {
	zd := lc.Zone(z.id)
	if zd == nil {
		z.log.Debug("zone has no loaded content; booting empty", "zone", z.id)
		return
	}
	for _, r := range zd.Rooms {
		z.spawnRoom(ProtoRef(r.Ref))
	}
	z.startRoom = ProtoRef(zd.StartRoom)
	z.runResets(zd.Resets)
	z.log.Debug("zone built from content", "zone", z.id,
		"rooms", len(zd.Rooms), "start_room", z.startRoom, "resets", len(zd.Resets))
}

// runResets executes a zone's reset script at boot. v1 understands op:"spawn_item": it spawns
// `count` (>=1) ephemeral instances of a prototype into a room's contents — exactly the old
// newDemoZone Move(z.spawn(ref), marketEntity) loop, now data-driven. Unknown ops and unknown
// rooms/prototypes are logged and skipped (content-lint is the real gate); the full reset
// interpreter (mobs, max-counts, repop timer, persistent flag) arrives in slice 4.4.
func (z *Zone) runResets(resets []content.ResetDTO) {
	for _, r := range resets {
		switch r.Op {
		case "spawn_item", "spawn_mob", "":
			room := z.rooms[ProtoRef(r.Room)]
			if room == nil {
				z.log.Warn("reset skipped: unknown room", "op", r.Op, "room", r.Room, "proto", r.Proto)
				continue
			}
			n := r.Count
			if n <= 0 {
				n = 1
			}
			for i := 0; i < n; i++ {
				e := z.spawn(ProtoRef(r.Proto))
				if e == nil {
					z.log.Warn("reset skipped: unknown prototype", "op", r.Op, "proto", r.Proto)
					break
				}
				Move(e, room)
			}
		default:
			z.log.Warn("reset op not understood (slice 4.1)", "op", r.Op)
		}
	}
}

// newDemoZone builds the named demo zone from the EMBEDDED demo pack (content.DemoPack),
// sharing the given per-shard prototype cache. It is the test/bare-run helper that the
// Phase 1-3 tests still call (handoff_fixes_test, container_test indirectly): the hand-
// authored body is gone, so this is now a thin loader call that produces byte-identical
// prototypes. The first call into the shared cache defines the demo content; a second call
// for a sibling zone re-defines the same immutable prototypes (idempotent — same data).
//
// Production does NOT call this: buildShard loads from the configured source (Postgres or the
// embedded pack) and builds every hosted zone via buildZone. This wrapper exists so the unit
// tests construct the demo world without a live database.
func newDemoZone(id string, protos *protoCache) *Zone {
	z := newZone(id)
	z.protos = protos
	lc, err := content.LoadDemoPack()
	if err != nil {
		// The demo pack is embedded and checked in; a parse failure is a build-time bug, not a
		// runtime condition. Panic loudly so a malformed pack can never silently ship.
		panic("world: load embedded demo pack: " + err.Error())
	}
	defineContent(protos, lc)
	z.buildZone(lc)
	return z
}
