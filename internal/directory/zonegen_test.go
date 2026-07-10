package directory

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zonegen_test.go — #315. The zone lease carries a monotonic GENERATION that AdoptZone's signature binds. The
// exact bump rule is the whole contract, and it is easy to get subtly wrong in either direction:
//
//   - bump on a RENEWAL and every in-flight AdoptZone goes stale before its destination can verify it (drains
//     never complete);
//   - fail to bump on a HANDOVER and a captured AdoptZone stays valid forever (the replay #315 exists to kill).
//
// These tests pin the rule against a real Redis (miniredis) rather than a hand-rolled fake, because the rule
// lives in Lua.
//
// The generation is SEEDED from the Redis clock on first claim, so its absolute value is meaningless — these
// tests assert deltas. That seeding is itself a security property, pinned by
// TestZoneLeaseGenerationNeverRetreadsAValueAfterAReset.

// TestZoneLeaseGenerationBumpsOnlyOnOwnershipChange walks a zone through its whole ownership life and asserts
// the generation at each step.
func TestZoneLeaseGenerationBumpsOnlyOnOwnershipChange(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()

	// A zone nobody has ever claimed: no owner, generation 0. verifyAdoptZone treats 0 as "no handover exists
	// to authorize" and refuses, so an unclaimed zone can never be adopted.
	owner, gen, err := r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Empty(t, owner, "a never-claimed zone has no owner")
	assert.Zero(t, gen, "a never-claimed zone is at generation 0")

	// First claim: ownership changed (nobody → shard-a).
	ok, err := r.ClaimZone(ctx, "midgaard", "shard-a", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	owner, gen, err = r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Equal(t, "shard-a", owner)
	require.NotZero(t, gen, "a claimed zone must have a non-zero generation")
	claimed := gen

	// Renewals: the SAME owner heartbeating its lease, which is what a healthy shard does every few seconds.
	// The generation must not move, or every in-flight AdoptZone would be invalidated by the source's own
	// heartbeat.
	for i := 0; i < 3; i++ {
		ok, err = r.ClaimZone(ctx, "midgaard", "shard-a", time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
	}
	_, gen, err = r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Equal(t, claimed, gen, "a renewal by the current owner must NOT bump the generation")

	// A refused claim (a different shard against a LIVE lease) changes nothing — not the owner, and crucially
	// not the generation. Otherwise any peer could invalidate a drain in flight just by probing.
	ok, err = r.ClaimZone(ctx, "midgaard", "shard-c", time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "a live lease must not be stealable")
	owner, gen, err = r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Equal(t, "shard-a", owner)
	assert.Equal(t, claimed, gen, "a REFUSED claim must not bump the generation")

	// The handover flip: this is the step that consumes an AdoptZone signed at generation 1.
	ok, err = r.HandoverZone(ctx, "midgaard", "shard-a", "shard-b", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	owner, gen, err = r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Equal(t, "shard-b", owner)
	assert.Equal(t, claimed+1, gen, "the handover flip must bump the generation")
	flipped := gen

	// A refused handover (from is no longer the owner) must not bump either — the source retrying a flip that
	// already landed cannot be allowed to burn a generation.
	ok, err = r.HandoverZone(ctx, "midgaard", "shard-a", "shard-b", time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
	_, gen, err = r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Equal(t, flipped, gen, "a REFUSED handover must not bump the generation")

	// A SELF-handover is refused at the Lua layer, not merely by the Go caller's guard. It is not an ownership
	// change, so bumping for it would invalidate every in-flight AdoptZone for the zone while flipping nothing
	// — and it would re-set the TTL of a lease its owner has stopped renewing.
	ok, err = r.HandoverZone(ctx, "midgaard", "shard-b", "shard-b", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "a self-handover must be refused by the script itself")
	_, gen, err = r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Equal(t, flipped, gen, "a self-handover must not bump the generation")

	// B renews as the new owner: unchanged.
	ok, err = r.ClaimZone(ctx, "midgaard", "shard-b", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	_, gen, err = r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Equal(t, flipped, gen)
}

// TestZoneLeaseGenerationSurvivesReleaseAndLapse is the anti-resurrection property. `gen` is the ONLY thing
// standing between a captured AdoptZone and a forced host, and an Ed25519 signature never expires. So the
// counter must survive every way a zone can stop being owned: a clean release, and a lease that simply lapses.
// If either reset it to 0, a request captured at generation 1 would go live again the next time the zone was
// claimed.
func TestZoneLeaseGenerationSurvivesReleaseAndLapse(t *testing.T) {
	r, mr := newTestRedisWithClock(t)
	ctx := context.Background()

	ok, err := r.ClaimZone(ctx, "darkwood", "shard-a", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	_, claimed, err := r.ZoneLease(ctx, "darkwood")
	require.NoError(t, err)

	// Clean release (graceful shutdown). The zone reads as unowned, but the generation stands.
	require.NoError(t, r.ReleaseZone(ctx, "darkwood", "shard-a"))
	owner, gen, err := r.ZoneLease(ctx, "darkwood")
	require.NoError(t, err)
	assert.Empty(t, owner, "a released zone has no live owner")
	assert.Equal(t, claimed, gen, "a clean release must NOT reset the generation")

	// A peer reclaims it: ownership changed, so the generation moves. A request captured at the old one is dead.
	ok, err = r.ClaimZone(ctx, "darkwood", "shard-b", 200*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)
	_, gen, err = r.ZoneLease(ctx, "darkwood")
	require.NoError(t, err)
	assert.Equal(t, claimed+1, gen)

	// Now let shard-b's lease LAPSE (a crash: nobody renews). Advance the SERVER clock, which is the clock the
	// Lua reads. The owner reads empty, the generation stands — and the hash itself must still exist, i.e. the
	// key carries no TTL that could take `gen` with it.
	mr.SetTime(time.Now().Add(time.Hour))
	assert.True(t, mr.Exists(r.zoneKey("darkwood")), "the zone hash must not be TTL'd away — it holds `gen`")
	owner, gen, err = r.ZoneLease(ctx, "darkwood")
	require.NoError(t, err)
	assert.Empty(t, owner, "a lapsed lease reads as unowned")
	assert.Equal(t, claimed+1, gen, "a lapsed lease must NOT reset the generation")

	// The reclaim after the crash is an ownership change.
	ok, err = r.ClaimZone(ctx, "darkwood", "shard-c", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	owner, gen, err = r.ZoneLease(ctx, "darkwood")
	require.NoError(t, err)
	assert.Equal(t, "shard-c", owner)
	assert.Greater(t, gen, claimed+1, "reclaiming a lapsed zone is an ownership change")
}

// TestZoneLeaseGenerationNeverRetreadsAValueAfterAReset is the anti-REVIVAL property, and it is the one the
// security review pressed hardest on.
//
// PERSIST protects `gen` from TTL expiry, but not from FLUSHDB, an allkeys-* eviction under maxmemory, a
// failover to a replica that lost writes, or a restore from an older snapshot. If the counter simply restarted
// at 1, then after any of those a zone would churn 1 → 2 → 3 … and, as it passed the value some long-captured
// AdoptZone was signed at, that request would VERIFY AGAIN. An Ed25519 signature never expires, so "the counter
// came back around" is a forced-host vector.
//
// So `gen` is seeded from the Redis clock on first creation. After a total wipe the counter restarts far above
// every value it ever issued, and an old capture can never match.
func TestZoneLeaseGenerationNeverRetreadsAValueAfterAReset(t *testing.T) {
	r, mr := newTestRedisWithClock(t)
	ctx := context.Background()

	// Drive a zone through a couple of ownership changes, as a normal rebalance would.
	ok, err := r.ClaimZone(ctx, "midgaard", "shard-a", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = r.HandoverZone(ctx, "midgaard", "shard-a", "shard-b", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	_, captured, err := r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	require.NotZero(t, captured)

	// Total directory loss. Everything the fence knew about this zone is gone.
	mr.FlushAll()
	_, gen, err := r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Zero(t, gen, "after a wipe the zone reads as never-claimed, and gen 0 is refused outright")

	// Recovery happens LATER in wall-clock time — that is the entire premise of the seed, and it is what the
	// test must model. Advancing the server clock explicitly also keeps this deterministic: without it, the
	// pre-wipe claim and the post-wipe reclaim can land in the same millisecond and the reseeded counter comes
	// back BELOW the captured generation, which is a real (if unrepresentative) failure of the property.
	mr.SetTime(time.Now().Add(time.Hour))

	// The cluster recovers: the zone is re-claimed and churns through more ownership changes than it ever had
	// before the wipe. Not one of them may land on a generation the zone previously issued.
	ok, err = r.ClaimZone(ctx, "midgaard", "shard-a", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	_, reseeded, err := r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Greater(t, reseeded, captured,
		"a re-seeded generation must start ABOVE every value the zone ever issued, or a captured AdoptZone revives")

	from, to := "shard-a", "shard-b"
	for i := 0; i < 8; i++ {
		ok, err = r.HandoverZone(ctx, "midgaard", from, to, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
		_, gen, err = r.ZoneLease(ctx, "midgaard")
		require.NoError(t, err)
		assert.NotEqual(t, captured, gen, "the counter must never re-tread a generation it has already issued")
		from, to = to, from
	}
}

// TestZoneLeaseGenerationsAreIndependentPerZone: the fence is per-zone, so a busy zone's churn must not age out
// a quiet zone's in-flight AdoptZone (and vice versa — a shared counter would make a captured request for zone
// X redeemable at the generation zone Y happened to reach).
func TestZoneLeaseGenerationsAreIndependentPerZone(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()

	for _, z := range []string{"midgaard", "darkwood"} {
		ok, err := r.ClaimZone(ctx, z, "shard-a", time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
	}
	_, midBefore, err := r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	_, darkBefore, err := r.ZoneLease(ctx, "darkwood")
	require.NoError(t, err)

	ok, err := r.HandoverZone(ctx, "midgaard", "shard-a", "shard-b", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	_, midAfter, err := r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	_, darkAfter, err := r.ZoneLease(ctx, "darkwood")
	require.NoError(t, err)
	assert.Equal(t, midBefore+1, midAfter, "midgaard changed hands")
	assert.Equal(t, darkBefore, darkAfter, "darkwood did not — its generation must be untouched")
}

// TestDirectorLeaseIsUnaffectedByTheZoneGeneration: ClaimLease and ClaimZone used to share one Lua script.
// #315 gave the zone a persistent generation counter, which a leader-election lease must NOT inherit — a
// director lease has no fence token to carry, and it relies on the key TTL to disappear. Pin the split, or a
// later refactor quietly re-merges them and every scope key becomes immortal.
func TestDirectorLeaseIsUnaffectedByTheZoneGeneration(t *testing.T) {
	r, mr := newTestRedisWithClock(t)
	ctx := context.Background()

	held, err := r.ClaimLease(ctx, "world", "inst-A", 200*time.Millisecond)
	require.NoError(t, err)
	require.True(t, held)

	key := r.leaseKey("world")
	fields, err := mr.HKeys(key)
	require.NoError(t, err)
	assert.NotContains(t, fields, "gen", "a director lease must carry no generation counter")
	assert.NotEqual(t, time.Duration(0), mr.TTL(key), "a director lease key must keep its TTL for GC")

	// A clean resign removes the key outright, so a standby sees a free lease and nothing lingers.
	require.NoError(t, r.ReleaseLease(ctx, "world", "inst-A"))
	assert.False(t, mr.Exists(key), "a released director lease key must be gone")

	held, err = r.ClaimLease(ctx, "world", "inst-B", time.Minute)
	require.NoError(t, err)
	assert.True(t, held, "a standby takes the resigned lease immediately")
}

// TestOnlyOneRacingHandoverCanFlipTheZone is the concurrent-drain case. Two sources may believe they own the
// zone — a rebalance directive racing a shutdown drain, or a source that was partitioned and came back — and
// both may have already made their destination build the zone. The directory arbitrates: `handoverZone` is
// owner-fenced, so exactly ONE flip lands, it bumps the generation exactly once, and the loser's AdoptZone
// (signed at the pre-flip generation) is thereby dead. Without the single-bump property the winner's OWN
// in-flight request would be invalidated too, and the drain would never converge.
func TestOnlyOneRacingHandoverCanFlipTheZone(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()

	ok, err := r.ClaimZone(ctx, "midgaard", "shard-a", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	_, preFlip, err := r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)

	// Both drains read the same generation and sign their AdoptZone at it. Now they race the flip.
	won, err := r.HandoverZone(ctx, "midgaard", "shard-a", "shard-b", time.Minute)
	require.NoError(t, err)
	assert.True(t, won, "the first flip from the live owner lands")

	lost, err := r.HandoverZone(ctx, "midgaard", "shard-a", "shard-c", time.Minute)
	require.NoError(t, err)
	assert.False(t, lost, "shard-a is no longer the live owner, so the second flip must be refused")

	owner, postFlip, err := r.ZoneLease(ctx, "midgaard")
	require.NoError(t, err)
	assert.Equal(t, "shard-b", owner, "exactly one destination wins the zone")
	assert.Equal(t, preFlip+1, postFlip,
		"the generation must move exactly once — the loser must not burn a second one")
}
