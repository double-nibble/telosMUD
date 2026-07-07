package world

import (
	"context"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// world_reconcile_test.go covers the zone-SHAPE reconcile (#191 PR 2/3): the diff that detects rooms a
// content edit DELETED and drives removeRoom (the teardown primitives from PR 1/3). The per-ref applier
// (resyncRoom) handles ADD/UPDATE; only DELETION needs the whole-zone diff, because a deleted room's ref
// is absent from the content so PublishPack never names it.

// liveRoomSet snapshots a zone's live room refs into a want-set, excluding the given refs — the shape a
// content edit that DELETED those rooms would present to the reconcile.
func liveRoomSet(z *Zone, exclude ...ProtoRef) map[ProtoRef]bool {
	ex := map[ProtoRef]bool{}
	for _, r := range exclude {
		ex[r] = true
	}
	want := map[ProtoRef]bool{}
	for ref := range z.rooms {
		if !ex[ref] {
			want[ref] = true
		}
	}
	return want
}

// TestReconcileZoneShapeRemovesDeletedRoom proves the reconcile tears down a room the reloaded content no
// longer defines, evacuating its player occupant to the start room.
func TestReconcileZoneShapeRemovesDeletedRoom(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	start := z.rooms[z.startRoom]
	market := z.rooms["midgaard:room:market"]
	if start == nil || market == nil {
		t.Fatal("demo midgaard should have a temple (start) and a market room")
	}

	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, market)
	z.players["Hero"] = s

	// The reloaded content dropped the market; the start room is unchanged.
	z.reconcileZoneShape(liveRoomSet(z, "midgaard:room:market"), z.startRoom)

	if z.rooms["midgaard:room:market"] != nil {
		t.Fatal("deleted room still live after reconcile")
	}
	if z.protos.get("midgaard:room:market") != nil {
		t.Fatal("deleted room prototype still cached after reconcile")
	}
	if s.entity.location != start {
		t.Fatalf("player not evacuated to start; at %v", targetShort(s.entity.location))
	}
	// A room the content still defines is untouched.
	if z.rooms["midgaard:room:smithy"] == nil {
		t.Fatal("reconcile wrongly removed a room the content still defines")
	}
}

// TestReconcileZoneShapeRepointsStartBeforeRemoval proves the reconcile repoints start_room FIRST, which is
// what makes the OLD start room removable (removeRoom refuses the LIVE start room). A player in the old
// start room is evacuated to the NEW one.
func TestReconcileZoneShapeRepointsStartBeforeRemoval(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	oldStart := z.startRoom // temple
	newStartRef := ProtoRef("midgaard:room:market")
	newStart := z.rooms[newStartRef]
	if newStart == nil {
		t.Fatal("demo midgaard should have a market room to repoint start to")
	}

	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, z.rooms[oldStart])
	z.players["Hero"] = s

	// The reloaded content moved start to the market AND deleted the old temple start room.
	z.reconcileZoneShape(liveRoomSet(z, oldStart), newStartRef)

	if z.startRoom != newStartRef {
		t.Fatalf("start room not repointed: %q", z.startRoom)
	}
	if z.rooms[oldStart] != nil {
		t.Fatal("old (now non-start) room was not removed")
	}
	if s.entity.location != newStart {
		t.Fatalf("player not evacuated to the NEW start; at %v", targetShort(s.entity.location))
	}
}

// TestReconcileZoneShapeNoChangeWhenAllPresent proves a reconcile whose want-set matches the live rooms
// removes nothing (the common case: an edit that only touched descriptions/exits, no shape change).
func TestReconcileZoneShapeNoChangeWhenAllPresent(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	before := len(z.rooms)

	z.reconcileZoneShape(liveRoomSet(z), z.startRoom)

	if len(z.rooms) != before {
		t.Fatalf("reconcile with no deletions changed the room set: %d -> %d", before, len(z.rooms))
	}
}

// TestReconcileZoneShapeIgnoresUndefinedStart proves the start-room repoint is guarded to a room the
// reloaded content actually defines — a malformed edit naming an undefined start room leaves z.startRoom
// as-is rather than pointing new logins at a room that does not exist.
func TestReconcileZoneShapeIgnoresUndefinedStart(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	orig := z.startRoom

	z.reconcileZoneShape(liveRoomSet(z), ProtoRef("midgaard:room:nonexistent"))

	if z.startRoom != orig {
		t.Fatalf("start repointed to an undefined room: %q", z.startRoom)
	}
}

// reloadTestPackMultiRoom is reloadTestPack plus a second, DELETABLE room (the cellar) reachable from the
// hall — so a reconcile test can drop the cellar and prove the live-room teardown.
func reloadTestPackMultiRoom() content.Pack {
	p := reloadTestPack()
	p.Zones[0].Rooms[0].Exits = map[string]string{"down": "rt:room:cellar"}
	p.Zones[0].Rooms = append(p.Zones[0].Rooms, content.RoomDTO{
		Ref:   "rt:room:cellar",
		Name:  "The Cellar",
		Long:  "A damp cellar.",
		Exits: map[string]string{"up": "rt:room:hall"},
	})
	return p
}

// TestReloadZoneShapeReconcileEndToEnd is the full-path proof: a player standing in a room that a content
// edit DELETES is evacuated when a `zone` invalidation lands on the bus — the whole chain (bus → reloader
// reloadZoneShape → reloadZoneMsg → reconcileZoneShape → removeRoom → relocate) runs under a LIVE zone loop,
// observed only via the player's output channel (race-free under -race).
func TestReloadZoneShapeReconcileEndToEnd(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPackMultiRoom())
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s := newReloadShard(t, src, bus)
	z := s.Zone()

	if z.rooms["rt:room:cellar"] == nil {
		t.Fatal("precondition: the cellar must boot live")
	}
	// A player in the doomed cellar. Setup completes BEFORE Run starts (goroutine-start happens-before), so
	// the loop never races this write; all later observation is via the out channel.
	p := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(p, "Hero")
	z.players["Hero"] = p
	Move(p.entity, z.rooms["rt:room:cellar"])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	// The edit drops the cellar (back to the single-room pack), then a `zone` invalidation triggers the
	// reconcile on every hosting shard.
	src.SetPack(reloadTestPack())
	if err := bus.Publish(ctx, contentbus.Invalidation{
		Kind: content.KindZone, Ref: "rt", Pack: "reloadtest",
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if drainContains(t, p, "swept somewhere safe") {
			return // the player was evacuated out of the deleted room — end to end
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("player was never evacuated after the zone-shape reconcile deleted their room")
}
