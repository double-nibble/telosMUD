package content

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// merge_test.go — the refactor safety net for the #423 read/merge split.
//
// #423 needed to VALIDATE a shard's content packs between reading them and publishing the merged result,
// which meant cutting content.Load in two: LoadPacks (+ the boot lints) and Merge. The merge loop is ~220
// lines of per-kind last-write-wins bookkeeping across two dozen definition kinds, and this repo has been
// bitten repeatedly by a definition FIELD silently dropping out of a store/merge path with a green suite —
// the failure mode is invisible precisely because nothing compares whole structs.
//
// So the load-bearing test here is not "does Merge work", it is "is Merge STILL Load": a whole-struct
// DeepEqual between the two halves and the original one-shot call, over content rich enough that a dropped
// kind or a mis-ordered override would show.

// TestNewLoadEqualsTheFrozenPreRefactorLoad is the anti-field-drop guard.
//
// It compares the CURRENT Load against loadOLD — the pre-refactor loader, frozen verbatim in
// loadoracle_test.go. That independence is the whole point: the first version of this test compared Load
// against Merge, which after the refactor are the same code, so it passed even with `packs = nil` inserted
// at the top of Merge. A kind or field dropped during the extraction fails HERE, against an oracle the
// change cannot move.
//
// DeepEqual over the whole struct, including the unexported byRef index, which same-package placement buys.
func TestNewLoadEqualsTheFrozenPreRefactorLoad(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		src     Source
		enabled []string
	}{
		{"rich fixture", staticSource(richMergeFixture()), []string{"base", "override"}},
		{"embedded demo", EmbeddedSource{}, []string{DemoPack}},
		{"embedded core", EmbeddedSource{}, []string{CorePack}},
		{"embedded core+demo", EmbeddedSource{}, []string{CorePack, DemoPack}},
		{"nil source", nil, []string{DemoPack}},
		{"nil enabled", EmbeddedSource{}, nil},
		{"empty enabled", EmbeddedSource{}, []string{}},
		{"unknown pack", EmbeddedSource{}, []string{"nonexistent"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, errOld := loadOLD(ctx, tc.src, tc.enabled)
			got, errNew := Load(ctx, tc.src, tc.enabled)
			require.Equal(t, errOld, errNew)
			require.True(t, reflect.DeepEqual(want, got),
				"Load diverged from the frozen pre-refactor loader — a definition kind or field was dropped "+
					"in the read/merge split\nold: %+v\nnew: %+v", want, got)
		})
	}
}

// TestLoadWithCoreEqualsTheFrozenPreRefactorPath does the same for the core-layered read, which took a
// different route after the split (it no longer goes through Load at all). Covers a nil source, which is
// the degraded-but-bootable path.
func TestLoadWithCoreEqualsTheFrozenPreRefactorPath(t *testing.T) {
	ctx := context.Background()
	for _, enabled := range [][]string{nil, {}, {DemoPack}, {CorePack}, {CorePack, DemoPack}, {"nonexistent"}} {
		for _, src := range []Source{EmbeddedSource{}, nil} {
			// The pre-refactor LoadWithCore, inlined: Load over the core-layered source.
			want, errOld := loadOLD(ctx, coreLayered{delegate: src}, append([]string{CorePack}, enabled...))
			got, errNew := LoadWithCore(ctx, src, enabled)
			require.Equal(t, errOld, errNew)
			require.True(t, reflect.DeepEqual(want, got), "LoadWithCore diverged for enabled=%v src=%T", enabled, src)
		}
	}
}

// TestLoadPacksWithCoreIsTheSameReadLoadWithCoreMerges pins the OTHER half of the split: the packs the
// snapshot refresh validates must be the packs LoadWithCore would have merged, in the same order (core
// first, so real content overrides it by ref). If these diverge, the #423 gate would be validating
// something other than what it publishes — the exact TOCTOU the single-read design exists to prevent.
func TestLoadPacksWithCoreIsTheSameReadLoadWithCoreMerges(t *testing.T) {
	ctx := context.Background()
	packs, err := LoadPacksWithCore(ctx, EmbeddedSource{}, []string{DemoPack})
	require.NoError(t, err)
	require.NotEmpty(t, packs)
	require.Equal(t, CorePack, packs[0].Pack, "core must be layered FIRST so real packs override it by ref")

	viaLoadWithCore, err := LoadWithCore(ctx, EmbeddedSource{}, []string{DemoPack})
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(viaLoadWithCore, Merge(packs)),
		"LoadPacksWithCore+Merge must equal LoadWithCore")
}

// TestLoadPacksWithCoreYieldsCoreAloneForANilSource covers the degraded-but-bootable path: with Postgres
// unreachable the caller still gets the embedded core pack, so the split cannot regress the guarantee that
// a backing-store-less server boots a start room rather than an empty, login-rejecting world.
func TestLoadPacksWithCoreYieldsCoreAloneForANilSource(t *testing.T) {
	packs, err := LoadPacksWithCore(context.Background(), nil, []string{"whatever"})
	require.NoError(t, err)
	require.Len(t, packs, 1)
	require.Equal(t, CorePack, packs[0].Pack)
}

// richMergeFixture builds two packs that between them touch every merge rule the loop implements: a zone
// override, a pack-global last-write-wins override, a distinct-ref accumulate, an append-only kind, a
// last-non-empty-scalar, and a map merge. It is deliberately broad — the point is coverage of the KINDS,
// so a field dropped during the extraction has somewhere to show up.
func richMergeFixture() []Pack {
	base := Pack{
		Pack: "base",
		Zones: []ZoneDTO{
			{
				Ref: "z1", Name: "Zone One", StartRoom: "z1:room:a", Rooms: []RoomDTO{
					{Ref: "z1:room:a", Name: "A", Long: "long a", Exits: map[string]string{"north": "z1:room:b"}},
					{Ref: "z1:room:b", Name: "B"},
				}, Items: []ProtoDTO{{Ref: "z1:obj:torch", Short: "a torch", Keywords: []string{"torch"}}},
				Mobs:   []ProtoDTO{{Ref: "z1:mob:rat", Short: "a rat", Keywords: []string{"rat"}}},
				Resets: []ResetDTO{{Op: "spawn_mob", Room: "z1:room:a", Proto: "z1:mob:rat"}},
			},
			{Ref: "z2", Name: "Zone Two", StartRoom: "z2:room:a", Rooms: []RoomDTO{{Ref: "z2:room:a", Name: "A2"}}},
		},
		Attributes:     []AttributeDTO{{Ref: "str", DisplayName: "Strength"}, {Ref: "dex", DisplayName: "Dexterity"}},
		Resources:      []ResourceDTO{{Ref: "hp", DisplayName: "Health", Vital: true}},
		DamageTypes:    []DamageTypeDTO{{Ref: "slash", DisplayName: "Slashing"}},
		Affects:        []AffectDTO{{Ref: "poison", Name: "Poison"}},
		Abilities:      []AbilityDTO{{Ref: "kick", Name: "Kick"}},
		CombatProfiles: []CombatProfileDTO{{Ref: "basic"}},
		DefaultCombat:  "basic",
		Channels:       []ChannelDTO{{Ref: "gossip", Name: "Gossip", Words: []string{"gossip"}}},
		ToggleDefs:     []ToggleDTO{{Ref: "brief", Words: []string{"brief"}}},
		Regions:        []RegionDTO{{Ref: "r1", Name: "Region One", Zones: []string{"z1"}}},
		Tracks:         []TrackDTO{{Ref: "level"}},
		Bundles:        []BundleDTO{{Ref: "warrior"}},
		RarityTiers:    []RarityTierDTO{{Ref: "common"}},
		Affixes:        []AffixDefDTO{{Ref: "sharp"}},
		LootTables:     []LootTableDTO{{Ref: "rats"}},
		SpawnSchedules: []SpawnScheduleDTO{{Ref: "weekly"}},
		Recipes:        []RecipeDTO{{Ref: "bread"}},
		HelpDefs:       []HelpDTO{{Ref: "combat"}},
		WearSlots:      []WearSlotDTO{{Ref: "head"}},
		Chargens:       []ChargenDTO{{Ref: "standard"}},
		TrustTiers:     []TrustTierDTO{{Name: "player"}, {Name: "builder"}},
		Commands:       []CommandDTO{{Verb: "wave"}},
		DisplayDefs:    []DisplayDefDTO{{Surface: "score"}},
		Formulas:       map[string]string{"hit": "return 1"},
		PvpLua:         "return false",
		WorldScript:    "-- base",
	}
	// override re-declares a subset by the SAME refs (last write wins), adds new refs (accumulate), and
	// re-sets the last-non-empty scalars — so ordering bugs in the extraction surface as a wrong winner.
	override := Pack{
		Pack: "override",
		Zones: []ZoneDTO{
			{Ref: "z1", Name: "Zone One Revised", StartRoom: "z1:room:b", Rooms: []RoomDTO{{Ref: "z1:room:b", Name: "B revised"}}},
			{Ref: "z3", Name: "Zone Three", Rooms: []RoomDTO{{Ref: "z3:room:a", Name: "A3"}}},
		},
		Attributes:  []AttributeDTO{{Ref: "str", DisplayName: "Might"}, {Ref: "int", DisplayName: "Intellect"}},
		Resources:   []ResourceDTO{{Ref: "mana", DisplayName: "Mana"}},
		Channels:    []ChannelDTO{{Ref: "gossip", Name: "Chat", Words: []string{"chat"}}},
		TrustTiers:  []TrustTierDTO{{Name: "builder", Rank: 1}},
		Commands:    []CommandDTO{{Verb: "bow"}},
		DisplayDefs: []DisplayDefDTO{{Surface: "room"}},
		Formulas:    map[string]string{"hit": "return 2", "dodge": "return 3"},
		PvpLua:      "return true",
		WorldScript: "-- override",
	}
	return []Pack{base, override}
}
