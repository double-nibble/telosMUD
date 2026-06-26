package world

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// character_test.go is the unit-level proof of the durability ladder with the IN-MEMORY store
// (no Postgres, no Redis): the restart milestone (docs/PHASE4-PLAN.md slice 4.2) made testable.
// It drives the zone actor directly via its inbox — exactly like zone_test.go — and exercises the
// SAME login read seam the gRPC server uses (shard.loadCharacterSnapshot) so the read/write paths
// are the real ones, just without the network.

// persistShard builds a single-zone demo shard backed by an in-memory store+checkpointer and runs
// it (zones + the async saver). Returns the shard, its home zone, and the store for assertions.
func persistShard(t *testing.T) (*Shard, *Zone, *MemStore) {
	t.Helper()
	mem := NewMemStore()
	shard := NewDemoShard().WithPersistence(mem, mem)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(ctx)
	return shard, shard.Zone(), mem
}

// login simulates server.go's Connect for name: it reads the durable snapshot OFF the zone
// goroutine (the real freshness check) and posts an attachMsg carrying it, then waits for the
// player to be registered. Returns the session's out channel so the test can read frames.
func login(t *testing.T, shard *Shard, z *Zone, name string) chan *playv1.ServerFrame {
	t.Helper()
	out := make(chan *playv1.ServerFrame, 64)
	var cz atomic.Pointer[Zone]
	loaded, loadedOK := shard.loadCharacterSnapshot(context.Background(), name)
	z.post(attachMsg{character: name, out: out, curZone: &cz, loaded: loaded, loadedOK: loadedOK})
	waitPlayer(t, z, name, true)
	return out
}

// input posts one command line and gives the zone a moment to apply it (deterministic enough for
// these tests; the zone is single-threaded so a short settle is reliable).
func sendInput(z *Zone, name, line string) {
	z.post(inputMsg{id: name, line: line})
}

// quit posts a clean leave (the logout flush point) and waits for the player to be gone, then for
// the async saver to have written the durable row at least once.
func quit(t *testing.T, z *Zone, name string) {
	t.Helper()
	z.post(leaveMsg{id: name})
	waitPlayer(t, z, name, false)
}

// waitPlayer polls until name is present (want=true) or absent (want=false) in the zone, by asking
// the zone goroutine (a roomQuery message would be ideal, but a short poll over a zone-owned read
// via a synchronous probe keeps the test simple — we use a tiny inbox round-trip).
func waitPlayer(t *testing.T, z *Zone, name string, want bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		got := zoneHasPlayer(z, name)
		if got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("player %q presence = %v, want %v", name, got, want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// zoneProbe asks the zone goroutine for a player's presence + PID readiness, via the synchronous
// presence probe — so the read of z.players is on the owning goroutine (race-free).
func zoneProbe(z *Zone, name string) presence {
	reply := make(chan presence, 1)
	z.post(presenceMsg{id: name, reply: reply})
	return <-reply
}

// zoneHasPlayer reports whether the zone currently holds name.
func zoneHasPlayer(z *Zone, name string) bool { return zoneProbe(z, name).present }

// waitEntityPID waits until the live player's entity has a durable PersistID (the async create
// returned), so a subsequent logout flush actually writes.
func waitEntityPID(t *testing.T, z *Zone, name string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if zoneProbe(z, name).pidSet {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("entity for %q never got a PersistID", name)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// waitRow polls the in-memory store until name has a durable row (the async saver wrote it).
func waitRow(t *testing.T, mem *MemStore, name string) CharSnapshot {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if snap, ok, _ := mem.LoadCharacter(context.Background(), name); ok && snap.PID != "" {
			return snap
		}
		select {
		case <-deadline:
			t.Fatalf("durable row for %q never appeared", name)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// waitRowWhere polls the in-memory durable row until it satisfies pred (e.g. reflects the logout
// room + inventory after the async flush), then returns it.
func waitRowWhere(t *testing.T, mem *MemStore, name string, pred func(CharSnapshot) bool) CharSnapshot {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if snap, ok, _ := mem.LoadCharacter(context.Background(), name); ok && snap.PID != "" && pred(snap) {
			return snap
		}
		select {
		case <-deadline:
			snap, _, _ := mem.LoadCharacter(context.Background(), name)
			t.Fatalf("durable row for %q never matched predicate; last = %+v", name, snap)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestDurabilityLadderSurvivesRestart is the headline slice-4.2 milestone, unit-tested with the
// in-memory store: a player logs in (brand-new => a row is minted), walks north into the market,
// picks up an item, then "logs out" (a clean leave flushes durably). A NEW session for the SAME
// name loads the durable record and is back in the market with the item carried — i.e. the
// character survived a "restart" (a fresh login with no in-memory carry-over).
func TestDurabilityLadderSurvivesRestart(t *testing.T) {
	shard, z, mem := persistShard(t)

	// First life: log in fresh. Brand-new => the row is minted asynchronously (createCharacter
	// posts the PID back); wait for it so the logout flush has a PersistID to CAS.
	out := login(t, shard, z, "Aria")
	waitRow(t, mem, "Aria")
	waitEntityPID(t, z, "Aria")
	drainChan(out)
	sendInput(z, "Aria", "north") // temple -> market (demo pack exit)
	waitFrame(t, out, "Market")   // the market room name
	sendInput(z, "Aria", "get torch")
	waitFrame(t, out, "torch")

	// Log out: the clean leave triggers an immediate durable flush. The flush is async (the saver
	// runs off-goroutine), so wait until the durable row reflects the logout room/inventory.
	quit(t, z, "Aria")
	snap := waitRowWhere(t, mem, "Aria", func(s CharSnapshot) bool {
		return s.RoomRef == "midgaard:room:market" && len(s.State.Inventory) == 1
	})
	if snap.RoomRef != "midgaard:room:market" {
		t.Fatalf("saved room = %q, want the market (where they logged out)", snap.RoomRef)
	}
	if len(snap.State.Inventory) != 1 || snap.State.Inventory[0].ProtoRef != "midgaard:obj:torch" {
		t.Fatalf("saved inventory = %+v, want a single torch", snap.State.Inventory)
	}

	// Second life ("restart"): a fresh login for the same name loads the record. The new session
	// has no in-memory carry-over — it must rehydrate from the store alone.
	out2 := login(t, shard, z, "Aria")
	// The first frame stream after a rehydrated login is the market room (the SAVED room), and
	// "inventory" lists the carried torch.
	waitFrame(t, out2, "Market")
	drainChan(out2)
	sendInput(z, "Aria", "inventory")
	waitFrame(t, out2, "torch")
}

// TestStateVersionCASRejectsStaleSave proves the optimistic-concurrency guard: a save whose
// state_version is behind the stored row loses the CAS (ok=false) and the saver posts a
// saveConflictMsg back to the zone. Driven directly against the store + a hand-built request so
// the CAS semantics are asserted in isolation.
func TestStateVersionCASRejectsStaleSave(t *testing.T) {
	mem := NewMemStore()
	ctx := context.Background()
	pid, err := mem.CreateCharacter(ctx, "Bo", "midgaard", "midgaard:room:temple")
	if err != nil {
		t.Fatal(err)
	}
	// Two writers hold the same base version 0.
	base := CharSnapshot{PID: pid, Name: "Bo", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 0}

	// Writer A wins, bumping the row to version 1.
	newV, ok, err := mem.SaveCharacter(ctx, base)
	if err != nil || !ok {
		t.Fatalf("first save: ok=%v err=%v, want ok", ok, err)
	}
	if newV != 1 {
		t.Fatalf("first save new version = %d, want 1", newV)
	}
	// Writer B (the zombie) still holds base version 0 -> must lose the CAS.
	_, ok, err = mem.SaveCharacter(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("stale save at version 0 must lose the CAS once the row moved to version 1")
	}
}

// TestSaveConflictPostsReconcile proves the async saver posts a saveConflictMsg to the zone when a
// flush loses the CAS, and that the zone's reconcile re-reads the current version. We pre-advance
// the store's row out from under a session, then enqueue a flush at the stale version.
func TestSaveConflictPostsReconcile(t *testing.T) {
	shard, z, mem := persistShard(t)
	out := login(t, shard, z, "Cy")
	waitRow(t, mem, "Cy") // brand-new row minted at version 0
	waitEntityPID(t, z, "Cy")
	drainChan(out)

	// Simulate a zombie writer advancing the durable row far ahead of this session's version.
	snap, _, _ := mem.LoadCharacter(context.Background(), "Cy")
	for i := 0; i < 3; i++ {
		v, ok, _ := mem.SaveCharacter(context.Background(), snap)
		if !ok {
			t.Fatalf("setup advance save lost CAS at version %d", snap.StateVersion)
		}
		snap.StateVersion = v
	}
	advanced := snap.StateVersion

	// Force this session to flush at its (now stale) version 0: the CAS loses, the saver posts
	// saveConflictMsg, and the zone reconciles by re-reading -> the session's version catches up to
	// `advanced`. Repeatedly nudging a flush, we assert the row eventually advances PAST `advanced`
	// — which can only happen once the session reconciled to `advanced` and a later flush's CAS
	// then matched (a stuck-stale session would lose every CAS and never move the row).
	deadline := time.After(3 * time.Second)
	for {
		z.post(drainFlushMsg{}) // each tick: flush at the session's current version
		if v, ok := mem.rowVersion("Cy"); ok && v > advanced {
			break // a flush succeeded => the session had caught up to `advanced` via reconcile
		}
		select {
		case <-deadline:
			t.Fatalf("reconcile after save conflict never let a later flush advance the row past %d", advanced)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestCheckpointFreshnessWins proves the login read picks the HIGHER-state_version source between
// the Postgres row and the Redis checkpoint — the crash-window freshness check. We write a stale
// row and a fresher checkpoint and assert the loaded snapshot is the checkpoint's.
func TestCheckpointFreshnessWins(t *testing.T) {
	mem := NewMemStore()
	shard := NewDemoShard().WithPersistence(mem, mem)
	ctx := context.Background()

	// A durable row at version 2 (the last Postgres flush).
	pid, _ := mem.CreateCharacter(ctx, "Del", "midgaard", "midgaard:room:temple")
	row := CharSnapshot{PID: pid, Name: "Del", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 0}
	for row.StateVersion < 2 {
		v, _, _ := mem.SaveCharacter(ctx, row)
		row.StateVersion = v
	}
	// A FRESHER checkpoint at version 5 in a DIFFERENT room (a more recent ~10s mirror).
	_ = mem.Checkpoint(ctx, CharSnapshot{
		PID: pid, Name: "Del", ZoneRef: "midgaard", RoomRef: "midgaard:room:market", StateVersion: 5,
	})

	snap, ok := shard.loadCharacterSnapshot(ctx, "Del")
	if !ok {
		t.Fatal("expected to load a snapshot")
	}
	if snap.StateVersion != 5 || snap.RoomRef != "midgaard:room:market" {
		t.Fatalf("freshness check chose version %d room %q, want the checkpoint (5, market)",
			snap.StateVersion, snap.RoomRef)
	}

	// Inverse: overwrite the checkpoint with a STALE one (version 1); the row (version 2) now wins.
	_ = mem.Checkpoint(ctx, CharSnapshot{
		PID: pid, Name: "Del", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 1,
	})
	snap2, ok := shard.loadCharacterSnapshot(ctx, "Del")
	if !ok {
		t.Fatal("expected to load a snapshot (inverse)")
	}
	if snap2.StateVersion != 2 || snap2.RoomRef != "midgaard:room:temple" {
		t.Fatalf("freshness check chose version %d room %q, want the row (2, temple)",
			snap2.StateVersion, snap2.RoomRef)
	}
}

// --- test helpers ----------------------------------------------------------------------

// drainChan discards currently-queued frames.
func drainChan(out chan *playv1.ServerFrame) {
	for {
		select {
		case <-out:
		default:
			return
		}
	}
}

// waitFrame waits for an Output frame whose markup contains substr.
func waitFrame(t *testing.T, out chan *playv1.ServerFrame, substr string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for output containing %q", substr)
		}
	}
}
