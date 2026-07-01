package directory

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandoverZone pins the Phase-16.4b fenced lease flip: a zone's live lease moves from the draining
// owner to the target ATOMICALLY (ShardForZone never reads ownerless mid-flip), only when the draining
// shard is still the live owner, and a stale/non-owner handover is refused.
func TestHandoverZone(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()

	// A owns the zone.
	held, err := r.ClaimZone(ctx, "midgaard", "shard-a", time.Second)
	require.NoError(t, err)
	require.True(t, held)

	// A hands it to B: fenced on A still being the live owner. The flip is atomic — at no observable point
	// is the zone ownerless, and ShardForZone flips straight from A to B.
	ok, err := r.HandoverZone(ctx, "midgaard", "shard-a", "shard-b", time.Second)
	require.NoError(t, err)
	require.True(t, ok, "handover from the live owner should succeed")

	owner, err := r.ShardForZone(ctx, "midgaard")
	require.NoError(t, err)
	assert.Equal(t, "shard-b", owner, "ownership must be B immediately after the flip (no gap)")

	// A can no longer hand it over — it is not the owner anymore (fencing: a stale source can't re-flip).
	ok, err = r.HandoverZone(ctx, "midgaard", "shard-a", "shard-c", time.Second)
	require.NoError(t, err)
	assert.False(t, ok, "a non-owner handover must be refused")
	owner, _ = r.ShardForZone(ctx, "midgaard")
	assert.Equal(t, "shard-b", owner, "a refused handover must not change ownership")

	// B, the live owner, can renew via its own ClaimZone and then hand off onward.
	ok, err = r.HandoverZone(ctx, "midgaard", "shard-b", "shard-c", time.Second)
	require.NoError(t, err)
	assert.True(t, ok)
	owner, _ = r.ShardForZone(ctx, "midgaard")
	assert.Equal(t, "shard-c", owner)

	// Handover of a zone NOBODY owns is refused (from-owner fence): can't conjure ownership via a handover.
	ok, err = r.HandoverZone(ctx, "never-claimed", "shard-a", "shard-b", time.Second)
	require.NoError(t, err)
	assert.False(t, ok, "handover of an unowned zone must be refused")
}
