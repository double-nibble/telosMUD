package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/db"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/world"
	"github.com/double-nibble/telosmud/tests/dblock"
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
	// Phase 8.6: the receiver-side comms-state subtree (channel toggles + ignore list + AFK) must survive
	// the same real-PG JSONB round-trip — the persistence done-when (comms state survives logout/login +
	// crash-rehydrate, since StateJSON is the durable form).
	snap.State.Comms = &world.CommsStateJSON{
		Channels: map[string]bool{"gossip": false, "newbie": true},
		Ignore:   []string{"Spammer", "Troll"},
		AFK:      true,
		AFKMsg:   "back soon",
	}
	res, err := p.SaveCharacter(ctx, snap)
	if err != nil || res.Outcome != world.SaveApplied {
		t.Fatalf("save (matching version): outcome=%v err=%v, want applied", res.Outcome, err)
	}
	if res.NewVersion != 1 {
		t.Fatalf("save bumped version to %d, want 1", res.NewVersion)
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

	// Phase 8.6: the comms-state subtree survived the JSONB round-trip (channel toggles + ignore + AFK).
	if reloaded.State.Comms == nil {
		t.Fatal("reloaded comms-state subtree is nil; it did not survive the round-trip")
	}
	if on, ok := reloaded.State.Comms.Channels["gossip"]; !ok || on {
		t.Fatalf("reloaded gossip override = (%v,%v), want (false,true)", on, ok)
	}
	if reloaded.State.Comms.AFK != true || reloaded.State.Comms.AFKMsg != "back soon" {
		t.Fatalf("reloaded AFK = (%v,%q), want (true,\"back soon\")", reloaded.State.Comms.AFK, reloaded.State.Comms.AFKMsg)
	}
	if len(reloaded.State.Comms.Ignore) != 2 {
		t.Fatalf("reloaded ignore list = %v, want 2 entries", reloaded.State.Comms.Ignore)
	}

	// A STALE save (still holding version 0) must LOSE the CAS — the zombie-writer fence. It carries the
	// same owner epoch as the winner, so the refusal is the state_version contention miss specifically.
	stale := snap
	stale.StateVersion = 0
	res, err = p.SaveCharacter(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != world.SaveStaleVersion {
		t.Fatalf("stale save at version 0 must lose the CAS after the row moved to version 1: outcome=%v", res.Outcome)
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
		outcome world.SaveOutcome
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
			res, err := p.SaveCharacter(ctx, snap)
			results[i] = result{outcome: res.Outcome, version: res.NewVersion, err: err, room: snap.RoomRef}
		}(i)
	}

	start.Done() // fire all writers at once: the CAS now genuinely contends on the single row.
	done.Wait()

	// INVARIANT 1: exactly one writer won the CAS; every other was cleanly rejected (a refusal outcome, no
	// error). All writers share the base snapshot's owner epoch, so the losers lose on state_version.
	wins, winner := 0, -1
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("writer %d returned an ERROR (a lost CAS must be a refusal outcome, not an error): %v", i, r.err)
		}
		if r.outcome == world.SaveApplied {
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

// TestRecipeBodyTrackRoundTrips pins that the recipe `track` field survives the recipe_defs JSONB body
// round-trip (docs/REMAINING.md §4) — a hermetic guard against the store field-drop class (the same class
// the reflect-walk DTO round-trip test in §7 will generalize). The import (marshal) and export (unmarshal)
// paths share this recipeBody, so pinning its round-trip catches a mistyped/missing json tag.
func TestRecipeBodyTrackRoundTrips(t *testing.T) {
	in := recipeBody{
		Name: "Leather Vest", Aliases: []string{"vest", "leather vest"}, // #34 discovery/alias fields
		Profession: "smith", Track: "smithing", Skill: "raw", MinSkill: 3, Station: "forge", QualityBase: 5,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out recipeBody
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Track != "smithing" {
		t.Fatalf("recipe track dropped in body round-trip: got %q", out.Track)
	}
	if out.Name != "Leather Vest" || len(out.Aliases) != 2 {
		t.Fatalf("#34 recipe name/aliases dropped in body round-trip: name=%q aliases=%v", out.Name, out.Aliases)
	}
}

// TestWearSlotBodyRoundTrips pins that the #35 wear-slot label/order/kind survive the wear_slot_defs JSONB
// body round-trip — the same hermetic field-drop guard as the recipe body (the import marshal + export
// unmarshal share this struct, so a mistyped/missing json tag shows here).
func TestWearSlotBodyRoundTrips(t *testing.T) {
	in := wearSlotBody{Label: "wielded", Order: 50, Kind: "wield"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out wearSlotBody
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("wearSlotBody round-trip changed the value: %+v -> %+v", in, out)
	}
}

// TestAffixBodyRoundTrips pins that the #37 named-affix attr/min/max survive the affix_defs JSONB body
// round-trip — the hermetic field-drop guard (import marshal + export unmarshal share this struct).
func TestAffixBodyRoundTrips(t *testing.T) {
	in := affixBody{Attr: "strength", Min: 1, Max: 5}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out affixBody
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("affixBody round-trip changed the value: %+v -> %+v", in, out)
	}
}

// TestBundleBodyUncappedRoundTrips pins that the profession `uncapped` flag survives the bundle_defs JSONB
// body round-trip (docs/REMAINING.md §4) — the store field-drop class. Import (marshal) + export (unmarshal)
// share this bundleBody, so pinning its round-trip catches a mistyped/missing json tag.
func TestBundleBodyUncappedRoundTrips(t *testing.T) {
	in := bundleBody{Kind: "profession", Uncapped: true, Grants: nil}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out bundleBody
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Uncapped {
		t.Fatalf("bundle uncapped flag dropped in body round-trip: %+v", out)
	}
}

// TestChannelBodyHearAccessRoundTrips pins that the split hear_access predicate survives the
// channel_defs JSONB body round-trip (docs/REMAINING.md Track 10) — the store field-drop class. The
// nil-vs-EMPTY distinction is LOAD-BEARING (nil = hear mirrors speak; empty = anyone hears, the
// announce shape), so both shapes are pinned: nil stays absent/nil, a present-but-zero pointer
// survives as present-but-zero.
func TestChannelBodyHearAccessRoundTrips(t *testing.T) {
	// Present-but-empty (the announce channel) survives as present-but-empty.
	in := channelBody{
		Name: "Announce", Access: content.ChannelAccessDTO{RequireFlag: "immortal"},
		HearAccess: &content.ChannelAccessDTO{},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out channelBody
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.HearAccess == nil {
		t.Fatal("present-but-empty hear_access became nil in the body round-trip (announce shape lost)")
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("channelBody round-trip changed the value: %+v -> %+v", in, out)
	}

	// Absent (nil) stays nil — the v1 hear-mirrors-speak rule must not become an open channel.
	b2, err := json.Marshal(channelBody{Name: "Guild", Access: content.ChannelAccessDTO{RequireFlag: "guildmember"}})
	if err != nil {
		t.Fatal(err)
	}
	var out2 channelBody
	if err := json.Unmarshal(b2, &out2); err != nil {
		t.Fatal(err)
	}
	if out2.HearAccess != nil {
		t.Fatalf("nil hear_access became non-nil in the body round-trip: %+v", out2.HearAccess)
	}
}

// TestResourceBodyGaugeRoundTrips pins that the #50 gauge flag survives the resource_defs JSONB body
// round-trip (the store field-drop class). Import (marshal) + export (unmarshal) share resourceBody, so
// a mistyped/missing json tag would silently drop a pool's HUD-visibility on a DB reload.
func TestResourceBodyGaugeRoundTrips(t *testing.T) {
	// Gauge (#50) and Primary (#71) both ride the JSONB body — pin that neither is dropped, the def-table
	// field-drop class the demo caught before (attrBody.Stat / the round-11 store trap).
	in := resourceBody{Regen: 1, PerRound: true, Gauge: true, Primary: true}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out resourceBody
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Gauge {
		t.Fatalf("resource gauge flag dropped in body round-trip: %+v", out)
	}
	if !out.Primary {
		t.Fatalf("resource primary flag (#71) dropped in body round-trip: %+v", out)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("resourceBody round-trip changed the value: %+v -> %+v", in, out)
	}
}

// TestLootTableBodyOnRollRoundTrips pins that the loot on_roll Lua hatch survives the loot_table_defs JSONB
// body round-trip (docs/REMAINING.md §4) — the store field-drop class. Import (marshal) + export (unmarshal)
// share this lootTableBody, so pinning its round-trip catches a mistyped/missing json tag on the new field.
func TestLootTableBodyOnRollRoundTrips(t *testing.T) {
	in := lootTableBody{OnRoll: `return {"x:item"}`}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out lootTableBody
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.OnRoll != in.OnRoll {
		t.Fatalf("loot on_roll dropped in body round-trip: got %q", out.OnRoll)
	}
}

// TestImportPacksAtomic (gated) proves ImportPacks (#212 slice 3) is CROSS-PACK atomic: importing a
// batch where a later pack fails rolls back the earlier packs too, so Postgres never lands at a torn
// half-version. It also confirms a clean multi-pack batch imports both packs.
func TestImportPacksAtomic(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := testPool(t)
	ctx := context.Background()

	suffix := time.Now().Format("150405.000000")
	packA := "pkA-" + suffix
	packB := "pkB-" + suffix
	zoneA := "za" + suffix
	zoneB := "zb" + suffix
	t.Cleanup(func() {
		cctx := context.Background()
		tx, err := p.pool.Begin(cctx)
		if err != nil {
			return
		}
		defer tx.Rollback(cctx) //nolint:errcheck // no-op after Commit
		for _, pk := range []string{packA, packB} {
			_ = deletePack(cctx, tx, pk)
		}
		_ = tx.Commit(cctx)
	})

	good := func(pack, zone string) content.Pack {
		lit := 1.0
		return content.Pack{
			Pack: pack,
			Zones: []content.ZoneDTO{{
				Ref: zone, Name: "Z", StartRoom: zone + ":room:1",
				Rooms: []content.RoomDTO{{Ref: zone + ":room:1", Name: "One"}},
			}},
			// A pack-global def too, so the test proves def-table rows (inserted before a later pack
			// fails) also roll back — the store field-drop-trap zone this PR defends.
			Attributes: []content.AttributeDTO{{
				Ref: zone + ":attr:hp", DisplayName: "HP", ValueKind: "int",
				DefaultBase: content.BaseSpecDTO{Lit: &lit},
			}},
		}
	}
	// A pack whose SECOND room duplicates the first room's ref -> the second INSERT violates the rooms
	// PK, failing importPackTx for this pack AFTER the earlier pack was already inserted in the same tx.
	broken := content.Pack{Pack: packB, Zones: []content.ZoneDTO{{
		Ref: zoneB, Name: "Z", StartRoom: zoneB + ":room:1",
		Rooms: []content.RoomDTO{{Ref: zoneB + ":room:1", Name: "One"}, {Ref: zoneB + ":room:1", Name: "Dup"}},
	}}}

	// A duplicate pack name in the batch is rejected outright (else the second's delete would silently
	// drop the first's rows).
	if err := p.ImportPacks(ctx, []content.Pack{good(packA, zoneA), good(packA, zoneB)}); err == nil {
		t.Fatal("ImportPacks with a duplicate pack name should be rejected")
	}
	if n := countRows(t, p, "zones", packA); n != 0 {
		t.Fatalf("a rejected duplicate-name batch must write nothing, found %d zone rows", n)
	}

	// Atomicity: [good A, broken B] must fail AND leave NEITHER pack in the DB — zones AND def rows.
	if err := p.ImportPacks(ctx, []content.Pack{good(packA, zoneA), broken}); err == nil {
		t.Fatal("ImportPacks with a broken pack should fail")
	}
	if n := countRows(t, p, "zones", packA); n != 0 {
		t.Fatalf("pack A zones must have rolled back with the failed batch, found %d", n)
	}
	if n := countRows(t, p, "attribute_defs", packA); n != 0 {
		t.Fatalf("pack A def rows must have rolled back with the failed batch, found %d", n)
	}
	if n := countRows(t, p, "zones", packB); n != 0 {
		t.Fatalf("broken pack B must not persist, found %d zone rows", n)
	}

	// Happy path: a clean multi-pack batch imports both, zones AND def rows.
	if err := p.ImportPacks(ctx, []content.Pack{good(packA, zoneA), good(packB, zoneB)}); err != nil {
		t.Fatalf("clean multi-pack import: %v", err)
	}
	if n := countRows(t, p, "zones", packA); n != 1 {
		t.Fatalf("pack A zone missing after clean import, found %d", n)
	}
	if n := countRows(t, p, "attribute_defs", packA); n != 1 {
		t.Fatalf("pack A def row missing after clean import, found %d", n)
	}
	if n := countRows(t, p, "zones", packB); n != 1 {
		t.Fatalf("pack B zone missing after clean import, found %d", n)
	}
}

func countRows(t *testing.T, p *Pool, table, pack string) int {
	t.Helper()
	var n int
	// table is a test-controlled literal, never user input.
	q := "SELECT count(*) FROM " + table + " WHERE pack=$1"
	if err := p.pool.QueryRow(context.Background(), q, pack).Scan(&n); err != nil {
		t.Fatalf("count %s for %s: %v", table, pack, err)
	}
	return n
}

// TestImportVersionConcurrentSameSHA (gated, #230) pins the leader-failover backstop DIRECTLY: two-plus
// CONCURRENT ImportVersion calls with the SAME content SHA — a stale director racing the promoted one —
// serialize on the content_version `SELECT ... FOR UPDATE` lock, so EXACTLY ONE reports changed=true and
// the version bumps once; the losers see the SHA already present and no-op. TestImportVersion covers the
// SEQUENTIAL idempotency (a redelivery); this asserts the concurrent race it was only reasoned about.
func TestImportVersionConcurrentSameSHA(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := testPool(t)
	ctx := context.Background()

	suffix := time.Now().Format("150405.000000")
	pack := "cpk-" + suffix
	zone := "cz" + suffix
	t.Cleanup(func() {
		cctx := context.Background()
		tx, err := p.pool.Begin(cctx)
		if err != nil {
			return
		}
		defer tx.Rollback(cctx) //nolint:errcheck
		_ = deletePack(cctx, tx, pack)
		_, _ = tx.Exec(cctx, `DELETE FROM content_pack_registry WHERE pack=$1`, pack)
		_ = tx.Commit(cctx)
	})
	good := content.Pack{Pack: pack, Zones: []content.ZoneDTO{{
		Ref: zone, Name: "Z", StartRoom: zone + ":room:1",
		Rooms: []content.RoomDTO{{Ref: zone + ":room:1", Name: "One"}},
	}}}
	sha := "csha-" + suffix

	const n = 4
	var wg sync.WaitGroup
	changed := make([]bool, n)
	versions := make([]uint64, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all at once so they contend on the FOR UPDATE lock
			v, _, ch, err := p.ImportVersion(ctx, []content.Pack{good},
				VersionMeta{ContentSHA: sha, ManifestVersion: "v1", ContentHash: "h1"})
			versions[i], changed[i], errs[i] = v, ch, err
		}(i)
	}
	close(start)
	wg.Wait()

	nChanged := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent import %d errored: %v", i, errs[i])
		}
		if changed[i] {
			nChanged++
		}
	}
	if nChanged != 1 {
		t.Fatalf("exactly ONE concurrent import of the same SHA must report changed=true, got %d", nChanged)
	}
	// Every caller settles on the single committed version: the one importer that bumped it, and the losers
	// that serialized after and saw the SHA already present.
	info, err := p.CurrentContentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range versions {
		if v != info.Version {
			t.Fatalf("import %d returned version %d, want the single settled version %d", i, v, info.Version)
		}
	}
}

// TestBumpContentVersion (gated, #232) proves the shard-local reload's DURABLE version mint: each bump
// atomically increments the single content_version authority (monotonic, no wall clock), CONCURRENT bumps
// never collide (N goroutines yield N distinct consecutive versions and advance the counter by exactly N —
// the atomicity a clock-free reload racing a pull relies on), and a bump touches ONLY version, never the
// published-content identity (content_sha / manifest_version / the pack registry).
func TestBumpContentVersion(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := testPool(t)
	ctx := context.Background()

	base, err := p.ContentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := p.BumpContentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v1 != base+1 {
		t.Fatalf("first bump = %d, want base+1 = %d", v1, base+1)
	}
	if v2, err := p.BumpContentVersion(ctx); err != nil || v2 != base+2 {
		t.Fatalf("second bump = %d (err %v), want base+2 = %d", v2, err, base+2)
	}

	// A bump moves ONLY the version — a reload re-materializes the same rows, so the published identity holds.
	before, err := p.CurrentContentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.BumpContentVersion(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := p.CurrentContentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.ContentSHA != before.ContentSHA || after.ManifestVersion != before.ManifestVersion {
		t.Fatalf("bump changed the content identity: sha %q->%q, manifest %q->%q",
			before.ContentSHA, after.ContentSHA, before.ManifestVersion, after.ManifestVersion)
	}
	if after.Version != before.Version+1 {
		t.Fatalf("bump version = %d, want %d (+1)", after.Version, before.Version+1)
	}

	// Concurrency: N atomic bumps yield N DISTINCT consecutive versions and advance by exactly N (no lost
	// update) — the row-lock serialization a reload racing another reload / a pull depends on.
	start, err := p.ContentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	got := make([]uint64, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = p.BumpContentVersion(ctx)
		}(i)
	}
	wg.Wait()

	seen := map[uint64]bool{}
	for i, v := range got {
		if errs[i] != nil {
			t.Fatalf("concurrent bump %d errored: %v", i, errs[i])
		}
		if v <= start || v > start+n {
			t.Fatalf("concurrent bump %d = %d, want in (%d, %d]", i, v, start, start+n)
		}
		if seen[v] {
			t.Fatalf("duplicate version %d across concurrent bumps — a lost update / non-atomic increment", v)
		}
		seen[v] = true
	}
	if final, err := p.ContentVersion(ctx); err != nil || final != start+n {
		t.Fatalf("after %d concurrent bumps, version = %d (err %v), want %d", n, final, err, start+n)
	}
}

// TestImportVersion (gated) exercises the #212 slice-4 versioned import: the monotonic version bump,
// the pack registry, and orphan pruning across versions.
func TestImportVersion(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := testPool(t)
	ctx := context.Background()

	suffix := time.Now().Format("150405.000000")
	packA := "vpkA-" + suffix
	packB := "vpkB-" + suffix
	zoneA := "vza" + suffix
	zoneB := "vzb" + suffix
	t.Cleanup(func() {
		cctx := context.Background()
		tx, err := p.pool.Begin(cctx)
		if err != nil {
			return
		}
		defer tx.Rollback(cctx) //nolint:errcheck
		for _, pk := range []string{packA, packB} {
			_ = deletePack(cctx, tx, pk)
			_, _ = tx.Exec(cctx, `DELETE FROM content_pack_registry WHERE pack=$1`, pk)
		}
		_ = tx.Commit(cctx)
	})
	good := func(pack, zone string) content.Pack {
		return content.Pack{Pack: pack, Zones: []content.ZoneDTO{{
			Ref: zone, Name: "Z", StartRoom: zone + ":room:1",
			Rooms: []content.RoomDTO{{Ref: zone + ":room:1", Name: "One"}},
		}}}
	}

	// Version 1: [A, B].
	v1, pruned, changed1, err := p.ImportVersion(ctx, []content.Pack{good(packA, zoneA), good(packB, zoneB)},
		VersionMeta{ContentSHA: "sha1-" + suffix, ManifestVersion: "v1", ContentHash: "h1"})
	if err != nil {
		t.Fatalf("import v1: %v", err)
	}
	if v1 == 0 {
		t.Fatal("version must be non-zero after an import")
	}
	if len(pruned) != 0 {
		t.Fatalf("first import should prune nothing, got %v", pruned)
	}
	if !changed1 {
		t.Fatal("a first real import must report changed=true")
	}
	info, err := p.CurrentContentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != v1 || info.ContentSHA != "sha1-"+suffix || info.ManifestVersion != "v1" {
		t.Fatalf("content_version stamp wrong: %+v (want version=%d sha1 v1)", info, v1)
	}
	if !containsAll(info.Packs, packA, packB) || len(info.Packs) != 2 {
		t.Fatalf("registry after v1 = %v, want {%s,%s}", info.Packs, packA, packB)
	}

	// Version 2: [A] — drops B, which must be pruned and its rows gone.
	v2, pruned, changed2, err := p.ImportVersion(ctx, []content.Pack{good(packA, zoneA)},
		VersionMeta{ContentSHA: "sha2-" + suffix, ManifestVersion: "v2", ContentHash: "h2"})
	if err != nil {
		t.Fatalf("import v2: %v", err)
	}
	if v2 <= v1 {
		t.Fatalf("version must be monotonic: v2=%d <= v1=%d", v2, v1)
	}
	if len(pruned) != 1 || pruned[0] != packB {
		t.Fatalf("v2 should prune [%s], got %v", packB, pruned)
	}
	if n := countRows(t, p, "zones", packB); n != 0 {
		t.Fatalf("dropped pack B's rows must be pruned, found %d zone rows", n)
	}
	if n := countRows(t, p, "zones", packA); n != 1 {
		t.Fatalf("kept pack A must still be present, found %d zone rows", n)
	}
	info, _ = p.CurrentContentVersion(ctx)
	if len(info.Packs) != 1 || info.Packs[0] != packA || info.Version != v2 {
		t.Fatalf("registry after v2 = %+v, want {%s} at v2=%d", info, packA, v2)
	}

	// Idempotency (the leader-failover invariant): re-importing the SAME SHA is a no-op — the version
	// must NOT bump and nothing is pruned, so a redelivery can't inflate the version / force a reconcile.
	v2again, pruned2, changed2b, err := p.ImportVersion(ctx, []content.Pack{good(packA, zoneA)},
		VersionMeta{ContentSHA: "sha2-" + suffix, ManifestVersion: "v2", ContentHash: "h2"})
	if err != nil {
		t.Fatalf("idempotent re-import: %v", err)
	}
	if v2again != v2 {
		t.Fatalf("re-importing the same SHA must not bump the version: got %d, want %d", v2again, v2)
	}
	if len(pruned2) != 0 {
		t.Fatalf("idempotent re-import should prune nothing, got %v", pruned2)
	}
	if changed2b {
		t.Fatal("re-importing the same SHA must report changed=false (so the caller skips the broadcast)")
	}

	// A DIFFERENT SHA for the same pack set DOES bump (content changed even if the pack list didn't).
	v3, _, changed3, err := p.ImportVersion(ctx, []content.Pack{good(packA, zoneA)},
		VersionMeta{ContentSHA: "sha3-" + suffix, ManifestVersion: "v3", ContentHash: "h3"})
	if err != nil {
		t.Fatalf("import v3: %v", err)
	}
	if v3 <= v2 {
		t.Fatalf("a new SHA must bump the version: v3=%d <= v2=%d", v3, v2)
	}
	if !changed2 || !changed3 {
		t.Fatalf("real imports must report changed=true (v2=%v v3=%v)", changed2, changed3)
	}
}

// TestPackZones (gated) covers the PR-E2 prune-guard lookup: PackZones returns exactly a pack's zone refs
// (sorted), and an empty list for a pack that owns no zones.
func TestPackZones(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := testPool(t)
	ctx := context.Background()

	suffix := time.Now().Format("150405.000000")
	pack := "pzpk-" + suffix
	z1 := "pza" + suffix
	z2 := "pzb" + suffix
	t.Cleanup(func() {
		cctx := context.Background()
		tx, err := p.pool.Begin(cctx)
		if err != nil {
			return
		}
		defer tx.Rollback(cctx) //nolint:errcheck
		_ = deletePack(cctx, tx, pack)
		_, _ = tx.Exec(cctx, `DELETE FROM content_pack_registry WHERE pack=$1`, pack)
		_ = tx.Commit(cctx)
	})

	twoZones := content.Pack{Pack: pack, Zones: []content.ZoneDTO{
		{Ref: z2, Name: "Two", StartRoom: z2 + ":room:1", Rooms: []content.RoomDTO{{Ref: z2 + ":room:1", Name: "R"}}},
		{Ref: z1, Name: "One", StartRoom: z1 + ":room:1", Rooms: []content.RoomDTO{{Ref: z1 + ":room:1", Name: "R"}}},
	}}
	if _, _, _, err := p.ImportVersion(ctx, []content.Pack{twoZones},
		VersionMeta{ContentSHA: "pzsha-" + suffix, ManifestVersion: "v1", ContentHash: "pzh"}); err != nil {
		t.Fatalf("import: %v", err)
	}

	zones, err := p.PackZones(ctx, pack)
	if err != nil {
		t.Fatal(err)
	}
	// z1 < z2 lexically (…a… < …b…), so the sorted result is [z1, z2] regardless of authoring order.
	if len(zones) != 2 || zones[0] != z1 || zones[1] != z2 {
		t.Fatalf("PackZones(%s) = %v, want sorted [%s %s]", pack, zones, z1, z2)
	}

	// A pack that was never imported owns no zones.
	empty, err := p.PackZones(ctx, "no-such-pack-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("PackZones for an unknown pack = %v, want empty", empty)
	}
}

func containsAll(xs []string, want ...string) bool {
	set := map[string]bool{}
	for _, x := range xs {
		set[x] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// TestCurrentContentVersionNoRow (gated, #246) pins the ErrNoContentVersion sentinel: a database with NO
// content_version row (a truly fresh/uninitialized state) returns the sentinel — distinct from a real read
// error — so telos-account can bootstrap on the demo default there yet still fail closed on a genuine error.
// Gated store tests run serially, and the singleton is restored under defer, so deleting it here is safe.
func TestCurrentContentVersionNoRow(t *testing.T) {
	dblock.LockContentRegistry(t)
	p := testPool(t)
	ctx := context.Background()

	// A migrated DB seeds content_version id=1 (version 0): the normal path returns no error.
	if _, err := p.CurrentContentVersion(ctx); err != nil {
		t.Fatalf("a migrated DB has the seeded singleton, want no error, got %v", err)
	}

	// Snapshot then delete the singleton to exercise the no-row path; restore it afterward for sibling tests.
	var saved int64
	if err := p.pool.QueryRow(ctx, `SELECT version FROM content_version WHERE id = 1`).Scan(&saved); err != nil {
		t.Fatalf("read seeded version: %v", err)
	}
	if _, err := p.pool.Exec(ctx, `DELETE FROM content_version WHERE id = 1`); err != nil {
		t.Fatalf("delete singleton: %v", err)
	}
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `INSERT INTO content_version (id, version) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET version = EXCLUDED.version`, saved)
	})

	// With no row, the read returns the SENTINEL (not a generic wrapped error), so a caller can tell
	// "fresh DB, demo is correct" from a real failure.
	_, err := p.CurrentContentVersion(ctx)
	if !errors.Is(err, ErrNoContentVersion) {
		t.Fatalf("no content_version row must return ErrNoContentVersion, got %v", err)
	}
}

// TestSaveCharacterEmptyZoneRefPreservesTheStoredZone pins the "" contract at the DURABLE sink (#411): an
// empty ZoneRef means "leave zone_ref alone", never "write SQL NULL over it".
//
// The world's only producer of ZoneRef (world.dumpCharacter) returns "" for a player who is inside a
// runtime-minted zone INSTANCE, whose ephemeral id must never be persisted. RoomRef is NOT empty on that
// path — an instance hosts its TEMPLATE's authored rooms — so a clearing write leaves the row internally
// inconsistent: a real room ref with no zone. On reconnect the world's ZoneRef != "" guard is false, it falls
// back to the home zone, cannot resolve that room there, and start-rooms the player. The ordinary save
// cadence does it to every dungeon occupant, and so does the drain's flush on every SIGTERM.
func TestSaveCharacterEmptyZoneRefPreservesTheStoredZone(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()

	name := "GatedZoneRefChar-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE name = $1`, name)
	})

	// The entrance anchor: where the player was before they stepped into the instance.
	if _, err := p.CreateCharacter(ctx, name, "midgaard", "midgaard:room:temple"); err != nil {
		t.Fatalf("create: %v", err)
	}
	snap, found, err := p.LoadCharacter(ctx, name)
	if err != nil || !found {
		t.Fatalf("load after create: found=%v err=%v", found, err)
	}

	// The instance-occupant save: no zone, but a real (template-authored) room.
	snap.ZoneRef, snap.RoomRef = "", "darkwood:room:lair"
	if res, err := p.SaveCharacter(ctx, snap); err != nil || res.Outcome != world.SaveApplied {
		t.Fatalf("save: outcome=%v err=%v", res.Outcome, err)
	}
	reloaded, _, err := p.LoadCharacter(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ZoneRef != "midgaard" {
		t.Fatalf("an empty ZoneRef wrote SQL NULL over zone_ref (now %q, was midgaard) while room_ref kept %q. "+
			"The row is a real room with no zone: the reconnect falls back to the home zone, cannot resolve that "+
			"room there, and start-rooms the player — permanent durable location loss for every instance "+
			"occupant on every save tick (#411)", reloaded.ZoneRef, reloaded.RoomRef)
	}
	if reloaded.RoomRef != "darkwood:room:lair" {
		t.Fatalf("room_ref = %q, want darkwood:room:lair — the preserve must be zone_ref ONLY", reloaded.RoomRef)
	}

	// The CONTROL: a real zone change is still written, so "" is the only preserving value.
	reloaded.ZoneRef, reloaded.RoomRef = "crypt", "crypt:room:entrance"
	if res, err := p.SaveCharacter(ctx, reloaded); err != nil || res.Outcome != world.SaveApplied {
		t.Fatalf("save (zone change): outcome=%v err=%v", res.Outcome, err)
	}
	if again, _, _ := p.LoadCharacter(ctx, name); again.ZoneRef != "crypt" {
		t.Fatalf("a real zone change was not written: zone_ref = %q, want crypt", again.ZoneRef)
	}
}
