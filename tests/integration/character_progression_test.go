package integration

import (
	"context"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/world"
	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestCharacterProgressionSubtreesRoundTrip closes #136: the runtime progression subtrees — tracks (the
// per-track advancement high-water), granted abilities, learned professions, attribute BASE overrides, the
// named-flag set, loot pity, and armed cooldowns — must survive a real-Postgres SaveCharacter -> LoadCharacter.
// progression_journey_test proves the apply->dump->load path in MEMORY, but nothing exercises these through
// the JSONB; TestCharacterCRUD leaves every one of them empty. The state column round-trips StateJSON verbatim,
// so a dropped field (a lost json tag, a state-column truncation) would slip silently — the same silent-drop
// class the store reflect-net caught for def slices. Gated on TELOS_TEST_DSN via OpenTestPool.
func TestCharacterProgressionSubtreesRoundTrip(t *testing.T) {
	pool := helpers.OpenTestPool(t)
	ctx := context.Background()

	// Unique name per run (the black-box package can't hard-delete the CITEXT-unique row; an orphan is harmless).
	name := "Progression-" + time.Now().Format("150405.000000000")
	_, err := pool.CreateCharacter(ctx, name, "midgaard", "midgaard:room:temple")
	require.NoError(t, err)

	snap, found, err := pool.LoadCharacter(ctx, name)
	require.NoError(t, err)
	require.True(t, found)

	// Populate every runtime subtree TestCharacterCRUD leaves empty, each non-empty so a drop is caught.
	snap.State.Tracks = map[string]int{"combat": 3, "arcane": 1}
	snap.State.Flags = []string{"pvp", "veteran"}
	snap.State.Abilities = []string{"fireball", "cleave"}
	snap.State.Professions = []string{"blacksmith"}
	snap.State.Attributes = map[string]float64{"strength": 16, "constitution": 14}
	snap.State.LootPity = map[string]int{"rare_sword": 12}
	snap.State.Cooldowns = map[string]int{"fireball": 30}
	// The remaining pure-DATA subtrees the store round-trips verbatim (the world-side clamp/re-attach on
	// load is NOT on the store path, so require.Equal is faithful here). Affects is a NESTED struct — a
	// dropped inner tag would slip past both TestCharacterCRUD and the def-slice reflect-net.
	snap.State.Resources = map[string]world.ResourceJSON{"hp": {Cur: 42}, "mana": {Cur: 17}}
	snap.State.Affects = []world.AffectJSON{{ID: "poison", Remaining: 30, Mag: 2.5, Stacks: 3}}

	res, err := pool.SaveCharacter(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, world.SaveApplied, res.Outcome, "CAS save at the matching version should apply")

	reloaded, found, err := pool.LoadCharacter(ctx, name)
	require.NoError(t, err)
	require.True(t, found)

	require.Equal(t, snap.State.Tracks, reloaded.State.Tracks, "tracks (progression high-water) dropped in the PG round-trip")
	require.Equal(t, snap.State.Flags, reloaded.State.Flags, "the named-flag set dropped in the PG round-trip")
	require.Equal(t, snap.State.Abilities, reloaded.State.Abilities, "granted abilities dropped in the PG round-trip")
	require.Equal(t, snap.State.Professions, reloaded.State.Professions, "learned professions dropped in the PG round-trip")
	require.Equal(t, snap.State.Attributes, reloaded.State.Attributes, "attribute base overrides dropped in the PG round-trip")
	require.Equal(t, snap.State.LootPity, reloaded.State.LootPity, "loot pity dropped in the PG round-trip")
	require.Equal(t, snap.State.Cooldowns, reloaded.State.Cooldowns, "armed cooldowns dropped in the PG round-trip")
	require.Equal(t, snap.State.Resources, reloaded.State.Resources, "resource pools dropped in the PG round-trip")
	require.Equal(t, snap.State.Affects, reloaded.State.Affects, "active affects (nested struct) dropped in the PG round-trip")
}
