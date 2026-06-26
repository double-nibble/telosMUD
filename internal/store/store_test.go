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
