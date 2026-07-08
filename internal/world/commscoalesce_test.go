package world

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
)

// commscoalesce_test.go pins #77: the mid-session comms republish is COALESCED across a runOps cascade — a
// grant op marks its target dirty and the OUTERMOST runOps flushes ONE republish per DISTINCT target at
// op-list end, instead of one publish per mutation. A bundle of M same-target grants must publish once; an
// AoE grant must publish once per distinct target, regardless of how many ops touched each.

// hearGatedShard builds a shard with one HEAR-gated channel (so republishCommsOnAccessChange actually fires)
// and returns the zone + the paired gate handle to subscribe config subjects on.
func hearGatedShard(t *testing.T) (*Zone, commbus.Bus) {
	t.Helper()
	src := content.NewMemSource()
	src.SetPack(content.Pack{
		Pack: "hg",
		Channels: []content.ChannelDTO{{
			Ref: "confession", Name: "Confession", Words: []string{"confess"},
			DefaultOn:  true,
			HearAccess: &content.ChannelAccessDTO{RequireFlag: "confessor"},
		}},
		Zones: []content.ZoneDTO{{
			Ref: "hg", Name: "Hear Gated", StartRoom: "hg:room:start",
			Rooms: []content.RoomDTO{{Ref: "hg:room:start", Name: "Start", Long: "A room."}},
		}},
	})
	lc, err := content.Load(context.Background(), src, []string{"hg"})
	if err != nil {
		t.Fatal(err)
	}
	wbus, gate := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	sh := NewShardFromContent(lc, []string{"hg"}, "hg", "", nil, nil).WithComms(wbus)
	return sh.Zone(), gate
}

// countConfigs drains every config payload published so far (after a brief settle for async delivery).
func countConfigs(ch <-chan commbus.ConfigPayload) int {
	time.Sleep(75 * time.Millisecond)
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

// TestCommsRepublishCoalescedForSameTargetBundle: a bundle of several grant ops on ONE player publishes the
// player's config exactly ONCE (the coalesced flush), not once per op.
func TestCommsRepublishCoalescedForSameTargetBundle(t *testing.T) {
	z, gate := hearGatedShard(t)
	sinner := newTestPlayerEntity(z, "Sinner")
	cfg := drainConfig(t, gate, "Sinner")

	c := &effectCtx{z: z, actor: sinner.entity, source: sinner.entity, target: sinner.entity, mag: 1, rng: rand.New(rand.NewSource(1))}
	// Five grant ops on the same target, each of which would republish inline pre-#77.
	ops := []effectOp{
		{kind: "set_flag", flag: "confessor"},
		{kind: "clear_flag", flag: "confessor"},
		{kind: "set_flag", flag: "confessor"},
		{kind: "set_flag", flag: "veteran"},
		{kind: "clear_flag", flag: "veteran"},
	}
	runOps(c, ops)

	if n := countConfigs(cfg); n != 1 {
		t.Fatalf("a 5-op same-target grant bundle published the config %d times, want exactly 1 (coalesced #77)", n)
	}
}

// TestCommsRepublishCoalescedAcrossNestedOps: the coalescing spans if/chance recursion — a grant nested
// inside a flow op still flushes ONCE at the OUTERMOST runOps, not per nested sub-list.
func TestCommsRepublishCoalescedAcrossNestedOps(t *testing.T) {
	z, gate := hearGatedShard(t)
	sinner := newTestPlayerEntity(z, "Sinner")
	cfg := drainConfig(t, gate, "Sinner")

	c := &effectCtx{z: z, actor: sinner.entity, source: sinner.entity, target: sinner.entity, mag: 1, rng: rand.New(rand.NewSource(1))}
	// A top-level grant + a chance(p=1) op whose branch also grants: two grants, two runOps frames, one flush.
	ops := []effectOp{
		{kind: "set_flag", flag: "confessor"},
		{kind: "chance", prob: 1, then: []effectOp{{kind: "set_flag", flag: "veteran"}}},
	}
	runOps(c, ops)

	if n := countConfigs(cfg); n != 1 {
		t.Fatalf("a grant nested in a flow op published %d times, want exactly 1 (outermost runOps owns the flush)", n)
	}
}

// TestCommsRepublishAoEOncePerDistinctTarget: an AoE grant op (runOpArea loops over N room targets)
// publishes ONCE per distinct target — coalescing dedups per target, so M area ops still cost N publishes,
// never M×N.
func TestCommsRepublishAoEOncePerDistinctTarget(t *testing.T) {
	z, gate := hearGatedShard(t)
	alice := newTestPlayerEntity(z, "Alice")
	bob := newTestPlayerEntity(z, "Bob")
	Move(alice.entity, z.rooms[z.startRoom]) // place both in the start room so the room AoE catches them
	Move(bob.entity, z.rooms[z.startRoom])
	// A NON-player actor (a mob's aura / a room effect) is the realistic AoE-grant source: a player-actor
	// cross-player flag write is gated by guardCrossPlayerWrite, but a mob→player grant passes the PvE gate.
	aura := makeMobTarget(z, alice.entity, "aura")
	aliceCfg := drainConfig(t, gate, "Alice")
	bobCfg := drainConfig(t, gate, "Bob")

	c := &effectCtx{z: z, actor: aura, source: aura, target: aura, mag: 1, rng: rand.New(rand.NewSource(1))}
	// Two area grant ops over the room: without coalescing that is 2 ops × 2 targets = 4 publishes; with #77
	// it is one publish per distinct target = 2 total (1 each).
	ops := []effectOp{
		{kind: "set_flag", flag: "confessor", area: "room"},
		{kind: "set_flag", flag: "veteran", area: "room"},
	}
	runOps(c, ops)

	if n := countConfigs(aliceCfg); n != 1 {
		t.Fatalf("Alice's config published %d times across 2 area ops, want exactly 1 per distinct target (#77)", n)
	}
	if n := countConfigs(bobCfg); n != 1 {
		t.Fatalf("Bob's config published %d times across 2 area ops, want exactly 1 per distinct target (#77)", n)
	}
}

// TestMarkCommsDirtyImmediateOutsideCascade: a grant marked dirty OUTSIDE any runOps cascade republishes
// immediately (there is no op-list boundary to coalesce to) — the fallback that keeps a direct caller safe.
func TestMarkCommsDirtyImmediateOutsideCascade(t *testing.T) {
	z, gate := hearGatedShard(t)
	sinner := newTestPlayerEntity(z, "Sinner")
	cfg := drainConfig(t, gate, "Sinner")

	c := &effectCtx{z: z, actor: sinner.entity, source: sinner.entity, target: sinner.entity, mag: 1}
	setFlag(sinner.entity, "confessor", true)
	c.markCommsDirty(sinner.entity) // commsCoalescing is false → immediate republish

	if n := countConfigs(cfg); n != 1 {
		t.Fatalf("markCommsDirty outside a cascade published %d times, want exactly 1 (immediate)", n)
	}
}
