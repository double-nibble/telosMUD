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
	"encoding/json"
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

// canonicalizeDefs canonicalizes a pack-GLOBAL def slice so the YAML-loaded and Postgres-loaded
// forms compare with reflect.DeepEqual regardless of representation differences the two load paths
// introduce. It is the order-insensitive, representation-insensitive analog of normalizeContent for
// the SIX global def kinds (attributes/resources/damage-types/affects/abilities/combat-profiles).
//
// Why a JSON round-trip rather than per-field guards: these DTOs carry many `any`/formula/op-list
// fields (FormulaNodeDTO, OnResolve, ToHit, Avoidance, the OnEvent maps) plus typed numeric fields,
// and the two loaders disagree on representation in two systematic ways the persistence-engineer
// documented:
//
//   - int vs float64: the YAML loader yields Go ints for some numeric fields, while a value that
//     round-trips through the DB's JSONB body comes back float64. Marshaling BOTH sides to JSON and
//     unmarshaling into `any` makes every number a float64 uniformly, so the distinction vanishes.
//   - nil vs empty: the DB path COALESCEs a missing slice/map to '[]'/'{}'::jsonb and unmarshals a
//     non-nil empty container, while the YAML path leaves it nil — and reflect.DeepEqual treats nil
//     and []T{} (or map{}) as unequal (the same trap normalizeContent handles for room Flags). The
//     recursive strip below drops null, empty arrays, and empty objects so the two normalize alike.
//
// The result is a []map[string]any sorted by ref. A DeepEqual over it auto-catches ANY future
// top-level field drop on the store import/load path — the systemic blind spot (three regressions:
// ability_defs, mob Living, affect Scope, resource PerRound) the zones-only round-trip never saw.
//
// One residual bound (persistence review, NOT a serialization gap): because the strip folds zero/empty
// on both sides, the catch only fires for a field the DEMO PACK populates with a non-zero value. A
// persisted field that is empty across all demo content would drop invisibly — close that by giving the
// demo pack a non-trivial value for it, not by changing this helper. The three historical regressions
// are all covered (the demo pack carries a non-zero value for each).
func canonicalizeDefs(t *testing.T, defs any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(defs)
	require.NoError(t, err, "canonicalizeDefs: marshal")
	var out []map[string]any
	require.NoError(t, json.Unmarshal(raw, &out), "canonicalizeDefs: unmarshal")
	for i := range out {
		applyImportDefaults(out[i])
		if v := stripEmpty(out[i]); v != nil {
			out[i] = v.(map[string]any)
		} else {
			out[i] = map[string]any{}
		}
	}
	sort.Slice(out, func(i, j int) bool { return refOf(out[i]) < refOf(out[j]) })
	return out
}

// applyImportDefaults collapses the DEFAULT-injection difference between the two load paths so the
// deep-compare measures content equivalence, not raw-zero-vs-defaulted representation. The store's
// ImportPack (internal/store/import.go) rewrites a handful of empty/zero DTO fields to their canonical
// default before the row is written (an empty stack_scope -> "source", invocation -> "command",
// max_stacks < 1 -> 1, scope -> "entity", a nil tags -> []), so the DB path reads those values back
// EXPLICIT while the embedded-YAML DTO keeps the author's zero value. Both map to identical runtime
// behavior (the world mapper applies the same defaults on either path), so this is a representation
// difference BELOW the content contract — exactly the kind normalizeContent already folds for room
// Flags. We apply the SAME defaults to BOTH sides here; mirror import.go if a default is added there.
//
// This is deliberately NARROW: it touches only the named default-injected fields. Any OTHER top-level
// field the DB path drops or mangles still surfaces in the deep-compare — that is the whole point.
func applyImportDefaults(m map[string]any) {
	defaultStr := func(key, def string) {
		if s, _ := m[key].(string); s == "" {
			if _, present := m[key]; present {
				m[key] = def
			}
		}
	}
	// Affect defaults (import.go affect loop).
	defaultStr("stack_scope", "source")
	defaultStr("stacking", "refresh")
	defaultStr("scope", "entity")
	if v, present := m["max_stacks"]; present {
		if n, ok := v.(float64); ok && n < 1 {
			m["max_stacks"] = float64(1)
		}
	}
	// Ability defaults (import.go ability loop).
	defaultStr("invocation", "command")
}

// refOf returns the "ref" field of a canonicalized def map (empty if absent — every def has one).
func refOf(m map[string]any) string {
	if r, ok := m["ref"].(string); ok {
		return r
	}
	return ""
}

// stripEmpty recursively returns nil for any null, empty array, or empty object so that the
// nil-vs-empty-container differences between the two load paths normalize identically. A map drops
// keys whose values strip to nil; a non-empty container is returned with its surviving children.
// Scalars pass through unchanged (numbers are already float64 post-JSON, so int↔float64 is moot).
func stripEmpty(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if s := stripEmpty(val); s != nil {
				out[k] = s
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			out = append(out, stripEmpty(val))
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return x
	}
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

	pk, found, err := content.LoadPack(content.DemoPack)
	require.NoError(t, err)
	require.True(t, found, "embedded demo pack present")
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

	// Zone-level scalar (#72): `instanceable` is the content opt-in to being minted as an instance template,
	// and it rides the zones.body JSONB — a column that existed since the first definition migration but had
	// NO occupant until this flag, so neither insertZone nor LoadPacks touched it.
	//
	// That is precisely the field-drop trap this repo keeps re-shipping (Round 35's `primary` on resourceBody;
	// Track 11 before it): a new definition field parses from YAML, survives an in-memory load, and comes back
	// ZERO through Postgres. The normalized zone DeepEqual above would catch it, but only as an opaque
	// whole-struct diff across every zone in the pack — so assert it directly and by NAME, and assert it in
	// BOTH directions, because the two failures are different bugs:
	//
	//   - crypt true->false is the DROP (the write or the read lost the field), and it silently disables the
	//     dungeon door on any deployment served from the store rather than the embed.
	//   - midgaard false->true would be a DEFAULT-ON bug (e.g. a NULL body read as "unset means allowed"),
	//     which is the security-relevant direction: it re-opens the uncapped faucet for every zone.
	dbCrypt := fromDB.Zone("crypt")
	require.NotNil(t, dbCrypt, "crypt zone survived the round trip")
	assert.True(t, dbCrypt.Instanceable,
		"round-trip DROPPED zones.instanceable for crypt: the flag parses from YAML but returns false from "+
			"Postgres, so the instance opt-in silently stops working wherever content is served from the store")
	dbTown := fromDB.Zone("midgaard")
	require.NotNil(t, dbTown, "midgaard zone survived the round trip")
	assert.False(t, dbTown.Instanceable,
		"round-trip turned zones.instanceable ON for midgaard, which never opted in. Defaulting the opt-in to "+
			"true re-opens the uncapped item faucet (a mint runs the zone's boot resets into a private copy) "+
			"for every zone in content")

	// Room-level MAP (#435): instance_entrances rides the rooms.body JSONB, because an exit row's `to_room`
	// carries a foreign key into rooms(ref) and an entrance names a ZONE, which could never satisfy it.
	//
	// The same field-drop trap as `instanceable` above, and the reason it is asserted BY NAME rather than left
	// to the whole-struct DeepEqual: this field is written in insertRooms and must be read back in TWO
	// separate places (loadRoomDefinition for a single ref, loadRooms for a whole pack), and a field missing
	// from either survives every world test — those load from the YAML tree, not from Postgres. RoomDTO.Lua is
	// dropped by this very path today, which is what the trap looks like when nobody asserts.
	//
	// A dropped entrance is silent and total: the door simply is not there, on any deployment served from the
	// store rather than the embed, and the player is told "You can't go that way."
	var dbGuildhall *content.RoomDTO
	for zi := range fromDB.Zones {
		for ri := range fromDB.Zones[zi].Rooms {
			if fromDB.Zones[zi].Rooms[ri].Ref == "midgaard:room:guildhall" {
				dbGuildhall = &fromDB.Zones[zi].Rooms[ri]
			}
		}
	}
	require.NotNil(t, dbGuildhall, "the guild hall survived the round trip")
	assert.Equal(t, "crypt", dbGuildhall.InstanceEntrances["enter"],
		"round-trip DROPPED rooms.instance_entrances: the door parses from YAML and comes back missing from "+
			"Postgres, so the dungeon door silently does not exist wherever content is served from the store")
	assert.NotContains(t, dbGuildhall.Exits, "enter",
		"the entrance came back as an ordinary EXIT. That is not a cosmetic difference: every mover — hMove "+
			"from a trigger, directional flee, room-and-adjacent AoE — resolves through exits, so a door in "+
			"that map can be traversed on another party's initiative")

	// Pack-level scalar (persistence review): DefaultCombat rides pack_meta.default_combat — it is
	// NEITHER zone content nor one of the six def slices, so neither DeepEqual reaches it. A drop on its
	// store import/load path is the same top-level-field-drop class this test closes, so pin it directly.
	assert.Equal(t, fromYAML.DefaultCombat, fromDB.DefaultCombat, "round-trip: pack default_combat scalar")

	// Pack-level scalar (#47): WorldScript rides pack_meta.world_script — the demo ships one, so a drop on
	// the store import/load path shows here. Non-empty in the demo, so this also asserts the field is populated.
	assert.Equal(t, fromYAML.WorldScript, fromDB.WorldScript, "round-trip: pack world_script scalar")
	assert.NotEmpty(t, fromYAML.WorldScript, "the demo pack should ship a world_script (#47)")

	// Deep-compare every pack-GLOBAL def kind, the same way the zone DeepEqual above covers zone
	// content. THIS is the systemic catch: a global def is NOT zone content, so the zones-only
	// round-trip never saw a top-level field silently dropped on the store import/load path — the
	// exact gap class behind THREE regressions (mob Living, affect Scope, resource PerRound), each
	// previously patched with a one-off per-field guard. A whole-struct DeepEqual over the canonical
	// form auto-catches ANY field drop, present or future, with no per-field maintenance. Table-driven
	// over all six slices so a failure names the def kind that diverged.
	defCases := []struct {
		name string
		db   any // the DB-loaded def slice
		yaml any // the embedded-YAML def slice
	}{
		{"attributes", fromDB.Attributes, fromYAML.Attributes},
		{"resources", fromDB.Resources, fromYAML.Resources},
		{"damage_types", fromDB.DamageTypes, fromYAML.DamageTypes},
		{"affects", fromDB.Affects, fromYAML.Affects},
		{"abilities", fromDB.Abilities, fromYAML.Abilities},
		{"combat_profiles", fromDB.CombatProfiles, fromYAML.CombatProfiles},
		{"channels", fromDB.Channels, fromYAML.Channels},
		{"regions", fromDB.Regions, fromYAML.Regions},
		{"tracks", fromDB.Tracks, fromYAML.Tracks},
		{"bundles", fromDB.Bundles, fromYAML.Bundles},
		{"rarity_tiers", fromDB.RarityTiers, fromYAML.RarityTiers},
		{"affix_defs", fromDB.Affixes, fromYAML.Affixes},
		{"loot_tables", fromDB.LootTables, fromYAML.LootTables},
		{"spawn_schedules", fromDB.SpawnSchedules, fromYAML.SpawnSchedules},
		{"recipes", fromDB.Recipes, fromYAML.Recipes},
		{"wear_slots", fromDB.WearSlots, fromYAML.WearSlots},
		{"chargens", fromDB.Chargens, fromYAML.Chargens},
		{"help_defs", fromDB.HelpDefs, fromYAML.HelpDefs},
	}
	for _, tc := range defCases {
		t.Run(tc.name, func(t *testing.T) {
			dbDefs := canonicalizeDefs(t, tc.db)
			yamlDefs := canonicalizeDefs(t, tc.yaml)
			// require.Equal renders a readable per-field diff; it subsumes the old hand-written guards
			// (mob Living lives on a mob proto, but resource PerRound / affect Scope are pure global
			// defs caught HERE). Any top-level field the DB path drops shows as a missing/zeroed key.
			require.Equalf(t, yamlDefs, dbDefs,
				"global def round-trip mismatch for %s: the DB import/load path diverged from the embedded "+
					"YAML — a top-level field was dropped or mis-serialized on the store path", tc.name)
		})
	}

	// One named regression pin retained for the failure MESSAGE: mob Living rides a mob PROTOTYPE
	// (zone content), not a global def, so the six-slice deep-compare above does NOT cover it — the
	// zone DeepEqual does, but only opaquely ("round-trip mismatch"). This names the Phase 6.3a
	// protoBody.Living drop directly: before the fix the goblin's stat sheet came back nil from the DB.
	goblin := findMob(dbZones, "darkwood:mob:goblin")
	require.NotNil(t, goblin, "round-trip: darkwood goblin mob missing from DB-loaded content")
	require.NotNil(t, goblin.Living, "round-trip: goblin Living block was DROPPED on the DB path (mob stat sheet lost)")
	yamlGoblin := findMob(normalizeContent(fromYAML.Zones), "darkwood:mob:goblin")
	require.NotNil(t, yamlGoblin, "round-trip: darkwood goblin missing from YAML content (test precondition)")
	require.NotNil(t, yamlGoblin.Living, "round-trip: goblin Living missing in YAML (test precondition)")
	// Assert DB-vs-YAML PARITY, not a magic balance value: the pin names a Living-block drop directly
	// in the failure message while surviving goblin stat retunes (it broke when strength 14 -> 12).
	assert.Equal(t, yamlGoblin.Living.CombatProfile, goblin.Living.CombatProfile, "round-trip: goblin combat_profile")
	assert.Equal(t, yamlGoblin.Living.Attributes["strength"], goblin.Living.Attributes["strength"],
		"round-trip: goblin strength (Living attributes lost on the DB path)")
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

	pk, found, err := content.LoadPack(content.DemoPack)
	require.NoError(t, err)
	require.True(t, found, "embedded demo pack present")

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
		{"region heartlands (Phase 10.3)", regionRefs(lc), "heartlands"},
		{"track hero_advancement (Phase 11.2)", trackRefs(lc), "hero_advancement"},
		{"bundle fighter (Phase 11.4)", bundleRefs(lc), "fighter"},
		{"loot table goblin_loot (Phase 12.1)", lootTableRefs(lc), "goblin_loot"},
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

func regionRefs(lc *content.LoadedContent) []string {
	out := make([]string, 0, len(lc.Regions))
	for _, rg := range lc.Regions {
		out = append(out, rg.Ref)
	}
	return out
}

func trackRefs(lc *content.LoadedContent) []string {
	out := make([]string, 0, len(lc.Tracks))
	for _, tr := range lc.Tracks {
		out = append(out, tr.Ref)
	}
	return out
}

func bundleRefs(lc *content.LoadedContent) []string {
	out := make([]string, 0, len(lc.Bundles))
	for _, bn := range lc.Bundles {
		out = append(out, bn.Ref)
	}
	return out
}

func lootTableRefs(lc *content.LoadedContent) []string {
	out := make([]string, 0, len(lc.LootTables))
	for _, lt := range lc.LootTables {
		out = append(out, lt.Ref)
	}
	return out
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

// TestInstanceableWithdrawalSurvivesAReImport is the DB half of #418's security direction.
//
// #418 made the shard's content snapshot track reloads, so an `instanceable` flip now takes effect on a
// running fleet instead of waiting for a rolling reboot. That is only true if a re-import actually CHANGES
// the stored flag — and `instanceable` rides the `zones.body` JSONB, the column this repo has now shipped
// three separate field-drops through (Round 35's `primary`, Round 11's def-table fields, and `instanceable`
// itself). TestStorePackRoundTrip pins that the flag arrives; nothing pinned that it can be TAKEN AWAY.
//
// The asymmetry matters because the two directions fail differently. A dropped write on the true->true
// path is a dungeon door that stops working — annoying, obvious, reported within the hour. A dropped write
// on the true->FALSE path is a builder revoking the instance opt-in, seeing the deploy succeed, and the
// faucet staying open: a mint runs the zone's boot resets into a private copy a player can strip and walk
// out of, and it routes around whatever in-world gate the revocation was meant to restore. Silent, and in
// the unsafe direction.
//
// It reads through content.LoadPacks, which is exactly what the shard's refresh calls under LoadWithCore.
func TestInstanceableWithdrawalSurvivesAReImport(t *testing.T) {
	p := helpers.OpenTestPool(t)
	ctx := context.Background()

	pk, found, err := content.LoadPack(content.DemoPack)
	require.NoError(t, err)
	require.True(t, found, "embedded demo pack present")

	// v1: as authored. `crypt` is the demo pack's opted-in instance template.
	require.NoError(t, p.ImportPack(ctx, pk), "import v1")
	v1, err := content.Load(ctx, p, []string{content.DemoPack})
	require.NoError(t, err)
	require.NotNil(t, v1.Zone("crypt"))
	require.True(t, v1.Zone("crypt").Instanceable, "precondition: crypt opts in at v1")

	// v2: the builder WITHDRAWS the opt-in and re-imports, the way a real revocation lands.
	withdrawn := pk
	withdrawn.Zones = append([]content.ZoneDTO(nil), pk.Zones...)
	for i := range withdrawn.Zones {
		if withdrawn.Zones[i].Ref == "crypt" {
			withdrawn.Zones[i].Instanceable = false
		}
	}
	require.NoError(t, p.ImportPack(ctx, withdrawn), "import v2 with the opt-in withdrawn")

	v2, err := content.Load(ctx, p, []string{content.DemoPack})
	require.NoError(t, err)
	require.NotNil(t, v2.Zone("crypt"), "crypt still exists at v2")
	assert.False(t, v2.Zone("crypt").Instanceable,
		"a re-import did NOT clear zones.instanceable for crypt: the withdrawal parses from YAML, the import "+
			"reports success, and Postgres keeps serving true — so every shard's content refresh (#418) reads "+
			"the opt-in as still granted and the instance faucet stays open after the builder closed it")
}
