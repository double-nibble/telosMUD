package integration

import (
	"context"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestStoreCommandsFormulasPvpLuaRoundTrip is the #20 regression net in realistic form: a DB-seeded pack's
// custom Lua verbs (Commands, Phase 7.4e), ruleset-formula overrides (Formulas, Phase 7.4f) and PvP-policy
// hook (PvpLua, Phase 7.4f) must survive ImportPack -> Load. Before #20 none had an INSERT/SELECT path, so a
// Postgres-sourced pack silently dropped all three — they survived ONLY on the embedded-YAML load path. The
// synthetic reflect net (store_reflect_test.go) proves no FIELD drops; this proves the realistic shapes and
// the strips-and-replaces re-import idempotency.
func TestStoreCommandsFormulasPvpLuaRoundTrip(t *testing.T) {
	p := helpers.OpenTestPool(t)
	ctx := context.Background()

	pk := content.Pack{
		Pack: "cmdfx",
		Commands: []content.CommandDTO{
			{Verb: "smile", Aliases: []string{"sm", "grin"}, Lua: `self:emit("You smile.")`},
			{Verb: "ponder", Lua: `self:emit("You ponder.")`}, // no aliases: must not synthesize a phantom one
		},
		Formulas: map[string]string{
			"to_hit": "return 20",
			"soak":   "return actor.armor * 2",
		},
		PvpLua: `function(a, t) return false end`,
	}
	require.NoError(t, p.ImportPack(ctx, pk), "import pack with commands/formulas/pvp")
	// Re-import is strips-and-replaces idempotent: a second import must neither duplicate rows nor error.
	require.NoError(t, p.ImportPack(ctx, pk), "re-import (idempotency)")

	lc, err := content.Load(ctx, p, []string{pk.Pack})
	require.NoError(t, err, "load pack from postgres")

	// Commands: both verbs, aliases + Lua intact, loaded exactly once despite the re-import.
	require.Len(t, lc.Commands, 2, "both custom verbs load exactly once after re-import (no duplication)")
	byVerb := map[string]content.CommandDTO{}
	for _, c := range lc.Commands {
		byVerb[c.Verb] = c
	}
	require.Equal(t, []string{"sm", "grin"}, byVerb["smile"].Aliases)
	require.Equal(t, `self:emit("You smile.")`, byVerb["smile"].Lua)
	require.Empty(t, byVerb["ponder"].Aliases, "a verb with no aliases round-trips empty, not a phantom alias")
	require.Equal(t, `self:emit("You ponder.")`, byVerb["ponder"].Lua)

	// Formulas: both entries with their bodies intact.
	require.Equal(t, map[string]string{"to_hit": "return 20", "soak": "return actor.armor * 2"}, lc.Formulas)

	// PvpLua: the pack-scalar policy hook, intact.
	require.Equal(t, `function(a, t) return false end`, lc.PvpLua)
}

// TestStoreCommandsFormulasMultiPackMerge proves the store load path honours the loader's cross-pack merge
// contract the migration comments promise (#20): PvpLua = last non-empty ENABLED pack wins; Formulas =
// last-write-wins by name; Commands accumulate across packs. Exercised through real Postgres. The pack names
// are chosen so alphabetical order (zzz_ before aaa_ is FALSE) is the OPPOSITE of enabled order, so a pass
// proves the winner is the enabled-LAST pack, not merely the alphabetically-last one.
func TestStoreCommandsFormulasMultiPackMerge(t *testing.T) {
	p := helpers.OpenTestPool(t)
	ctx := context.Background()

	first := content.Pack{ // enabled FIRST, but alphabetically LAST
		Pack:     "zzz_merge_first",
		Commands: []content.CommandDTO{{Verb: "wave", Lua: `-- first wave`}},
		Formulas: map[string]string{"to_hit": "return 1", "soak": "return 2"},
		PvpLua:   `-- first pvp`,
	}
	second := content.Pack{ // enabled LAST, but alphabetically FIRST -> must still win
		Pack:     "aaa_merge_second",
		Commands: []content.CommandDTO{{Verb: "bow", Lua: `-- second bow`}},
		Formulas: map[string]string{"to_hit": "return 99"}, // overrides first's to_hit
		PvpLua:   `-- second pvp`,                          // overrides first's pvp
	}
	require.NoError(t, p.ImportPack(ctx, first))
	require.NoError(t, p.ImportPack(ctx, second))

	// enabled order = [first, second], so `second` is the last-writer regardless of pack-name ordering.
	lc, err := content.Load(ctx, p, []string{first.Pack, second.Pack})
	require.NoError(t, err, "load two packs")

	require.Equal(t, `-- second pvp`, lc.PvpLua, "last non-empty ENABLED pack's PvpLua wins (not alphabetical)")
	require.Equal(t, "return 99", lc.Formulas["to_hit"], "a later pack overrides a same-named formula")
	require.Equal(t, "return 2", lc.Formulas["soak"], "a formula only the earlier pack defines still survives")

	verbs := map[string]bool{}
	for _, c := range lc.Commands {
		verbs[c.Verb] = true
	}
	require.True(t, verbs["wave"] && verbs["bow"], "custom verbs accumulate across packs (both present)")
}
