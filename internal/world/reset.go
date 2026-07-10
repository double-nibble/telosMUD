package world

import (
	"context"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
)

// reset.go is the zone-reset / repop interpreter (docs/PERSISTENCE.md §5, slice 4.4). A zone's
// reset script is a list of data-driven ops (content.ResetDTO) loaded into the zone at build
// time. The SAME interpreter runs the script at two moments:
//
//   - at zone BOOT (buildZone -> runResets), filling the world with its starting content;
//   - on the repop TIMER (the per-zone pulse cadence, reset_secs), topping the world back up
//     after players (and decay/combat, later) have removed instances.
//
// Both run ON THE ZONE GOROUTINE (boot before any goroutine starts; repop via the pulse
// scheduler, which fires inside Zone.Run) — so every spawn + Move is single-writer, exactly
// like a command handler. The interpreter does NO I/O: resets come from the already-loaded
// content (the embedded pack works with no Postgres), so a storeless zone repops identically.
// The one exception, the persistent-flag gate, is the only path that may touch the store, and
// it does so OFF the goroutine (see resetPersistent) and posts the result back.
//
// # Counting / top-up semantic (the no-leak / no-dup contract)
//
// Each op declares a `max` (its ceiling). On every reset the interpreter COUNTS the live
// instances the op owns and spawns ONLY the difference up to max — so:
//
//   - repop NEVER exceeds max (a full room is a no-op: live == max, difference 0);
//   - repop NEVER leaks (it tops up to max, not by max — running it twice is idempotent);
//   - repop is deterministic and stateless (no per-spawn bookkeeping survives across ticks).
//
// "Owns" is defined ROOM-SCOPED: the count is the number of live instances of `proto` in the
// op's target room (or its into-container). We chose room-scoped over the classic Diku
// GLOBAL-count (count every instance of the proto anywhere in the zone, including in player
// inventories) deliberately:
//
//   - Room-scoped is the simplest correct v1: one map walk over the target room's contents, no
//     zone-wide entity registry, no "spawned-by" tag threaded onto every instance.
//   - The PLAYER-TOOK-IT case: a torch a player picks up and carries OUT of the room no longer
//     counts toward max, so the next repop spawns a replacement — the floor refills to max. This
//     is the intuitive "the world restocks what was taken" behavior. The classic Diku global
//     count instead would NOT restock (the carried torch still counts), which produces the
//     well-known hoarding quirk: a player who pockets every torch suppresses the repop for
//     everyone until they log out or drop them. We accept room-scoped's "a hoarder gets the
//     room restocked behind them" over global's "a hoarder starves the room" — restock-on-take
//     is friendlier and far simpler, and the duplication it implies is bounded by max per room.
//
// Counting by (proto, room) rather than by a "spawned-by-this-reset" marker also means a
// builder-placed and a reset-placed torch are interchangeable for top-up purposes, which is the
// behavior content authors expect (max is "how many torches should be on this floor", full stop).

// runResets executes a zone's full reset script ONCE — the entry point shared by boot (buildZone)
// and the repop pulse (repop). It walks the ops in script order, applying each. Boot and repop
// call it identically; the only difference is timing (boot fills an empty zone, repop tops up a
// populated one), and the top-up math makes both correct from whatever the current population is.
func (z *Zone) runResets(resets []content.ResetDTO) {
	for i := range resets {
		z.applyReset(&resets[i])
	}
}

// applyReset interprets one reset op. It is the single place a reset spawns: it resolves the
// target room (and optional into-container), computes how many instances are MISSING relative to
// the op's ceiling, and spawns exactly that many — never more. A persistent op routes to the
// durable load path instead of spawning ephemerally (resetPersistent). Unknown ops / unknown
// rooms / unknown prototypes are logged and skipped (content-lint is the real gate).
func (z *Zone) applyReset(r *content.ResetDTO) {
	switch r.Op {
	case "spawn_item", "spawn_mob", "":
		// supported: a prototype spawn into a room (or a container in that room).
	default:
		z.log.Warn("reset op not understood", "op", r.Op)
		return
	}

	room := z.rooms[ProtoRef(r.Room)]
	if room == nil {
		z.log.Warn("reset skipped: unknown room", "op", r.Op, "room", r.Room, "proto", r.Proto)
		return
	}

	// Resolve the placement target: the room floor, a container present in the room, or a MOB present in
	// the room (so a goblin can carry loot — its inventory becomes its corpse's loot, Phase 6.3b). The
	// into-target is whichever live instance of `into` is in the room: a container if it has one, else a
	// living mob (its contents are its inventory).
	target := room
	if r.Into != "" {
		c := intoTargetInRoom(room, ProtoRef(r.Into))
		if c == nil {
			z.log.Warn("reset skipped: into-target not present in room",
				"op", r.Op, "room", r.Room, "into", r.Into, "proto", r.Proto)
			return
		}
		target = c
	}

	// Persistent gate (docs/PERSISTENCE.md §4): a flagged op's objects are durable, NOT
	// ephemeral. They must not be re-spawned on every repop — they load from object_instances
	// exactly once. Route to the durable path, which is a no-op without a store (degrades
	// gracefully) and does its I/O off the zone goroutine.
	if r.Persistent {
		z.resetPersistent(r, target)
		return
	}

	// The ceiling: max if set, else the boot `count` (>=1). max wins so a repop tops up to a
	// stable number regardless of the boot seed; count alone (max==0) is a fixed boot placement.
	ceiling := r.Max
	if ceiling <= 0 {
		ceiling = r.Count
		if ceiling <= 0 {
			ceiling = 1
		}
	}

	// Top-up: count the live instances this op owns, spawn only the difference. A ROAM spawn (#202) counts
	// ZONE-WIDE — a wandering mob leaves its spawn room, so a room-scoped count would see the room empty and
	// leak a replacement every repop; counting the whole zone means one roamer anywhere satisfies the reset.
	live := countProto(target, ProtoRef(r.Proto))
	if r.Roam {
		live = countProtoInZone(z, ProtoRef(r.Proto))
	}
	need := ceiling - live
	if need <= 0 {
		z.log.Debug("repop no-op (at or above max)", "op", r.Op, "proto", r.Proto,
			"room", r.Room, "into", r.Into, "live", live, "max", ceiling)
		return
	}
	spawned := 0
	for i := 0; i < need; i++ {
		e := z.spawn(ProtoRef(r.Proto))
		if e == nil {
			z.log.Warn("reset skipped: unknown prototype", "op", r.Op, "proto", r.Proto)
			break
		}
		Move(e, target)
		z.fireSpawn(e) // #202: prime a scripted entity + fire on("spawn") now it is placed (arms a wanderer's loop)
		spawned++
	}
	z.log.Debug("repop", "zone", z.id, "op", r.Op, "proto", r.Proto, "room", r.Room,
		"into", r.Into, "live", live, "max", ceiling, "spawned", spawned, "skipped", need-spawned)
}

// countProto returns how many live instances of proto sit directly in target's contents. This is
// the room-scoped (or container-scoped) top-up count: an instance a player carried OUT of target
// is no longer here, so it does not count and the next repop replaces it. O(room population),
// trivial at MUD counts; runs on the zone goroutine so the contents read is race-free.
func countProto(target *Entity, proto ProtoRef) int {
	n := 0
	for _, e := range target.contents {
		if e.proto == proto {
			n++
		}
	}
	return n
}

// countProtoInZone counts live instances of proto across EVERY room in the zone (a roam reset's zone-wide
// population, #202). A roamer sits in whatever room it has wandered to, so only a whole-zone scan sees it; a
// room-scoped count would miss it and leak a replacement each repop. Counts room-floor instances (a roaming
// mob stands on a room floor, never inside a container/another mob). O(zone population) on the zone goroutine.
func countProtoInZone(z *Zone, proto ProtoRef) int {
	n := 0
	for _, room := range z.rooms {
		n += countProto(room, proto)
	}
	return n
}

// containerInRoom returns the first live instance of the container prototype `proto` sitting on
// room's floor that actually carries a *Container component (so items can be placed inside it), or
// nil. Used by the into-container reset op (a chest of loot). The container must already have been
// placed by an earlier op in the same script (script order matters: spawn the chest, then fill it).
func containerInRoom(room *Entity, proto ProtoRef) *Entity {
	for _, e := range room.contents {
		if e.proto == proto {
			if _, ok := Get[*Container](e); ok {
				return e
			}
		}
	}
	return nil
}

// intoTargetInRoom resolves a reset `into` target in room: a CONTAINER instance of `proto` (a chest),
// or — if none — a LIVING instance of `proto` (a mob), whose contents are its inventory. This is what
// lets a reset arm a mob with carried loot (`spawn_item ... into: <mob_proto>`): the item lands in the
// mob's inventory and, when the mob dies, flows into its corpse (death.go). The mob must already have
// been spawned by an earlier op in the same script (spawn the goblin, then arm it). Returns nil when
// neither a container nor a living instance of `proto` is present.
func intoTargetInRoom(room *Entity, proto ProtoRef) *Entity {
	if c := containerInRoom(room, proto); c != nil {
		return c
	}
	for _, e := range room.contents {
		if e.proto == proto && e.living != nil {
			return e
		}
	}
	return nil
}

// --- Persistent-flag gate (docs/PERSISTENCE.md §4) --------------------------------------------

// PersistentObject is one durable world-object row (object_instances): the prototype to spawn and
// its COW delta. It mirrors the durability of a carried item (character.go ItemJSON) but for an
// object that exists independent of any logged-in character — housing contents, persistent rooms.
// Phase 4 round-trips the proto ref + nesting; the typed delta (and saving deltas BACK) is a later
// concern, exactly as ItemJSON.Delta is deferred for carried items.
type PersistentObject struct {
	ProtoRef string             // the flyweight prototype to spawn
	Contents []PersistentObject // nested container contents
}

// ObjectLoader is the OPTIONAL durable world-object source for persistent reset ops (the read side
// of object_instances). It is the world-object twin of CharacterStore: load by location, off the
// zone goroutine. nil today (the demo flags no persistent op and nothing wires a loader), so the
// persistent gate degrades to a logged no-op — the bare/storeless boot is unaffected. Saving
// deltas back (the write side) is a later concern, kept out of v1 per the slice scope.
type ObjectLoader interface {
	// LoadObjects returns the durable instances at (locationKind, locationRef) — e.g.
	// ("room", "midgaard:room:vault"). Off the zone goroutine.
	LoadObjects(ctx context.Context, locationKind, locationRef string) ([]PersistentObject, error)
}

// resetPersistent is the persistent-flag gate (docs/PERSISTENCE.md §4): a flagged op's objects are
// world-durable, so they must NOT be ephemerally re-spawned on each repop — they load from
// object_instances exactly ONCE. The gate guarantees that with a per-(zone,op) "already loaded"
// guard: the FIRST reset (boot) attempts the durable load; every subsequent repop tick is a no-op
// for this op, so a persistent object is never duplicated by the timer.
//
// The load itself is blocking I/O, so it runs OFF the zone goroutine (a spawned goroutine, like
// the saver/login reads) and posts the result back as a loadObjectsMsg the zone applies on its own
// goroutine. With no loader wired (z.objects == nil) the gate is a logged no-op — graceful
// degradation for the storeless/bare boot. target is the resolved room/container the objects belong
// in (captured now so the post-back doesn't re-resolve).
func (z *Zone) resetPersistent(r *content.ResetDTO, target *Entity) {
	key := persistentKey(r)
	if z.persistentDone[key] {
		// Already loaded once (or attempted): a repop tick must never re-spawn a durable object.
		return
	}
	z.persistentDone[key] = true // mark BEFORE the async load so a repop during the load doesn't re-fire

	if z.objects == nil {
		z.log.Debug("persistent reset op skipped: no object loader (ephemeral boot)",
			"op", r.Op, "proto", r.Proto, "room", r.Room, "into", r.Into)
		return
	}
	// Resolve the durable location for the row lookup: a container's own ref keys its contents,
	// otherwise the room floor.
	locKind, locRef := "room", r.Room
	if r.Into != "" {
		locKind, locRef = "container", r.Into
	}
	loader := z.objects
	z.log.Debug("loading persistent objects", "op", r.Op, "location_kind", locKind, "location_ref", locRef)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), saveIOTimeout)
		defer cancel()
		objs, err := loader.LoadObjects(ctx, locKind, locRef)
		if err != nil {
			z.log.Debug("persistent object load failed (non-fatal)", "location_ref", locRef, "err", err)
			return
		}
		if len(objs) == 0 {
			return // nothing durable stored there yet
		}
		z.post(loadObjectsMsg{target: target, objects: objs})
	}()
}

// rehydrateObjects spawns durable world-objects into their target ON the zone goroutine (single-
// writer), the world-object twin of loadCharacter's inventory rehydrate. Posted back by
// resetPersistent's off-goroutine load. The spawn is the same flyweight COW instance the world
// always builds; persistence only chooses WHAT to spawn and WHERE. The target may have been torn
// down between the load and this message (zone rebuild) — guard the nil.
func (z *Zone) rehydrateObjects(m loadObjectsMsg) {
	if m.target == nil {
		return
	}
	for _, o := range m.objects {
		z.spawnPersistent(m.target, o)
	}
	z.log.Debug("persistent objects rehydrated", "zone", z.id, "count", len(m.objects))
}

// spawnPersistent spawns one durable object from its proto ref into parent's contents, recursing
// into container contents. Mirrors loadItem (character.go) for the world-object case. Skips an
// unknown prototype (content stripped/renamed) with a warning rather than crashing.
func (z *Zone) spawnPersistent(parent *Entity, o PersistentObject) {
	e := z.spawn(ProtoRef(o.ProtoRef))
	if e == nil {
		z.log.Warn("persistent object: unknown prototype, skipped", "proto", o.ProtoRef)
		return
	}
	Move(e, parent)
	// #304: prime the scripted entity and fire on("spawn") the instant it is placed, exactly as the ephemeral
	// repop path does (applyReset). Without this a durable scripted MOB — one loaded from object_instances
	// rather than re-spawned each repop — would never be primed and never receive on("spawn"), so a
	// wander/behavior loop that arms itself there would stay inert. A no-op for a non-scripted object
	// (fireSpawn returns early when the entity has no script), which is the common case, so existing content
	// is unaffected.
	//
	// Fired BEFORE the contents recursion, to keep the two paths' contract identical: on the ephemeral path a
	// mob's inventory is armed by a SEPARATE LATER reset op, so on("spawn") fires with an empty inventory (see
	// the ORDERING note on fireSpawn). Doing the same here means (a) a content author has ONE rule across both
	// paths — "spawn fires when placed, don't assume inventory yet" — and (b) every scripted child spawns
	// under a parent whose own spawn has already fired, so a child's handler never observes a half-constructed
	// parent via self:room().
	z.fireSpawn(e)
	for _, child := range o.Contents {
		z.spawnPersistent(e, child)
	}
}

// persistentKey is the stable identity of a persistent reset op for the once-only load guard. The
// DTO carries no id, so we key on the fields that define WHICH durable location it loads — op,
// proto, room, into — which is exactly what makes two ticks of the same op coalesce to one load.
func persistentKey(r *content.ResetDTO) string {
	return r.Op + "|" + r.Proto + "|" + r.Room + "|" + r.Into
}

// --- Repop cadence: reset_secs -> pulse (docs/PERSISTENCE.md §5-6) ----------------------------

// pulsesPerSecond converts the zone-reset cadence (authored in SECONDS, reset_secs) into pulse
// units (pulse.go's heartbeat). reset_secs*pulsesPerSecond is the stride the repop callback fires
// on. Derived from pulseInterval so it tracks the one timing knob (4 at the 250ms heartbeat). A
// package var so a test can shrink it to drive the cadence deterministically via the scheduler.
var pulsesPerSecond uint64 = uint64(time.Second / pulseInterval)

// startRepop registers the per-zone repop pulse callback driven by reset_secs, the FIRST time the
// zone is built (idempotent: a no-op once repopPulse is set, and a no-op when resetSecs==0 — no
// timed reset). The callback fires ON the zone goroutine (pulse.go), so it has single-writer
// access; it re-runs the WHOLE reset script via runResets, which tops every op up to its max
// (idempotent on a full zone). The script is captured by value at build time — no I/O on the tick.
func (z *Zone) startRepop(resets []content.ResetDTO, resetSecs int) {
	if z.repopPulse != nil || resetSecs <= 0 || len(resets) == 0 {
		return
	}
	stride := uint64(resetSecs) * pulsesPerSecond
	if stride == 0 {
		stride = 1
	}
	z.repopPulse = z.pulses.every(stride, func(pulse uint64) bool {
		z.log.Debug("repop tick", "zone", z.id, "pulse", pulse, "ops", len(resets))
		z.runResets(resets)
		return true // keep repopping for the life of the zone
	})
	z.log.Debug("repop cadence started", "zone", z.id, "reset_secs", resetSecs, "stride_pulses", stride)
}
