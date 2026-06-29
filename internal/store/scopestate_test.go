package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scopestate_test.go is the gated (TELOS_TEST_DSN) Postgres round-trip + CAS test for director scope
// state (Phase 10.1). It pins: a fresh key loads as not-found; the first write creates it at version 1;
// a write with the right expected version succeeds and bumps; a write with a STALE expected version is
// REJECTED (ok=false) and does not clobber — the failover backstop.

func TestWorldStateRoundTripAndCAS(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	const key = "test_invasion_active"

	// Absent → not found, version 0.
	_, ver, found, err := p.LoadWorldState(ctx, key)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, uint64(0), ver)

	// First write (expected 0) creates at version 1.
	nv, ok, err := p.SaveWorldState(ctx, key, []byte(`{"phase":1}`), 0)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(1), nv)

	// Load reflects it.
	val, ver, found, err := p.LoadWorldState(ctx, key)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, uint64(1), ver)
	assert.JSONEq(t, `{"phase":1}`, string(val))

	// A correct-version write bumps to 2.
	nv, ok, err = p.SaveWorldState(ctx, key, []byte(`{"phase":2}`), 1)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(2), nv)

	// A STALE write (expected 1, but it's now 2) is rejected and does NOT clobber.
	_, ok, err = p.SaveWorldState(ctx, key, []byte(`{"phase":99}`), 1)
	require.NoError(t, err)
	assert.False(t, ok, "a stale-version write must lose the CAS")
	val, ver, _, err = p.LoadWorldState(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), ver)
	assert.JSONEq(t, `{"phase":2}`, string(val), "the stale write must not have clobbered the value")
}

func TestRegionStateRoundTripAndCAS(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	const region, key = "test_duskwall", "mood"

	nv, ok, err := p.SaveRegionState(ctx, region, key, []byte(`"besieged"`), 0)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(1), nv)

	val, ver, found, err := p.LoadRegionState(ctx, region, key)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, uint64(1), ver)
	assert.JSONEq(t, `"besieged"`, string(val))

	// A different region's same key is independent.
	_, _, found, err = p.LoadRegionState(ctx, "test_other", key)
	require.NoError(t, err)
	assert.False(t, found)

	// Stale CAS rejected.
	_, ok, err = p.SaveRegionState(ctx, region, key, []byte(`"liberated"`), 0)
	require.NoError(t, err)
	assert.False(t, ok)
}
