package world

import "testing"

// TestAffectTickSurvivesIntraShardMove is the regression for the dropped-tick bug the
// distributed-systems-architect found in slice 5.2: an affect (or resource regen) tick is
// registered on the SOURCE zone, but when a player walks zone->zone IN-PROCESS (the common
// cross-zone path), the entity is re-homed to the destination and nothing re-armed the tick
// there — affects/regen silently froze, and the stale handle blocked any re-arm. The fix is the
// re-arm in Zone.transferIn. This test drives the two zone goroutines' pulses directly so it is
// deterministic, and asserts both halves of the contract: the SOURCE tick must NOT touch the
// departed player's affect, and the DESTINATION tick MUST keep it counting down.
func TestAffectTickSurvivesIntraShardMove(t *testing.T) {
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	A, B := shard.zones["midgaard"], shard.zones["darkwood"]
	if A == nil || B == nil {
		t.Fatal("zones not built")
	}

	// A player in A, carrying a demo affect (poison: a -2 strength, ticking affect). Arming
	// applyAffect registers the per-entity tick on A's pulse.
	s := newTestPlayerEntity(A, "Mover")
	e := s.entity
	A.join(s, "")

	inst := applyAffect(e, "poison", attachOpts{})
	if inst == nil {
		t.Fatal("poison not applied (demo affect def missing?)")
	}
	inst.remaining = 20
	a, _ := Get[*Affected](e)
	if a.tick == nil {
		t.Fatal("tick not armed on source zone A")
	}

	// Walk A -> B intra-shard: transferOut on A, transferIn on B (drain the message B receives).
	var bRoom ProtoRef
	for ref := range B.rooms {
		bRoom = ref
		break
	}
	A.transferOut(s, B, bRoom, "north", e.location)
	B.handle(<-B.inbox)
	if B.players["Mover"] == nil || e.zone != B {
		t.Fatal("player not re-homed to B after transfer")
	}

	// Source A ticks: re-resolves "Mover", finds it absent, cancels WITHOUT touching the moved
	// (now B-owned) entity. The affect must be untouched by A.
	beforeA := inst.remaining
	A.pulses.tick()
	if inst.remaining != beforeA {
		t.Fatalf("source A tick decremented a departed player's affect: %d -> %d", beforeA, inst.remaining)
	}

	// Destination B ticks: the affect must keep counting down (the re-arm worked).
	before := inst.remaining
	for i := 0; i < 5; i++ {
		B.pulses.tick()
	}
	if inst.remaining >= before {
		t.Fatalf("affect stopped ticking on destination after the move: remaining stuck at %d", before)
	}
}
