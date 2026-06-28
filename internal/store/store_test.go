package store

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sync"
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

// TestSaveCharacterConcurrentCAS pins the optimistic-concurrency CAS under genuine CONTENTION (the
// Wave-2 distributed-correctness gap): N goroutines all load the SAME version and fire SaveCharacter
// CONCURRENTLY against the SAME row. The state_version CAS (`UPDATE ... WHERE state_version = $base
// RETURNING`) must let EXACTLY ONE win (ok=true, version -> base+1) and cleanly REJECT all the others
// (ok=false, no error) — the zombie-writer fence holding when two shards (or a shard + a reconnect
// resume) race to flush the same character. TestCharacterCRUD covers the SEQUENTIAL stale-save; this
// adds the genuinely simultaneous race, where Postgres row-locking — not test ordering — picks the
// winner.
//
// Determinism without flakiness: a sync.WaitGroup barrier releases every writer at once so the saves
// truly contend, and we assert on the INVARIANT (exactly one ok, the rest cleanly rejected, no
// errors, final version = base+1), never on WHICH goroutine wins — that is racy by design and not a
// property we pin.
func TestSaveCharacterConcurrentCAS(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	name := "CASRace-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE name = $1`, name)
	})

	pid, err := p.CreateCharacter(ctx, name, "midgaard", "midgaard:room:temple")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Every writer loads the SAME base snapshot (version 0) — the contention precondition: they all
	// believe they hold the current version, exactly like two shards that each loaded the row before
	// either saved.
	base, found, err := p.LoadCharacter(ctx, name)
	if err != nil || !found {
		t.Fatalf("load base: found=%v err=%v", found, err)
	}
	if base.PID != pid || base.StateVersion != 0 {
		t.Fatalf("base snapshot = %+v, want pid=%s version=0", base, pid)
	}

	const writers = 8
	type result struct {
		ok      bool
		version uint64
		err     error
		room    string
	}
	results := make([]result, writers)

	var start sync.WaitGroup
	start.Add(1) // the barrier: all writers block until the test releases them at once.
	var done sync.WaitGroup
	done.Add(writers)

	for i := 0; i < writers; i++ {
		go func(i int) {
			defer done.Done()
			// Each writer saves a DISTINCT room off the same base version, so the winner is identifiable
			// (its room is the one that lands) and a lost writer cannot accidentally match the winner.
			snap := base
			snap.RoomRef = roomForWriter(i)
			start.Wait() // released simultaneously below
			v, ok, err := p.SaveCharacter(ctx, snap)
			results[i] = result{ok: ok, version: v, err: err, room: snap.RoomRef}
		}(i)
	}

	start.Done() // fire all writers at once: the CAS now genuinely contends on the single row.
	done.Wait()

	// INVARIANT 1: exactly one writer won the CAS; every other was cleanly rejected (ok=false, no error).
	wins, winner := 0, -1
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("writer %d returned an ERROR (a lost CAS must be ok=false, not an error): %v", i, r.err)
		}
		if r.ok {
			wins++
			winner = i
			if r.version != 1 {
				t.Fatalf("winning writer %d bumped version to %d, want 1 (base 0 + 1)", i, r.version)
			}
		}
	}
	if wins != 1 {
		t.Fatalf("CAS contention: %d writers won, want EXACTLY 1 (the zombie-writer fence let multiple saves through)", wins)
	}

	// INVARIANT 2: the durable row reflects the WINNER and is at version 1 — no lost-update, no
	// double-bump from a leaked second writer.
	final, _, err := p.LoadCharacter(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if final.StateVersion != 1 {
		t.Fatalf("final version = %d, want 1 (exactly one CAS committed)", final.StateVersion)
	}
	if final.RoomRef != results[winner].room {
		t.Fatalf("final room = %q, want the winner's room %q (the wrong writer's state landed)", final.RoomRef, results[winner].room)
	}
}

// roomForWriter maps a writer index to a distinct, valid room ref so the CAS winner is identifiable
// by the room that lands durably. The rooms are real demo rooms (FK-safe is irrelevant here —
// characters.room_ref is a free-form ref, not an FK to rooms in this schema).
func roomForWriter(i int) string {
	rooms := []string{
		"midgaard:room:temple", "midgaard:room:market",
		"darkwood:room:grove", "darkwood:room:hollow",
		"darkwood:room:lair", "midgaard:room:temple",
		"midgaard:room:market", "darkwood:room:grove",
	}
	return rooms[i%len(rooms)]
}
