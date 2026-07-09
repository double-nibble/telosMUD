package store

import (
	"context"
	"testing"
	"time"

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

	// The test needs the key ABSENT (the version-0 CAS-create is the first assertion), and the local
	// dev database persists across runs — so scrub before AND after (before covers a prior failed run).
	// The up-front scrub is a PRECONDITION, so its error fails fast; the cleanup one is best-effort.
	_, err := p.pool.Exec(ctx, `DELETE FROM world_state WHERE key = $1`, key)
	require.NoError(t, err, "pre-scrub world_state")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM world_state WHERE key = $1`, key)
	})

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

	// Same re-run-safety scrub as the world-state test: the version-0 CAS-create and the
	// other-region not-found probe both require these rows absent at start.
	_, err := p.pool.Exec(ctx,
		`DELETE FROM region_state WHERE region_id IN ($1, $2) AND key = $3`, region, "test_other", key)
	require.NoError(t, err, "pre-scrub region_state")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(),
			`DELETE FROM region_state WHERE region_id IN ($1, $2) AND key = $3`, region, "test_other", key)
	})

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

// TestSnapshotScopeState pins the #44 snapshot reads against real Postgres: SnapshotWorldState /
// SnapshotRegionState return EVERY key->value for their scope, so a joining zone can seed its read-replica.
func TestSnapshotScopeState(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	region := "snapregion-" + suffix
	wk1 := "snapw1-" + suffix
	wk2 := "snapw2-" + suffix
	rk := "snapr1-" + suffix
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM world_state WHERE key = ANY($1)`, []string{wk1, wk2})
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM region_state WHERE region_id = $1`, region)
	})

	if _, _, err := p.SaveWorldState(ctx, wk1, []byte(`{"a":1}`), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.SaveWorldState(ctx, wk2, []byte(`true`), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.SaveRegionState(ctx, region, rk, []byte(`"tense"`), 0); err != nil {
		t.Fatal(err)
	}

	world, err := p.SnapshotWorldState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The value column is JSONB, so Postgres re-serializes ({"a":1} -> {"a": 1}); compare as JSON.
	require.Contains(t, world, wk1)
	require.Contains(t, world, wk2)
	assert.JSONEq(t, `{"a":1}`, string(world[wk1]))
	assert.JSONEq(t, `true`, string(world[wk2]))

	reg, err := p.SnapshotRegionState(ctx, region)
	if err != nil {
		t.Fatal(err)
	}
	require.Len(t, reg, 1)
	assert.JSONEq(t, `"tense"`, string(reg[rk]))
}
