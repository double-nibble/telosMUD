package store

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/content"
)

// reservedRefTables have a bare `ref` primary key and a `pack` column, but NOTHING imports into them — they
// were created so the schema is whole and are documented RESERVED in db/migrations/00003_ability_tables.sql.
// A ref that is never written cannot collide, so they are correctly absent from refOwnerTables. Listed here
// rather than silently skipped so that if either ever starts being imported, the parity test below fails and
// forces the decision.
var reservedRefTables = map[string]bool{"class_defs": true, "race_defs": true}

// TestRefOwnerTablesMatchTheSchema is the maintenance guard the refOwnerTables comment promises. The
// collision check is only as good as its table list, and the failure mode of a stale list is SILENT: a new
// definition table would simply go unchecked and reintroduce the raw duplicate-key error #366 is about.
//
// It asks Postgres directly which tables can actually collide — a bare `ref` primary key plus a `pack`
// column — and requires the list to match exactly. Adding a definition table now fails here until the
// author either includes it or makes it collision-proof by putting `pack` in the primary key.
//
// Gated on TELOS_TEST_DSN like the rest of the Postgres tier; CI's integration job runs it.
func TestRefOwnerTablesMatchTheSchema(t *testing.T) {
	p := testPool(t)
	rows, err := p.pool.Query(context.Background(), `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
		JOIN pg_index i ON i.indrelid = c.oid AND i.indisprimary
		WHERE EXISTS (SELECT 1 FROM pg_attribute p WHERE p.attrelid = c.oid AND p.attname = 'pack' AND p.attnum > 0)
		  AND array_length(i.indkey, 1) = 1
		  AND (SELECT a.attname FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum = i.indkey[0]) = 'ref'
		ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()

	var inSchema []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		if reservedRefTables[name] {
			continue
		}
		inSchema = append(inSchema, name)
	}
	require.NoError(t, rows.Err())

	covered := make([]string, 0, len(refOwnerTables))
	for _, t := range refOwnerTables {
		covered = append(covered, t.table)
	}
	sort.Strings(covered)
	sort.Strings(inSchema)

	require.Equal(t, inSchema, covered,
		"refOwnerTables must list EXACTLY the tables that can collide across packs (a bare `ref` primary key "+
			"plus a `pack` column). A table in the schema but not the list goes unchecked, and the seed→pull "+
			"collision comes back as a raw duplicate-key error; a table in the list but not the schema is dead "+
			"weight. If you added a definition table, either add it here or give it a (pack, ref) primary key.")
}

// TestIncomingRefsCoversEveryOwnedTable pins the OTHER half: knowing which tables can collide is useless if
// the import's refs are never extracted for them. incomingRefs must produce entries for every table in
// refOwnerTables when handed a pack that populates all of them — otherwise the query loop skips that table
// (`len(list) == 0` → continue) and the check silently passes.
func TestIncomingRefsCoversEveryOwnedTable(t *testing.T) {
	pk := content.Pack{
		Pack: "p",
		Zones: []content.ZoneDTO{{
			Ref: "z", Rooms: []content.RoomDTO{{Ref: "z:room:a"}},
			Items: []content.ProtoDTO{{Ref: "z:obj:a"}}, Mobs: []content.ProtoDTO{{Ref: "z:mob:a"}},
		}},
		Attributes:     []content.AttributeDTO{{Ref: "attr"}},
		Resources:      []content.ResourceDTO{{Ref: "res"}},
		DamageTypes:    []content.DamageTypeDTO{{Ref: "dmg"}},
		Affects:        []content.AffectDTO{{Ref: "aff"}},
		Abilities:      []content.AbilityDTO{{Ref: "abil"}},
		CombatProfiles: []content.CombatProfileDTO{{Ref: "cp"}},
		Channels:       []content.ChannelDTO{{Ref: "chan"}},
		ToggleDefs:     []content.ToggleDTO{{Ref: "tog"}},
		Regions:        []content.RegionDTO{{Ref: "reg"}},
		Tracks:         []content.TrackDTO{{Ref: "trk"}},
		Bundles:        []content.BundleDTO{{Ref: "bun"}},
		RarityTiers:    []content.RarityTierDTO{{Ref: "rar"}},
		LootTables:     []content.LootTableDTO{{Ref: "loot"}},
		SpawnSchedules: []content.SpawnScheduleDTO{{Ref: "spawn"}},
		Recipes:        []content.RecipeDTO{{Ref: "rec"}},
		HelpDefs:       []content.HelpDTO{{Ref: "help"}},
		Chargens:       []content.ChargenDTO{{Ref: "chargen"}},
	}
	got := incomingRefs([]content.Pack{pk})
	for _, tbl := range refOwnerTables {
		require.NotEmpty(t, got[tbl.table],
			"incomingRefs extracts no refs for %q, so the collision check skips that table entirely", tbl.table)
	}
}

// TestIncomingRefsSkipsBlankRefs keeps a pack with an empty/whitespace ref from turning the `ref = ANY($1)`
// probe into a match against unrelated blank rows.
func TestIncomingRefsSkipsBlankRefs(t *testing.T) {
	got := incomingRefs([]content.Pack{{
		Pack:       "p",
		Zones:      []content.ZoneDTO{{Ref: "  "}},
		Attributes: []content.AttributeDTO{{Ref: ""}},
	}})
	require.Empty(t, got["zones"])
	require.Empty(t, got["attribute_defs"])
}
