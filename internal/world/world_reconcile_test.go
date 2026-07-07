package world

import (
	"context"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// world_reconcile_test.go covers the zone-SHAPE reconcile (#191): the single authoritative path that
// converges a zone's live room graph to the reloaded content's desired state — spawn ADDs, resync
// UPDATEs, tear down DELETIONs (removeRoom, PR 1/3), apply a start_room change — with a monotonic version
// guard against racing reloads. The desired state rides the KindZone invalidation on the wire (no source
// re-read); these tests drive reconcileZone directly with a reconcileZoneMsg, and one end-to-end proof
// runs the whole bus→zone chain under a live loop.

// liveRoomRefs snapshots a zone's live room refs, excluding the given refs — the desired-room list a
// content edit that DELETED those rooms would carry on its KindZone invalidation.
func liveRoomRefs(z *Zone, exclude ...ProtoRef) []string {
	ex := map[ProtoRef]bool{}
	for _, r := range exclude {
		ex[r] = true
	}
	var out []string
	for ref := range z.rooms {
		if !ex[ref] {
			out = append(out, string(ref))
		}
	}
	return out
}

// TestReconcileZoneRemovesDeletedRoom proves the reconcile tears down a room the desired set no longer
// names, evacuating its player occupant to the start room, and leaves a still-desired room untouched.
func TestReconcileZoneRemovesDeletedRoom(t *testing.T) {
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
	z.reconcileZone(reconcileZoneMsg{
		zoneRef:   z.id,
		version:   1,
		rooms:     liveRoomRefs(z, "midgaard:room:market"),
		startRoom: z.startRoom,
	})

	if z.rooms["midgaard:room:market"] != nil {
		t.Fatal("deleted room still live after reconcile")
	}
	if z.protos.get("midgaard:room:market") != nil {
		t.Fatal("deleted room prototype still cached after reconcile")
	}
	if s.entity.location != start {
		t.Fatalf("player not evacuated to start; at %v", targetShort(s.entity.location))
	}
	if z.rooms["midgaard:room:smithy"] == nil {
		t.Fatal("reconcile wrongly removed a room the content still defines")
	}
}

// TestReconcileZoneRepointsStartBeforeRemoval proves the reconcile applies start_room FIRST, which is what
// makes the OLD start room removable (removeRoom refuses the LIVE start room). A player in the old start
// room is evacuated to the NEW one.
func TestReconcileZoneRepointsStartBeforeRemoval(t *testing.T) {
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
	z.reconcileZone(reconcileZoneMsg{
		zoneRef:   z.id,
		version:   1,
		rooms:     liveRoomRefs(z, oldStart),
		startRoom: newStartRef,
	})

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

// TestReconcileZoneAddsNewRoom proves the reconcile OWNS the ADD path: a room present in the desired set
// but not yet live is spawned (off the already-swapped prototype cache). It replaces the per-ref applier's
// resync as the single authoritative zone-shape path.
func TestReconcileZoneAddsNewRoom(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	const annex = "midgaard:room:annex"
	if z.rooms[annex] != nil {
		t.Fatal("precondition: the annex must not exist before the reconcile")
	}
	// Swap a new-room prototype into the shared cache (what the per-ref KindRoom invalidation does before
	// the trailing KindZone reconcile lands).
	z.protos.reload(ProtoRef(annex), buildPrototype(content.Definition{
		Kind: content.KindRoom, Ref: annex, Found: true,
		Room: content.RoomDTO{Ref: annex, Name: "The Annex", Long: "A freshly built annex."},
	}))

	z.reconcileZone(reconcileZoneMsg{
		zoneRef: z.id,
		version: 1,
		rooms:   append(liveRoomRefs(z), annex), // every live room PLUS the new annex
	})

	if z.rooms[annex] == nil {
		t.Fatal("reconcile did not spawn the newly-desired room")
	}
	if got := z.rooms[annex].Long(); got != "A freshly built annex." {
		t.Fatalf("added room long = %q", got)
	}
}

// TestReconcileZoneNoChangeWhenAllPresent proves a reconcile whose desired set matches the live rooms
// removes nothing (the common case: an edit that only touched descriptions/exits, no shape change).
func TestReconcileZoneNoChangeWhenAllPresent(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	before := len(z.rooms)

	z.reconcileZone(reconcileZoneMsg{zoneRef: z.id, version: 1, rooms: liveRoomRefs(z), startRoom: z.startRoom})

	if len(z.rooms) != before {
		t.Fatalf("reconcile with no shape change altered the room set: %d -> %d", before, len(z.rooms))
	}
}

// TestReconcileZoneIgnoresUndefinedStart proves the start-room repoint is guarded to a room the desired set
// actually names — a malformed edit naming an undefined start room leaves z.startRoom as-is rather than
// pointing new logins at a room that does not exist.
func TestReconcileZoneIgnoresUndefinedStart(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	orig := z.startRoom

	z.reconcileZone(reconcileZoneMsg{
		zoneRef:   z.id,
		version:   1,
		rooms:     liveRoomRefs(z),
		startRoom: "midgaard:room:nonexistent",
	})

	if z.startRoom != orig {
		t.Fatalf("start repointed to an undefined room: %q", z.startRoom)
	}
}

// TestReconcileZoneVersionGuard proves a STALE reconcile (version ≤ the newest applied) is dropped, so a
// racing reload cannot reorder an older snapshot ahead of a newer one. A newer version applies.
func TestReconcileZoneVersionGuard(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	// v10 deletes the market.
	z.reconcileZone(reconcileZoneMsg{zoneRef: z.id, version: 10, rooms: liveRoomRefs(z, "midgaard:room:market"), startRoom: z.startRoom})
	if z.rooms["midgaard:room:market"] != nil {
		t.Fatal("v10 reconcile did not delete the market")
	}
	if z.lastReconciledPackVer != 10 {
		t.Fatalf("version cursor = %d, want 10", z.lastReconciledPackVer)
	}

	// A STALE v5 reconcile that re-includes the market must be DROPPED (would resurrect it).
	z.reconcileZone(reconcileZoneMsg{zoneRef: z.id, version: 5, rooms: liveRoomRefs(z, "midgaard:room:smithy"), startRoom: z.startRoom})
	if z.rooms["midgaard:room:market"] != nil {
		t.Fatal("a stale (v5) reconcile was applied — it resurrected the market and/or removed the smithy")
	}
	if z.rooms["midgaard:room:smithy"] == nil {
		t.Fatal("a stale (v5) reconcile removed the smithy (should have been dropped whole)")
	}

	// A NEWER v11 reconcile that also deletes the smithy IS applied.
	z.reconcileZone(reconcileZoneMsg{zoneRef: z.id, version: 11, rooms: liveRoomRefs(z, "midgaard:room:market", "midgaard:room:smithy"), startRoom: z.startRoom})
	if z.rooms["midgaard:room:smithy"] != nil {
		t.Fatal("a newer (v11) reconcile was not applied")
	}
}

// TestReconcileZoneEmptyPayloadSkips proves the defensive guard: a KindZone reconcile carrying NO desired
// rooms is a no-op (it does NOT tear the live zone down to its start room), and it does not advance the
// version cursor — so a malformed/degenerate empty payload can neither wipe the zone nor block a later
// legitimate reload.
func TestReconcileZoneEmptyPayloadSkips(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	before := len(z.rooms)
	if before == 0 {
		t.Fatal("precondition: demo midgaard should have rooms")
	}

	z.reconcileZone(reconcileZoneMsg{zoneRef: z.id, version: 7, rooms: nil, startRoom: z.startRoom})

	if len(z.rooms) != before {
		t.Fatalf("empty-payload reconcile tore down rooms: %d -> %d", before, len(z.rooms))
	}
	if z.lastReconciledPackVer != 0 {
		t.Fatalf("empty-payload reconcile advanced the version cursor to %d (must stay 0)", z.lastReconciledPackVer)
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

// TestReconcileZoneEndToEnd is the full-path proof: a player standing in a room that a content edit DELETES
// is evacuated when a KindZone invalidation lands on the bus — the whole chain (bus → reloader reconcileZone
// → reconcileZoneMsg → z.reconcileZone → removeRoom → relocate) runs under a LIVE zone loop, observed only
// via the player's output channel (race-free under -race). The desired room set rides the invalidation, so
// no source re-read is involved.
func TestReconcileZoneEndToEnd(t *testing.T) {
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

	// The edit drops the cellar: the KindZone invalidation carries the surviving room set (hall only). This
	// is exactly what PublishPack emits for the reduced pack.
	if err := bus.Publish(ctx, contentbus.Invalidation{
		Kind: content.KindZone, Ref: "rt", Pack: "reloadtest",
		Version: uint64(time.Now().UnixNano()), Rooms: []string{"rt:room:hall"}, StartRoom: "rt:room:hall",
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
