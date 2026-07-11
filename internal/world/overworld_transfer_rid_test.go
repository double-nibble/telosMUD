package world

import (
	"fmt"
	"testing"
)

// TestIntraShardTransferReassignsLocalRIDs is the regression for the rid-collision bug the overworld
// minimap surfaced (#361): a player walking intra-shard grove(darkwood) -> overworld via the `exit`
// exit kept its SOURCE-zone rid, which collides with a destination-zone room (both ridAllocators are
// independent + 1-based). entityByRID then resolved the player's Lua handle to whichever colliding
// entity Go's randomized map iteration hit first, so the `room` display template's self:room()/
// self:toggle() reads intermittently failed and the map fell back to the built-in render. The fix
// re-homes the whole carried subtree into the destination zone's rid space (Zone.rehomeSubtree).
func TestIntraShardTransferReassignsLocalRIDs(t *testing.T) {
	shard := NewMultiShard([]string{"darkwood", "overworld"}, "darkwood", "", nil, nil)
	D, O := shard.zones["darkwood"], shard.zones["overworld"]
	s := newTestPlayerEntity(D, "Kas")
	e := s.entity
	D.join(s, "")
	commsOf(s).toggleOverride["overworld"] = true
	Move(e, D.rooms["darkwood:room:grove"])

	D.transferOut(s, O, "overworld:room:c2_r19", "exit", e.location)
	O.handle(<-O.inbox)

	// (1) The rid must be UNIQUE within the destination zone (no collision with a room/mob/item).
	if got := O.entityByRID(e.rid); got != e {
		t.Fatalf("entityByRID(%d) resolved %q (isPlayer=%v), not the transferred player — rid collision",
			e.rid, protoOfT(got), got != nil && Has[*PlayerControlled](got))
	}
	dupes := 0
	for _, room := range O.rooms {
		if room.rid == e.rid {
			dupes++
		}
		for _, c := range room.contents {
			if c.rid == e.rid {
				dupes++
			}
		}
	}
	if dupes != 1 {
		t.Fatalf("player rid %d appears on %d entities in the destination zone, want exactly 1", e.rid, dupes)
	}

	// (2) The overworld map must render on the arrival room AND deterministically as the player walks —
	// render each column-2 room many times to defeat any residual map-iteration nondeterminism.
	for row := 19; row >= 0; row-- {
		ref := ProtoRef(fmt.Sprintf("overworld:room:c2_r%d", row))
		Move(e, O.rooms[ref])
		for i := 0; i < 8; i++ {
			if _, ok := O.renderDisplaySheet("room", e); !ok {
				t.Fatalf("overworld map fell back at %s (iteration %d) after intra-shard transfer", ref, i)
			}
		}
	}
}

func protoOfT(e *Entity) ProtoRef {
	if e == nil {
		return "<nil>"
	}
	return e.proto
}
