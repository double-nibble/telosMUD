package world

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
)

// Empty-world boot audit (docs/PHASE4-PLAN.md §7.5 / risk 5): with zero content the engine
// must boot and a login must be REJECTED with a clean message, never panic. These exercise
// the resolveRoom/join/attach/transferIn guards added in slice 4.1.

// TestEmptyShardBootsAndRejectsLoginCleanly builds a shard from EMPTY content (the bare-engine
// path) and logs a fresh player in. The zone has no rooms and no start room, so join must
// reject cleanly: the player gets a clear message and is NOT registered.
func TestEmptyShardBootsAndRejectsLoginCleanly(t *testing.T) {
	empty, _ := content.Load(context.Background(), nil, nil)
	shard := NewShardFromContent(empty, []string{"void"}, "void", "", nil, nil)
	z := shard.Zone()
	if z == nil {
		t.Fatal("empty shard has no home zone")
	}
	if len(z.rooms) != 0 || z.startRoom != "" {
		t.Fatalf("empty zone should have no rooms/start room: rooms=%d start=%q", len(z.rooms), z.startRoom)
	}
	// Bare-engine invariant (Phase 5.1): no content => zero attribute/resource/damage-type defs.
	// Combat ops are simply unavailable; a stat read on any entity returns 0 (no panic).
	if n := z.attrDefs().len(); n != 0 {
		t.Fatalf("empty shard should have 0 attribute defs, got %d", n)
	}
	if n := z.resourceDefs().len(); n != 0 {
		t.Fatalf("empty shard should have 0 resource defs, got %d", n)
	}
	if n := z.damageTypeDefs().len(); n != 0 {
		t.Fatalf("empty shard should have 0 damage-type defs, got %d", n)
	}
	// Phase 5.3: no content => zero ability defs and no ability commands. "fireball" is unavailable.
	if n := z.abilityDefs().len(); n != 0 {
		t.Fatalf("empty shard should have 0 ability defs, got %d", n)
	}
	if z.abilityForVerb("fireball") != nil {
		t.Fatal("empty shard should expose no ability commands (fireball unavailable)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	out := make(chan *playv1.ServerFrame, 16)
	var cz atomic.Pointer[Zone]
	z.claimInboundArrival() // the claim the production resolver takes under s.mu; the handler releases one unconditionally (#413)
	z.post(attachMsg{character: "Lost", out: out, curZone: &cz})

	// We must get a clean text frame, and the zone must keep running (no panic killed it).
	got := nextOutput(t, &session{character: "Lost", out: out})
	if !strings.Contains(got, "no rooms") {
		t.Fatalf("empty-world login message = %q, want a 'no rooms' rejection", got)
	}
}

// TestEmptyZoneJoinDoesNotRegisterPlayer drives join() directly on an empty zone and asserts
// it neither registers the player nor null-derefs (it would in lookRoom without the guard).
func TestEmptyZoneJoinDoesNotRegisterPlayer(t *testing.T) {
	z := newZone("void") // no rooms, no start room
	out := make(chan *playv1.ServerFrame, 16)
	s := &session{character: "Nobody", out: out, epoch: 1}
	z.newPlayerEntity(s, "Nobody")

	z.join(s, "") // must not panic

	if z.players["Nobody"] != nil {
		t.Fatal("empty-world join must not register a placeless player")
	}
	if s.entity.location != nil {
		t.Fatal("rejected player must have no location")
	}
}
