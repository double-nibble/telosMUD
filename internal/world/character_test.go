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
	loaded, loadedOK, _ := shard.loadCharacterSnapshot(context.Background(), name)
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
	res, err := mem.SaveCharacter(ctx, base)
	if err != nil || res.Outcome != SaveApplied {
		t.Fatalf("first save: outcome=%v err=%v, want applied", res.Outcome, err)
	}
	if res.NewVersion != 1 {
		t.Fatalf("first save new version = %d, want 1", res.NewVersion)
	}
	// Writer B (the zombie) still holds base version 0 -> must lose the CAS. Both writers carry the
	// same (zero) owner epoch, so the ownership fence is satisfied and the refusal is specifically
	// the state_version contention miss, not SaveNotOwner.
	res, err = mem.SaveCharacter(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != SaveStaleVersion {
		t.Fatalf("stale save at version 0 must lose the CAS once the row moved to version 1: outcome=%v", res.Outcome)
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

	// Simulate a zombie writer advancing the durable row ahead of this session's version. RE-READ the
	// current version and retry on each CAS miss: the session's own background machinery (the
	// post-create flush, the cadence saver) writes "Cy" concurrently, so a setup that assumed a fixed
	// starting version would intermittently lose its first CAS — a flaky setup under -race/-count. We
	// drive the row to a comfortable margin (>=4) above the session's version (0-2) so the session is
	// reliably STALE for the reconcile assertion below.
	var advanced uint64
	setupDeadline := time.Now().Add(3 * time.Second)
	for advanced < 4 {
		if cur, found, _ := mem.LoadCharacter(context.Background(), "Cy"); found {
			if res, _ := mem.SaveCharacter(context.Background(), cur); res.Outcome == SaveApplied {
				advanced = res.NewVersion
			}
		}
		if time.Now().After(setupDeadline) {
			t.Fatalf("setup could not advance the durable row to version >=4 (reached %d)", advanced)
		}
	}

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

// TestLogoutFlushSurvivesCASConflict proves the durability-ladder fix for a logout flush that
// LOSES its state_version CAS to a concurrent (cadence) flush. Because the session is REMOVED in the
// same leave() that enqueued the flush, a CAS miss cannot be bounced back to a live session to
// re-dump (Zone.saveConflict would find no session). The logout flush is therefore enqueued as
// saveFinal, and the saver itself re-reads the current version, rebases this authoritative snapshot,
// and retries the CAS — so the durable record ends on the player's LOGOUT room, never stranded at a
// pre-move room a racing cadence write happened to win with.
//
// We force the conflict deterministically: log in, walk to the market, then advance the durable row
// OUT FROM UNDER the session (a stand-in for a cadence flush that won the CAS first at the old
// version), and only then quit. The first logout CAS (at the session's stale version) loses; the
// saver's saveFinal retry must still land market.
func TestLogoutFlushSurvivesCASConflict(t *testing.T) {
	shard, z, mem := persistShard(t)

	out := login(t, shard, z, "Quincy")
	waitRow(t, mem, "Quincy")
	waitEntityPID(t, z, "Quincy")
	drainChan(out)
	sendInput(z, "Quincy", "north") // temple -> market
	waitFrame(t, out, "Market")

	// A concurrent writer (think: a cadence flush) advances the durable row past the session's
	// version, so the logout flush's CAS at the session's version is guaranteed to MISS. We do NOT
	// change the room here — the point is that the LOGOUT snapshot (market) must still win after the
	// saver re-reads and rebases, not be lost to whatever room the row currently holds (temple).
	// Re-read + retry on a CAS miss: the session's own background saver writes "Quincy" concurrently,
	// so a fixed-version save would intermittently lose this setup CAS (a flaky setup under -race/-count).
	setupDeadline := time.Now().Add(3 * time.Second)
	for {
		if cur, found, _ := mem.LoadCharacter(context.Background(), "Quincy"); found {
			if res, _ := mem.SaveCharacter(context.Background(), cur); res.Outcome == SaveApplied {
				break
			}
		}
		if time.Now().After(setupDeadline) {
			t.Fatal("setup: could not advance the durable row ahead of the session")
		}
	}

	// Quit: leave() enqueues a saveFinal. Its first CAS loses (stale version); the saver re-reads,
	// rebases onto the advanced version, and retries — landing the logout room (market).
	quit(t, z, "Quincy")
	got := waitRowWhere(t, mem, "Quincy", func(s CharSnapshot) bool {
		return s.RoomRef == "midgaard:room:market"
	})
	if got.RoomRef != "midgaard:room:market" {
		t.Fatalf("logout flush lost its CAS and was dropped: durable room = %q, want market", got.RoomRef)
	}
}

// TestFinalFlushYieldsToReattachedSession proves the MUST-FIX from the durability review: a logout
// flush that loses its CAS must NOT force-overwrite a NEWER legitimate write made by a session that
// RE-ATTACHED within the link-death grace while the reconcile loop was running. finalizeFlush probes
// z.players (the single-writer authority): a present live session means its current state wins, so
// the stale logout snapshot is routed through the live reconcile path (saveConflictMsg -> re-dump
// current) instead of being force-written — it must never revert the live session's room.
//
// We drive finalizeFlush directly with a STALE logout snapshot (old room, old version) while a LIVE
// session for the same name is present in the zone at a NEWER room, and assert the durable row ends
// on the LIVE room — never reverted to the stale snapshot's room.
func TestFinalFlushYieldsToReattachedSession(t *testing.T) {
	shard, z, mem := persistShard(t)

	// A live session: logs in, walks to the market. This stands in for the player who RE-ATTACHED
	// within the link-death grace — z.players holds it, and its current room is the market.
	out := login(t, shard, z, "Rex")
	waitRow(t, mem, "Rex")
	waitEntityPID(t, z, "Rex")
	drainChan(out)
	sendInput(z, "Rex", "north") // temple -> market
	waitFrame(t, out, "Market")

	// Let the live session flush its current (market) state durably so the row reflects the live
	// truth before we fire the stale final flush.
	z.post(drainFlushMsg{})
	live := waitRowWhere(t, mem, "Rex", func(s CharSnapshot) bool { return s.RoomRef == "midgaard:room:market" })

	// A STALE logout snapshot for the SAME name: an OLD room (temple) at an OLD version. Were
	// finalizeFlush to blindly force-rebase + write this, it would REVERT the live session's market
	// room to temple. Because a live session is present, it must instead yield to the reconcile path.
	stale := CharSnapshot{
		PID: live.PID, Name: "Rex", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 0,
	}
	req := saveRequest{snap: stale, zone: z, id: "Rex", reason: saveFinal}
	ctx, cancel := context.WithTimeout(context.Background(), finalFlushBudget)
	defer cancel()
	// curVersion is what the refused CAS observed under the row lock — here, the live session's own
	// last flush. It goes unused on this path: the live-session probe yields before the rebase.
	z.saver.finalizeFlush(ctx, req, stale, live.StateVersion)

	// The durable row must NOT have been reverted to temple. The yield posts saveConflictMsg, whose
	// reconcile re-dumps the LIVE session's current (market) state — so the row stays at market.
	settle := waitRowWhere(t, mem, "Rex", func(s CharSnapshot) bool { return s.RoomRef == "midgaard:room:market" })
	if settle.RoomRef != "midgaard:room:market" {
		t.Fatalf("final flush reverted a re-attached live session: durable room = %q, want market (never temple)", settle.RoomRef)
	}
}

// TestCheckpointFreshnessWins proves the login read picks the HIGHER-state_version source between
// the Postgres row and the Redis checkpoint — the crash-window freshness check. We write a stale
// row and a fresher checkpoint and assert the loaded snapshot is the checkpoint's.
//
// NOTE (#322): this exercises the comparator with checkpoint_version STRICTLY GREATER than the row's — a
// relationship that is not actually reachable in production, because the checkpoint is dumped at the
// pre-CAS-bump version so checkpoint_version <= row_version always holds. It is a valid unit test of the
// operator in isolation, but the reachable crash-recovery case is the TIE — see
// TestCheckpointWinsOnStateVersionTie, which is the one that fails on the pre-fix strict `>`.
func TestCheckpointFreshnessWins(t *testing.T) {
	mem := NewMemStore()
	shard := NewDemoShard().WithPersistence(mem, mem)
	ctx := context.Background()

	// A durable row at version 2 (the last Postgres flush).
	pid, _ := mem.CreateCharacter(ctx, "Del", "midgaard", "midgaard:room:temple")
	row := CharSnapshot{PID: pid, Name: "Del", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 0}
	for row.StateVersion < 2 {
		res, _ := mem.SaveCharacter(ctx, row)
		row.StateVersion = res.NewVersion
	}
	// A FRESHER checkpoint at version 5 in a DIFFERENT room (a more recent ~10s mirror).
	_ = mem.Checkpoint(ctx, CharSnapshot{
		PID: pid, Name: "Del", ZoneRef: "midgaard", RoomRef: "midgaard:room:market", StateVersion: 5,
	})

	snap, ok, _ := shard.loadCharacterSnapshot(ctx, "Del")
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
	snap2, ok, _ := shard.loadCharacterSnapshot(ctx, "Del")
	if !ok {
		t.Fatal("expected to load a snapshot (inverse)")
	}
	if snap2.StateVersion != 2 || snap2.RoomRef != "midgaard:room:temple" {
		t.Fatalf("freshness check chose version %d room %q, want the row (2, temple)",
			snap2.StateVersion, snap2.RoomRef)
	}
}

// TestCheckpointWinsOnStateVersionTie is the #322 regression: state_version only advances on a durable
// Postgres CAS, never on a checkpoint-only pulse, so the ~10s checkpoints between two ~60s flushes all carry
// the SAME state_version as the last flush but STRICTLY NEWER content (the player kept moving). A strict `>`
// freshness check made every one of those checkpoints lose the tie to Postgres, silently widening the crash
// data-loss window from the ~10s the checkpoint tier advertises back to the ~60s Postgres cadence. The tie
// must resolve to the CHECKPOINT — by construction it is never older than the row it mirrors at an equal
// version.
func TestCheckpointWinsOnStateVersionTie(t *testing.T) {
	mem := NewMemStore()
	shard := NewDemoShard().WithPersistence(mem, mem)
	ctx := context.Background()

	// The durable row: last Postgres flush left the player in the temple at version 2.
	pid, _ := mem.CreateCharacter(ctx, "Tie", "midgaard", "midgaard:room:temple")
	row := CharSnapshot{PID: pid, Name: "Tie", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 0}
	for row.StateVersion < 2 {
		res, _ := mem.SaveCharacter(ctx, row)
		row.StateVersion = res.NewVersion
	}

	// A checkpoint-only pulse fired AFTER the flush: the player walked to the market, but no Postgres flush
	// bumped the version, so the checkpoint mirrors the SAME version 2 with the newer room. This is the exact
	// tie the bug dropped.
	_ = mem.Checkpoint(ctx, CharSnapshot{
		PID: pid, Name: "Tie", ZoneRef: "midgaard", RoomRef: "midgaard:room:market", StateVersion: 2,
	})

	snap, ok, _ := shard.loadCharacterSnapshot(ctx, "Tie")
	if !ok {
		t.Fatal("expected to load a snapshot")
	}
	if snap.StateVersion != 2 || snap.RoomRef != "midgaard:room:market" {
		t.Fatalf("on a state_version tie the checkpoint must win: got version %d room %q, want (2, market)",
			snap.StateVersion, snap.RoomRef)
	}

	// Guard the boundary the tie-break relies on: a GENUINELY stale checkpoint (a LOWER version, e.g. Redis
	// lapsed while Postgres advanced) must still lose. Advance the row to version 3 (market via a flush),
	// leaving the checkpoint at version 2 — the row is now strictly fresher and must win.
	row = CharSnapshot{PID: pid, Name: "Tie", ZoneRef: "midgaard", RoomRef: "midgaard:room:square", StateVersion: 2}
	advance, _ := mem.SaveCharacter(ctx, row)
	if advance.Outcome != SaveApplied {
		t.Fatalf("row flush to advance version failed: outcome=%v", advance.Outcome)
	}
	if advance.NewVersion != 3 {
		t.Fatalf("expected row to advance to version 3, got %d", advance.NewVersion)
	}
	snap2, ok, _ := shard.loadCharacterSnapshot(ctx, "Tie")
	if !ok {
		t.Fatal("expected to load a snapshot (stale-checkpoint case)")
	}
	if snap2.StateVersion != 3 || snap2.RoomRef != "midgaard:room:square" {
		t.Fatalf("a strictly-newer row must beat a lower-version checkpoint: got version %d room %q, want (3, square)",
			snap2.StateVersion, snap2.RoomRef)
	}
}

// TestCheckpointTieRecoversMigratedHandoffLocation locks in the highest-value case the #322 tie-break
// unblocks (raised by the distsys review): a just-migrated player. The cross-shard handoff carries
// state_version and the destination seeds session.stateVersion from it, so a player who walked
// midgaard->darkwood is at version N on BOTH the durable row (still the SOURCE's last-flushed pre-handoff
// location, because the destination has not flushed yet) and the destination's first checkpoint pulse (the
// MIGRATED location, at the same seeded version N). Under the pre-fix strict `>` a crash before the
// destination's first flush would rehydrate the player at their PRE-HANDOFF room — an actual data-loss bug.
// The tie-break recovers the migrated location. This test models that state at the load seam so a future
// revert to `>` is caught.
func TestCheckpointTieRecoversMigratedHandoffLocation(t *testing.T) {
	mem := NewMemStore()
	shard := NewDemoShard().WithPersistence(mem, mem)
	ctx := context.Background()

	// Source's last durable flush before the handoff: the player is in midgaard at version 3.
	pid, _ := mem.CreateCharacter(ctx, "Migrant", "midgaard", "midgaard:room:temple")
	row := CharSnapshot{PID: pid, Name: "Migrant", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 0}
	for row.StateVersion < 3 {
		res, _ := mem.SaveCharacter(ctx, row)
		row.StateVersion = res.NewVersion
	}

	// The destination rehydrated the carried player (session.stateVersion seeded to the SAME 3, no flush on
	// the destination yet) and its first ~10s checkpoint pulse mirrors the MIGRATED location at that version.
	_ = mem.Checkpoint(ctx, CharSnapshot{
		PID: pid, Name: "Migrant", ZoneRef: "darkwood", RoomRef: "darkwood:room:entrance", StateVersion: 3,
	})

	snap, ok, _ := shard.loadCharacterSnapshot(ctx, "Migrant")
	if !ok {
		t.Fatal("expected to load a snapshot")
	}
	// The tie must recover the MIGRATED darkwood location, not the pre-handoff midgaard row.
	if snap.ZoneRef != "darkwood" || snap.RoomRef != "darkwood:room:entrance" {
		t.Fatalf("post-handoff crash recovery landed at %s/%s, want the migrated darkwood:room:entrance "+
			"(a strict `>` would strand the player at the pre-handoff midgaard row)", snap.ZoneRef, snap.RoomRef)
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
