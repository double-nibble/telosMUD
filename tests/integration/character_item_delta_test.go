package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/world"
	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestCharacterItemDeltaRoundTrip closes #87: the per-instance item DELTA (quality + bound + stack + kept)
// must survive a real-Postgres SaveCharacter -> LoadCharacter. TestCharacterCRUD persists inventory with an
// EMPTY delta only, so nothing today proves a POPULATED ItemJSON.Delta actually rides the character state
// JSONB — a regression that dropped Delta from StateJSON's marshal (or the ItemJSON `delta` tag) would slip
// silently. Gated on TELOS_TEST_DSN via OpenTestPool.
func TestCharacterItemDeltaRoundTrip(t *testing.T) {
	pool := helpers.OpenTestPool(t)
	ctx := context.Background()

	// Unique name per run (the black-box integration package can't hard-delete the CITEXT-unique row, so a
	// timestamped name avoids re-run collisions; an orphaned row is harmless, like the e2e characters).
	name := "ItemDelta-" + time.Now().Format("150405.000000000")
	pid, err := pool.CreateCharacter(ctx, name, "midgaard", "midgaard:room:temple")
	require.NoError(t, err)

	snap, found, err := pool.LoadCharacter(ctx, name)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, pid, snap.PID)

	// A FULLY-populated delta: quality (level + affixes), bound, a partial stack, and kept — every field of
	// itemDeltaJSON non-zero so a drop of any one on the JSONB path is caught.
	delta := json.RawMessage(`{"quality":{"level":5,"affixes":{"fire":2,"keen":1}},"bound":true,"stack":7,"kept":true}`)
	snap.State.Inventory = []world.ItemJSON{{ProtoRef: "midgaard:obj:sword", Delta: delta}}

	res, err := pool.SaveCharacter(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, world.SaveApplied, res.Outcome, "CAS save at the matching version should apply")

	reloaded, found, err := pool.LoadCharacter(ctx, name)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, reloaded.State.Inventory, 1)
	got := reloaded.State.Inventory[0]
	require.Equal(t, "midgaard:obj:sword", got.ProtoRef)
	require.JSONEqf(t, string(delta), string(got.Delta),
		"the item delta (quality/bound/stack/kept) must survive the real-PG round-trip intact (#87); got %q", string(got.Delta))
}
