package store

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/db"
	"github.com/double-nibble/telosmud/internal/world"
)

// store_test.go holds the GATED Postgres integration test(s) that MUST stay co-located with the
// store package because they reach into UNEXPORTED internals. The black-box, exported-API-only
// integration tests (the pack round-trip and re-import idempotency) live under tests/integration/
// per the project TEST STANDARD (see docs/TESTING.md). TestCharacterCRUD stays here because its
// cleanup pokes p.pool directly — moving it to a black-box package would mean exporting plumbing
// that exists only for the test.
//
// These require a TELOS_TEST_DSN pointing at a throwaway database and t.Skip when it is unset — so
// a local `go test ./...` with no database passes, while CI (or a dev who exports the DSN, as
// `make test-integration` does) runs them. Each test migrates the schema and works in its own
// name space, cleaning up after itself, so it is safe to run repeatedly against the same database.

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
	// Phase 7.6: a populated self.state Script subtree must survive the real-PG JSONB round-trip
	// (same whole-blob path as Inventory; this closes the Script-specific coverage gap).
	snap.State.Script = json.RawMessage(`{"q":1}`)
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
	// Phase 7.6: the self.state Script subtree survived the JSONB round-trip. Compare SEMANTICALLY, not
	// byte-for-byte: Postgres JSONB canonicalizes whitespace (it returns `{"q": 1}`, a space after the
	// colon), which is irrelevant — the real load path (unmarshalLuaState → json.Unmarshal) is
	// whitespace-insensitive. A byte assertion would be a false failure on JSONB's canonical form.
	var gotScript, wantScript map[string]any
	if err := json.Unmarshal(reloaded.State.Script, &gotScript); err != nil {
		t.Fatalf("reloaded self.state Script not valid JSON: %v (%s)", err, reloaded.State.Script)
	}
	_ = json.Unmarshal([]byte(`{"q":1}`), &wantScript)
	if !reflect.DeepEqual(gotScript, wantScript) {
		t.Fatalf("reloaded self.state Script = %s, want {\"q\":1} (semantically)", reloaded.State.Script)
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
