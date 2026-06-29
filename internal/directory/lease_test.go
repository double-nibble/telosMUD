package directory

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClaimLease pins the generic leader-election lease (Phase 10.1c) against miniredis: an owner claims
// it, a SECOND owner is refused while it's held, the holder can renew its own, and a release frees it for
// the other — the mutual exclusion the director relies on for "exactly one live owner per scope".
func TestClaimLease(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()

	held, err := r.ClaimLease(ctx, "world", "inst-A", time.Second)
	require.NoError(t, err)
	require.True(t, held, "first claimant should hold the lease")

	held, err = r.ClaimLease(ctx, "world", "inst-B", time.Second)
	require.NoError(t, err)
	assert.False(t, held, "a second owner must not take a held lease")

	// The holder renews its own.
	held, err = r.ClaimLease(ctx, "world", "inst-A", time.Second)
	require.NoError(t, err)
	assert.True(t, held, "owner should be able to renew its own lease")

	// Release frees it for the other.
	require.NoError(t, r.ReleaseLease(ctx, "world", "inst-A"))
	held, err = r.ClaimLease(ctx, "world", "inst-B", time.Second)
	require.NoError(t, err)
	assert.True(t, held, "after release the other owner can claim")

	// A release by a NON-owner is a no-op (the CAS arbitrates) — inst-A can't evict inst-B.
	require.NoError(t, r.ReleaseLease(ctx, "world", "inst-A"))
	held, err = r.ClaimLease(ctx, "world", "inst-A", time.Second)
	require.NoError(t, err)
	assert.False(t, held, "a non-owner release must not free another's lease")

	// Distinct lease ids are independent.
	held, err = r.ClaimLease(ctx, "region:duskwall", "inst-A", time.Second)
	require.NoError(t, err)
	assert.True(t, held, "a different lease id is independent")
}
