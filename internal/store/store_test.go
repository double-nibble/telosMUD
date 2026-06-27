package store

import (
	"context"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/db"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/world"
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
			// nil-vs-empty distinction that is not a content difference. Collapsing both
			// to nil keeps the guard catching REAL content drift (a flag that exists in
			// one path and not the other) while ignoring the empty-slice representation.
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

// store_test.go holds the GATED integration tests against a real Postgres. They require a
// TELOS_TEST_DSN pointing at a throwaway database and t.Skip when it is unset — so a local
// `go test ./...` with no database passes, while CI (or a dev who exports the DSN) runs them.
// Each test migrates the schema and works in its own pack / name space, cleaning up after itself,
// so they are safe to run repeatedly against the same database.

// testPool opens the gated test database (skipping the test when TELOS_TEST_DSN is unset),
// migrates the schema, and returns a live pool. The migration is idempotent (goose tracks
// applied versions), so running every gated test migrates once and re-runs are no-ops.
func testPool(t *testing.T) *Pool {
	t.Helper()
	dsn := os.Getenv("TELOS_TEST_DSN")
	if dsn == "" {
		t.Skip("TELOS_TEST_DSN not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	p, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestStorePackRoundTrip is the 4.1 carry-forward: import the embedded demo pack into Postgres,
// LoadPacks it back, and assert the assembled LoadedContent equals what the embedded loader
// produces directly — i.e. the DB path and the YAML path agree byte-for-byte (the parity guard,
// now exercised through real SQL rather than only the in-memory loader).
func TestStorePackRoundTrip(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	data, err := content.DemoPackBytes()
	if err != nil {
		t.Fatal(err)
	}
	pk, err := content.ParsePack(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ImportPack(ctx, pk); err != nil {
		t.Fatalf("import demo pack: %v", err)
	}

	// Load from Postgres and from the embedded YAML, and compare the assembled content.
	fromDB, err := content.Load(ctx, p, []string{content.DemoPack})
	if err != nil {
		t.Fatalf("load from postgres: %v", err)
	}
	fromYAML, err := content.LoadDemoPack()
	if err != nil {
		t.Fatalf("load embedded: %v", err)
	}
	// Compare order-insensitively: the DB returns rows ORDER BY ref, the YAML keeps authoring
	// order, so normalize both before DeepEqual (the content, not the slice order, is the contract).
	dbZones := normalizeContent(fromDB.Zones)
	yamlZones := normalizeContent(fromYAML.Zones)
	if !reflect.DeepEqual(dbZones, yamlZones) {
		t.Fatalf("round-trip mismatch:\n DB  = %+v\n YAML= %+v", dbZones, yamlZones)
	}

	// Explicitly pin the combat-mob Living round-trip (Phase 6.3a): the darkwood goblin's Living block
	// (its stat sheet + `melee` profile ref) must survive the mob_prototypes.body JSONB trip. Before
	// the protoBody.Living fix this came back nil from the DB path and the DeepEqual above failed; this
	// guard names the regression directly so a re-break reports "goblin Living dropped", not a diff dump.
	goblin := findMob(dbZones, "darkwood:mob:goblin")
	if goblin == nil {
		t.Fatal("round-trip: darkwood goblin mob missing from DB-loaded content")
	}
	if goblin.Living == nil {
		t.Fatal("round-trip: goblin Living block was DROPPED on the DB path (mob stat sheet lost)")
	}
	if goblin.Living.CombatProfile != "melee" {
		t.Fatalf("round-trip: goblin combat_profile = %q, want 'melee'", goblin.Living.CombatProfile)
	}
	if goblin.Living.Attributes["strength"] != 14 {
		t.Fatalf("round-trip: goblin strength = %v, want 14 (Living attributes lost)", goblin.Living.Attributes["strength"])
	}

	// Explicitly pin the room-scoped affect round-trip (Phase 6.4a, [G13]): the `web` affect's top-level
	// Scope ("room") must survive the DB path. It is a pack-GLOBAL def (not zone content), so the zone
	// DeepEqual above does NOT cover it — exactly the class of gap that hid the mob-Living and (earlier)
	// the ability_defs drops. AffectDTO.Scope is a first-class field mapped to runtime affectDef.roomScoped
	// (content_map.go: Scope=="room"); before the affect_defs.scope column it loaded as "" => roomScoped
	// false, so `web` would attach to one entity instead of the room. We assert the DB-loaded value equals
	// the embedded-YAML value AND is "room" — a re-break drops it back to "" and this fails directly.
	yamlWeb := findAffect(fromYAML.Affects, "web")
	dbWeb := findAffect(fromDB.Affects, "web")
	if yamlWeb == nil {
		t.Fatal("round-trip: 'web' affect missing from embedded YAML content (test precondition)")
	}
	if yamlWeb.Scope != "room" {
		t.Fatalf("round-trip: embedded 'web' affect scope = %q, want 'room' (test precondition)", yamlWeb.Scope)
	}
	if dbWeb == nil {
		t.Fatal("round-trip: 'web' affect missing from DB-loaded content")
	}
	if dbWeb.Scope != "room" {
		t.Fatalf("round-trip: 'web' affect scope was DROPPED on the DB path: got %q, want 'room' "+
			"(room-scoped affect would attach to one entity instead of the room)", dbWeb.Scope)
	}
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

// TestImportPackIdempotent pins the seed/import idempotency contract (the deletePack regression). A
// pack re-import is meant to STRIP the pack's prior rows in one transaction, then re-insert — so
// running `make seed` / `make up` twice against a populated database replaces content rather than
// colliding. The bug: deletePack cleared attribute/resource/damage_type/affect defs but OMITTED
// ability_defs, so the SECOND import failed on "duplicate key value violates unique constraint
// ability_defs_pkey" (e.g. fireball). It survived several slices because it only reproduced against
// REAL Postgres on a RE-import — exactly the gap a single-import or in-memory test cannot see. This
// test imports the demo pack twice and asserts the second succeeds with content intact.
func TestImportPackIdempotent(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	data, err := content.DemoPackBytes()
	if err != nil {
		t.Fatal(err)
	}
	pk, err := content.ParsePack(data)
	if err != nil {
		t.Fatal(err)
	}

	if err := p.ImportPack(ctx, pk); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// THE REGRESSION: the second import must strip-and-replace, not collide on a duplicate key. Before
	// the deletePack fix this returned the ability_defs_pkey violation.
	if err := p.ImportPack(ctx, pk); err != nil {
		t.Fatalf("second import must be idempotent (strip-and-replace), got: %v", err)
	}

	// Content intact after the re-import: every global def kind loads back, and the ability that
	// triggered the original bug is present exactly once (the table deletePack must clear).
	lc, err := content.Load(ctx, p, []string{content.DemoPack})
	if err != nil {
		t.Fatalf("load after re-import: %v", err)
	}
	fireballs := 0
	for _, ab := range lc.Abilities {
		if ab.Ref == "fireball" {
			fireballs++
		}
	}
	if fireballs != 1 {
		t.Fatalf("after re-import: found %d 'fireball' abilities, want exactly 1", fireballs)
	}
	if len(lc.Attributes) == 0 || len(lc.Resources) == 0 || len(lc.Affects) == 0 {
		t.Fatalf("after re-import: global defs missing (attrs=%d resources=%d affects=%d)",
			len(lc.Attributes), len(lc.Resources), len(lc.Affects))
	}

	// Combat content (Phase 6.3a) must survive the re-import exactly like the abilities above: the
	// `melee` profile present exactly once (deletePack strips combat_profile_defs so a second import
	// strips-and-replaces, never collides on the duplicate ref), and the pack's default_combat scalar
	// (pack_meta) intact. This is the combat-content extension of the deletePack idempotency contract.
	melees := 0
	for _, cp := range lc.CombatProfiles {
		if cp.Ref == "melee" {
			melees++
		}
	}
	if melees != 1 {
		t.Fatalf("after re-import: found %d 'melee' combat profiles, want exactly 1", melees)
	}
	if lc.DefaultCombat != "melee" {
		t.Fatalf("after re-import: default_combat = %q, want 'melee'", lc.DefaultCombat)
	}

	// Room-scoped affect (Phase 6.4a, [G13]): the `web` affect's scope must survive the re-import too —
	// the affect_defs.scope column is overwritten (not collided) by the strip-and-replace, so a second
	// import keeps scope='room' rather than reverting to the 'entity' default.
	if web := findAffect(lc.Affects, "web"); web == nil {
		t.Fatal("after re-import: 'web' affect missing")
	} else if web.Scope != "room" {
		t.Fatalf("after re-import: 'web' affect scope = %q, want 'room'", web.Scope)
	}
}

// TestCharacterCRUD exercises the pgx CharacterStore against a real database: create mints a
// PersistID and a version-0 row; load returns it; the state_version CAS bumps on a matching save
// and REJECTS a stale one; and the round-tripped state JSONB (inventory) survives the trip.
func TestCharacterCRUD(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	name := "GatedTestChar-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		// Hard-delete the row so re-runs start clean (the CITEXT name is UNIQUE).
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE name = $1`, name)
	})

	pid, err := p.CreateCharacter(ctx, name, "midgaard", "midgaard:room:temple")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if pid == "" {
		t.Fatal("create returned an empty PersistID")
	}

	snap, found, err := p.LoadCharacter(ctx, name)
	if err != nil || !found {
		t.Fatalf("load after create: found=%v err=%v", found, err)
	}
	if snap.PID != pid || snap.RoomRef != "midgaard:room:temple" || snap.StateVersion != 0 {
		t.Fatalf("loaded snapshot = %+v, want pid=%s room=temple version=0", snap, pid)
	}

	// Save with the matching version: CAS succeeds, version -> 1, and the inventory persists.
	snap.RoomRef = "midgaard:room:market"
	snap.State.AppliedSeq = 7
	snap.State.Inventory = []world.ItemJSON{{ProtoRef: "midgaard:obj:torch"}}
	newV, ok, err := p.SaveCharacter(ctx, snap)
	if err != nil || !ok {
		t.Fatalf("save (matching version): ok=%v err=%v, want ok", ok, err)
	}
	if newV != 1 {
		t.Fatalf("save bumped version to %d, want 1", newV)
	}

	reloaded, _, err := p.LoadCharacter(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.StateVersion != 1 || reloaded.RoomRef != "midgaard:room:market" {
		t.Fatalf("reloaded = %+v, want version=1 room=market", reloaded)
	}
	if len(reloaded.State.Inventory) != 1 || reloaded.State.Inventory[0].ProtoRef != "midgaard:obj:torch" {
		t.Fatalf("reloaded inventory = %+v, want a single torch", reloaded.State.Inventory)
	}
	if reloaded.State.AppliedSeq != 7 {
		t.Fatalf("reloaded applied_seq = %d, want 7", reloaded.State.AppliedSeq)
	}

	// A STALE save (still holding version 0) must LOSE the CAS — the zombie-writer fence.
	stale := snap
	stale.StateVersion = 0
	_, ok, err = p.SaveCharacter(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("stale save at version 0 must lose the CAS after the row moved to version 1")
	}

	// Loading an unknown name is found=false, not an error.
	_, found, err = p.LoadCharacter(ctx, "definitely-no-such-character-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("loading an unknown name must report found=false")
	}
}
