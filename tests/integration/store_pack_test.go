// Package integration holds the project's integration tests (all under tests/integration,
// per the project TEST STANDARD — see docs/TESTING.md). These are gated on a real Postgres
// via TELOS_TEST_DSN: the default hermetic `go test ./...` skips them, and `make
// test-integration` (or CI with a Postgres service) runs them.
//
// This file is the black-box conversion exemplar for the standard: it lives in package
// `integration` and reaches the store ONLY through its exported API (store.Open / ImportPack /
// content.Load), uses testify (require/assert), and prefers table-driven assertions. The one
// store test that needs unexported internals (TestCharacterCRUD, which pokes p.pool to clean up
// a row) stays co-located in internal/store as a unit test — it cannot move to a black-box
// package without exporting plumbing that exists only for the test.
package integration

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// normalizeContent sorts a loaded pack's zones and their child slices by stable ref so two loads
// compare independent of slice order. The DB path returns rows ORDER BY ref (alphabetical) while
// the embedded YAML preserves authoring order — the CONTENT is identical, only the order differs,
// so the round-trip parity check must be order-insensitive.
func normalizeContent(zones []content.ZoneDTO) []content.ZoneDTO {
	out := append([]content.ZoneDTO(nil), zones...)
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	for zi := range out {
		z := &out[zi]
		z.Rooms = append([]content.RoomDTO(nil), z.Rooms...)
		sort.Slice(z.Rooms, func(i, j int) bool { return z.Rooms[i].Ref < z.Rooms[j].Ref })
		for ri := range z.Rooms {
			// Canonicalize an unflagged room's Flags to nil. The two loaders represent
			// "no flags" DIFFERENTLY but EQUIVALENTLY: the YAML loader leaves Flags nil,
			// while the DB loader COALESCEs a missing flags key to '[]'::jsonb and
			// unmarshals it into a non-nil []string{}. reflect.DeepEqual treats nil and
			// []string{} as unequal, so without this the parity check fails on a Go
			// nil-vs-empty distinction that is not a content difference.
			if len(z.Rooms[ri].Flags) == 0 {
				z.Rooms[ri].Flags = nil
			}
		}
		z.Items = append([]content.ProtoDTO(nil), z.Items...)
		sort.Slice(z.Items, func(i, j int) bool { return z.Items[i].Ref < z.Items[j].Ref })
		z.Mobs = append([]content.ProtoDTO(nil), z.Mobs...)
		sort.Slice(z.Mobs, func(i, j int) bool { return z.Mobs[i].Ref < z.Mobs[j].Ref })
	}
	return out
}

// TestStorePackRoundTrip is the 4.1 carry-forward: import the embedded demo pack into Postgres,
// LoadPacks it back, and assert the assembled LoadedContent equals what the embedded loader
// produces directly — the DB path and the YAML path agree (the parity guard, exercised through
// real SQL). It then pins the global-def round-trips (mob Living, room-scoped affect, per-round
// reaction budget) that the zone DeepEqual does NOT cover — the exact gap class that hid the
// mob-Living drop and (earlier) the ability_defs drop.
func TestStorePackRoundTrip(t *testing.T) {
	p := helpers.OpenTestPool(t)
	ctx := context.Background()

	data, err := content.DemoPackBytes()
	require.NoError(t, err)
	pk, err := content.ParsePack(data)
	require.NoError(t, err)
	require.NoError(t, p.ImportPack(ctx, pk), "import demo pack")

	fromDB, err := content.Load(ctx, p, []string{content.DemoPack})
	require.NoError(t, err, "load from postgres")
	fromYAML, err := content.LoadDemoPack()
	require.NoError(t, err, "load embedded")

	// Compare order-insensitively: the DB returns rows ORDER BY ref, the YAML keeps authoring
	// order, so normalize both before DeepEqual (the content, not the slice order, is the contract).
	dbZones := normalizeContent(fromDB.Zones)
	yamlZones := normalizeContent(fromYAML.Zones)
	if !reflect.DeepEqual(dbZones, yamlZones) {
		t.Fatalf("round-trip mismatch:\n DB  = %+v\n YAML= %+v", dbZones, yamlZones)
	}

	// Pin the combat-mob Living round-trip (Phase 6.3a): the darkwood goblin's Living block (its
	// stat sheet + `melee` profile ref) must survive the mob_prototypes.body JSONB trip. Before the
	// protoBody.Living fix this came back nil from the DB path; this names the regression directly.
	goblin := findMob(dbZones, "darkwood:mob:goblin")
	require.NotNil(t, goblin, "round-trip: darkwood goblin mob missing from DB-loaded content")
	require.NotNil(t, goblin.Living, "round-trip: goblin Living block was DROPPED on the DB path (mob stat sheet lost)")
	assert.Equal(t, "melee", goblin.Living.CombatProfile, "round-trip: goblin combat_profile")
	assert.Equal(t, float64(14), goblin.Living.Attributes["strength"], "round-trip: goblin strength (Living attributes lost)")

	// Pin the room-scoped affect round-trip (Phase 6.4a, [G13]): the `web` affect's top-level Scope
	// ("room") must survive the DB path. It is a pack-GLOBAL def (not zone content), so the zone
	// DeepEqual above does NOT cover it — the gap class that hid the mob-Living and ability_defs
	// drops. Before the affect_defs.scope column it loaded as "" => roomScoped false.
	yamlWeb := findAffect(fromYAML.Affects, "web")
	require.NotNil(t, yamlWeb, "round-trip: 'web' affect missing from embedded YAML content (test precondition)")
	require.Equal(t, "room", yamlWeb.Scope, "round-trip: embedded 'web' affect scope (test precondition)")
	dbWeb := findAffect(fromDB.Affects, "web")
	require.NotNil(t, dbWeb, "round-trip: 'web' affect missing from DB-loaded content")
	assert.Equalf(t, "room", dbWeb.Scope, "round-trip: 'web' affect scope was DROPPED on the DB path "+
		"(room-scoped affect would attach to one entity instead of the room)")

	// Pin the per-round reaction-budget flag round-trip (Phase 6.4b, [G9]): the `reactions`
	// resource's top-level PerRound must survive the DB path (it rides the resource body JSONB).
	// Same global-def gap class — without it the reaction budget never refreshes.
	dbReactions := findResource(fromDB.Resources, "reactions")
	require.NotNil(t, dbReactions, "round-trip: 'reactions' resource missing from DB-loaded content")
	assert.True(t, dbReactions.PerRound, "round-trip: 'reactions' resource per_round was DROPPED on the DB "+
		"path (the reaction budget would never refresh, breaking opportunity attacks)")
}

// TestImportPackIdempotent pins the seed/import idempotency contract (the deletePack regression). A
// pack re-import is meant to STRIP the pack's prior rows in one transaction, then re-insert — so
// running `make seed` / `make up` twice against a populated database replaces content rather than
// colliding. The bug: deletePack cleared attribute/resource/damage_type/affect defs but OMITTED
// ability_defs, so the SECOND import failed on "duplicate key value violates unique constraint
// ability_defs_pkey" (e.g. fireball). It survived several slices because it only reproduced against
// REAL Postgres on a RE-import — exactly the gap a single-import or in-memory test cannot see. This
// test imports the demo pack twice and asserts the second succeeds with content intact.
func TestImportPackIdempotent(t *testing.T) {
	p := helpers.OpenTestPool(t)
	ctx := context.Background()

	data, err := content.DemoPackBytes()
	require.NoError(t, err)
	pk, err := content.ParsePack(data)
	require.NoError(t, err)

	require.NoError(t, p.ImportPack(ctx, pk), "first import")
	// THE REGRESSION: the second import must strip-and-replace, not collide on a duplicate key.
	// Before the deletePack fix this returned the ability_defs_pkey violation.
	require.NoError(t, p.ImportPack(ctx, pk), "second import must be idempotent (strip-and-replace)")

	lc, err := content.Load(ctx, p, []string{content.DemoPack})
	require.NoError(t, err, "load after re-import")

	// Each global-def kind must load back exactly once after the re-import. Table-driven: every row
	// is a count-by-ref over a def slice plus the expected count (exactly 1 for the named def the
	// strip-and-replace must clear, so it is never duplicated AND never dropped).
	countRefs := func(refs []string, want string) int {
		n := 0
		for _, r := range refs {
			if r == want {
				n++
			}
		}
		return n
	}
	cases := []struct {
		name string
		refs []string
		want string // the ref that must appear exactly once
	}{
		{"ability fireball (the original deletePack collision)", abilityRefs(lc), "fireball"},
		{"combat profile melee (Phase 6.3a)", combatProfileRefs(lc), "melee"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, 1, countRefs(tc.refs, tc.want),
				"after re-import: %q must appear exactly once (strip-and-replace, never duplicated/dropped)", tc.want)
		})
	}

	// Content intact after the re-import: the global def kinds are all present.
	assert.NotEmpty(t, lc.Attributes, "after re-import: attribute defs missing")
	assert.NotEmpty(t, lc.Resources, "after re-import: resource defs missing")
	assert.NotEmpty(t, lc.Affects, "after re-import: affect defs missing")

	// The pack's default_combat scalar (pack_meta) intact after the re-import.
	assert.Equal(t, "melee", lc.DefaultCombat, "after re-import: default_combat")

	// Room-scoped affect (Phase 6.4a, [G13]): the `web` affect's scope must survive the re-import —
	// the affect_defs.scope column is overwritten (not collided) by strip-and-replace.
	web := findAffect(lc.Affects, "web")
	require.NotNil(t, web, "after re-import: 'web' affect missing")
	assert.Equal(t, "room", web.Scope, "after re-import: 'web' affect scope")
}

func abilityRefs(lc *content.LoadedContent) []string {
	out := make([]string, 0, len(lc.Abilities))
	for _, ab := range lc.Abilities {
		out = append(out, ab.Ref)
	}
	return out
}

func combatProfileRefs(lc *content.LoadedContent) []string {
	out := make([]string, 0, len(lc.CombatProfiles))
	for _, cp := range lc.CombatProfiles {
		out = append(out, cp.Ref)
	}
	return out
}

// findResource returns the pack-global ResourceDTO with the given ref, or nil.
func findResource(resources []content.ResourceDTO, ref string) *content.ResourceDTO {
	for i := range resources {
		if resources[i].Ref == ref {
			return &resources[i]
		}
	}
	return nil
}

// findAffect returns the pack-global AffectDTO with the given ref, or nil.
func findAffect(affects []content.AffectDTO, ref string) *content.AffectDTO {
	for i := range affects {
		if affects[i].Ref == ref {
			return &affects[i]
		}
	}
	return nil
}

// findMob returns the mob ProtoDTO with the given ref across all zones, or nil.
func findMob(zones []content.ZoneDTO, ref string) *content.ProtoDTO {
	for zi := range zones {
		for mi := range zones[zi].Mobs {
			if zones[zi].Mobs[mi].Ref == ref {
				return &zones[zi].Mobs[mi]
			}
		}
	}
	return nil
}
