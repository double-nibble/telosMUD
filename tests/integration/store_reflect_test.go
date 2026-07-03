package integration

import (
	"context"
	"reflect"
	"strconv"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// store_reflect_test.go — the reflect-walk store round-trip net (docs/REMAINING.md Track 0). The existing
// TestStorePackRoundTrip DeepEquals the DEMO pack through Postgres, but its documented blind spot is that a
// field NO demo content populates with a non-zero value drops invisibly (the canonicalize strip folds
// zero-vs-dropped on both sides). This closes that: it reflect-fills a SYNTHETIC pack so EVERY field of every
// persisted global-def kind is non-zero, round-trips it through ImportPack → Load, and asserts each def kind
// comes back intact. A body struct (internal/store: lootTableBody, recipeBody, …) that forgets to carry a new
// DTO field then shows as present-in-expected / absent-in-actual — a failing test the moment the field is added,
// with no per-field maintenance.
//
// Scope: the 15 pack-GLOBAL def kinds, which are FK-free (import.go: "no FK into the zone tree") so a synthetic
// pack with no zones imports cleanly. Zone content (rooms/exits carry cross-room FKs; mob/item protos) stays
// covered by TestStorePackRoundTrip's demo DeepEqual. Commands / PvpLua / Formulas are deliberately excluded —
// they are NOT persisted through ImportPack/Load today (a separate gap, not a field-drop this net can assert).

// fillNonZero recursively sets v to a DISTINCTIVE non-zero value so that a field dropped on the store round-trip
// (which comes back as its zero value) is detectable, and a field SWAP between two same-typed fields is too
// (each scalar gets a unique value from counter). An `any`/interface field gets a JSON-stable probe map: those
// op-list/formula fields round-trip opaquely through the JSONB body, so a drop of the WHOLE field still shows,
// while filling them with a concrete shape can't misfire. depth caps any unexpected recursive type.
func fillNonZero(v reflect.Value, counter *int, depth int) {
	if depth > 20 || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		*counter++
		v.SetString(sentinelString(*counter))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		*counter++
		v.SetInt(int64(*counter))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		*counter++
		v.SetUint(uint64(*counter)) //nolint:gosec // the sentinel counter is small and monotonically positive
	case reflect.Float32, reflect.Float64:
		*counter++
		v.SetFloat(float64(*counter) + 0.5)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillNonZero(v.Elem(), counter, depth+1)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(s.Index(0), counter, depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		fillNonZero(key, counter, depth+1)
		val := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(val, counter, depth+1)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fillNonZero(v.Field(i), counter, depth+1)
		}
	case reflect.Interface:
		// An `any` field (op-list / formula body): a JSON-stable non-empty value that round-trips through
		// the opaque JSONB body and survives the canonicalize strip. float64 so it matches post-JSON.
		*counter++
		v.Set(reflect.ValueOf(map[string]any{"probe": float64(*counter)}))
	}
}

// sentinelString returns a deterministic non-empty string for scalar slot n.
func sentinelString(n int) string { return "probe_" + strconv.Itoa(n) }

// fillOneDef appends a single fully-populated synthetic def to the slice pointed to by ptr (a *[]SomeDTO).
func fillOneDef(ptr any, counter *int) {
	rv := reflect.ValueOf(ptr).Elem()
	elem := reflect.New(rv.Type().Elem()).Elem()
	fillNonZero(elem, counter, 0)
	rv.Set(reflect.Append(rv, elem))
}

// TestStoreDTOReflectRoundTrip is the Track-0 regression net: a fully-populated synthetic pack proves every
// field of every persisted global-def kind survives the Postgres import/load path — closing the "field no demo
// content sets drops invisibly" blind spot of TestStorePackRoundTrip.
func TestStoreDTOReflectRoundTrip(t *testing.T) {
	p := helpers.OpenTestPool(t)
	ctx := context.Background()

	counter := 0
	pk := content.Pack{Pack: "reflectprobe"}
	// One fully-populated synthetic def per persisted global-def kind. (Zones omitted: cross-room exit FKs;
	// covered by TestStorePackRoundTrip. Commands/PvpLua/Formulas omitted: not DB-persisted.)
	fillOneDef(&pk.Attributes, &counter)
	fillOneDef(&pk.Resources, &counter)
	fillOneDef(&pk.DamageTypes, &counter)
	fillOneDef(&pk.Affects, &counter)
	fillOneDef(&pk.Abilities, &counter)
	fillOneDef(&pk.CombatProfiles, &counter)
	fillOneDef(&pk.Channels, &counter)
	fillOneDef(&pk.Regions, &counter)
	fillOneDef(&pk.Tracks, &counter)
	fillOneDef(&pk.Bundles, &counter)
	fillOneDef(&pk.RarityTiers, &counter)
	fillOneDef(&pk.LootTables, &counter)
	fillOneDef(&pk.SpawnSchedules, &counter)
	fillOneDef(&pk.Recipes, &counter)
	fillOneDef(&pk.Chargens, &counter)
	fillOneDef(&pk.DisplayDefs, &counter)
	fillOneDef(&pk.TrustTiers, &counter)

	require.NoError(t, p.ImportPack(ctx, pk), "import synthetic reflect-fill pack")
	lc, err := content.Load(ctx, p, []string{pk.Pack})
	require.NoError(t, err, "load synthetic pack from postgres")

	cases := []struct {
		name string
		in   any // the synthetic (expected) def slice
		out  any // the DB-loaded (actual) def slice
	}{
		{"attributes", pk.Attributes, lc.Attributes},
		{"resources", pk.Resources, lc.Resources},
		{"damage_types", pk.DamageTypes, lc.DamageTypes},
		{"affects", pk.Affects, lc.Affects},
		{"abilities", pk.Abilities, lc.Abilities},
		{"combat_profiles", pk.CombatProfiles, lc.CombatProfiles},
		{"channels", pk.Channels, lc.Channels},
		{"regions", pk.Regions, lc.Regions},
		{"tracks", pk.Tracks, lc.Tracks},
		{"bundles", pk.Bundles, lc.Bundles},
		{"rarity_tiers", pk.RarityTiers, lc.RarityTiers},
		{"loot_tables", pk.LootTables, lc.LootTables},
		{"spawn_schedules", pk.SpawnSchedules, lc.SpawnSchedules},
		{"recipes", pk.Recipes, lc.Recipes},
		{"chargens", pk.Chargens, lc.Chargens},
		{"display_defs", pk.DisplayDefs, lc.DisplayDefs},
		{"trust_tiers", pk.TrustTiers, lc.TrustTiers},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// canonicalizeDefs (from store_pack_test.go) folds the two load paths' int/float + nil/empty
			// representation quirks; because the synthetic def is ALL non-zero, a dropped field is
			// present-in-expected / absent-in-actual after the strip — a clean, field-naming failure.
			require.Equalf(t, canonicalizeDefs(t, tc.in), canonicalizeDefs(t, tc.out),
				"reflect round-trip: a field of the %s DTO was dropped or mis-serialized on the store "+
					"import/load path (its store body struct likely omits the field)", tc.name)
		})
	}
}

// reflectNetCovered lists the content.Pack def-slice fields TestStoreDTOReflectRoundTrip fills + round-trips.
// It is the single source of truth for coverage, checked against content.Pack by the drift guard below.
var reflectNetCovered = map[string]bool{
	"Attributes": true, "Resources": true, "DamageTypes": true, "Affects": true,
	"Abilities": true, "CombatProfiles": true, "Channels": true, "Regions": true,
	"Tracks": true, "Bundles": true, "RarityTiers": true, "LootTables": true,
	"SpawnSchedules": true, "Recipes": true, "Chargens": true, "DisplayDefs": true,
	"TrustTiers": true,
}

// reflectNetExcluded lists the content.Pack def-slice fields deliberately NOT in the reflect net, each with
// its reason — so an exclusion is a documented choice, not an oversight.
var reflectNetExcluded = map[string]string{
	"Zones":    "cross-room exit FKs make an all-non-zero synthetic zone tree un-importable; the zone tree is covered by TestStorePackRoundTrip's demo DeepEqual",
	"Commands": "custom Lua verbs are NOT persisted through ImportPack/Load today (the Commands/PvpLua/Formulas store gap — see docs/REMAINING.md)",
}

// TestStoreReflectNetCoversEveryDefSlice is a hermetic DRIFT GUARD (no Postgres): it asserts the reflect
// round-trip net exercises EVERY []XxxDTO def slice on content.Pack, or explicitly excludes it with a reason.
// Without this, a future def kind added to Pack + wired through import/load but forgotten in the net above
// would go silently uncovered — the exact silent-coverage regression this net exists to prevent, one level up.
func TestStoreReflectNetCoversEveryDefSlice(t *testing.T) {
	pt := reflect.TypeOf(content.Pack{})
	var uncovered []string
	for i := 0; i < pt.NumField(); i++ {
		f := pt.Field(i)
		// A def-slice field is a []T where T is a struct (skips []string, scalars, maps).
		if f.Type.Kind() != reflect.Slice || f.Type.Elem().Kind() != reflect.Struct {
			continue
		}
		if reflectNetCovered[f.Name] {
			continue
		}
		if _, ok := reflectNetExcluded[f.Name]; ok {
			continue
		}
		uncovered = append(uncovered, f.Name)
	}
	require.Emptyf(t, uncovered,
		"content.Pack def slice(s) %v are neither exercised by TestStoreDTOReflectRoundTrip nor in "+
			"reflectNetExcluded — add each to the reflect round-trip net (or exclude it with a reason) so a "+
			"new def kind cannot silently lose store round-trip coverage", uncovered)
}
