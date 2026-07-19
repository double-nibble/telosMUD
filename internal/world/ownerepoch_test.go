package world

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
)

// ownerepoch_test.go is the regression net for #432: a stale shard's final flush force-wrote its
// 60-second-old snapshot over the live owner's durable state, because the `state_version` CAS that
// was documented as "the fence against a zombie original" is CONTENTION control, and every caller
// answered a contention refusal by re-reading, rebasing and writing again. finalizeFlush did that
// explicitly (`snap.StateVersion = cur.StateVersion`), making the CAS succeed by construction, and
// the only guard in front of it — zonePresent — probes THIS process's z.players, so a live session
// on another shard was structurally invisible to it.
//
// The exploit that made it a security bug, not a correctness wart: get two live copies (a second
// login landing on a different shard is enough — the kicked connection stays detached-but-resident
// for the whole 60s link-dead grace), externalize wealth on copy B, then let copy A's reap roll the
// character back while the items persist on the alt. Repeatable.
//
// The fix is an OWNERSHIP axis at the sink: `characters.owner_epoch`, minted atomically by every
// claim (login, handoff), carried on every snapshot, and applied as an independent conjunct
// (`owner_epoch <= $k`) that no amount of rebasing can reach around. These tests pin the fence at
// each place it can be defeated: the final-flush force-write, the live-path reconcile loop, the zone's
// reaction to a not-owner verdict, the atomicity of the mint itself, and the login/handoff paths that
// must never wedge a legitimate player with their own fence.

// ---------------------------------------------------------------------------------------------
// Cardinality helpers. Existence assertions are what let the #69 corpse dupe reach main: a test that
// asks "is the item there?" passes with nine of them. These count occurrences of a prototype across
// EVERY durable row AND every checkpoint, descending into container contents (`put torch in bag` is
// a real hiding place and a naive top-level scan misses it entirely).
// ---------------------------------------------------------------------------------------------

// countProtoInItem counts occurrences of proto in it and, recursively, everything nested inside it.
func countProtoInItem(it ItemJSON, proto string) int {
	n := 0
	if it.ProtoRef == proto {
		n++
	}
	for _, c := range it.Contents {
		n += countProtoInItem(c, proto)
	}
	return n
}

// countProtoInState counts occurrences of proto anywhere in one persisted entity state — carried
// inventory, worn equipment, and the contents of any container in either.
func countProtoInState(st StateJSON, proto string) int {
	n := 0
	for _, it := range st.Inventory {
		n += countProtoInItem(it, proto)
	}
	for _, it := range st.Equipment {
		n += countProtoInItem(it, proto)
	}
	return n
}

// protoCensus is the dupe oracle: how many copies of proto exist in each durable tier, across EVERY
// character (not just the one under test) and inside every container.
//
// The two tiers are counted SEPARATELY because they are mirrors of one another — one logical item is
// legitimately present in both — so a single total would conflate "the same item, checkpointed" with
// "a second item". Each tier is then asserted at an exact expected count: a rollback that resurrects
// an externalized item takes the row tier from 1 to 2 while the alt still holds theirs, which is
// exactly the shape of the #432 dupe. The checkpoint tier is counted at all because it is its own
// unfenced-by-default slot: before #432 the Postgres fence could be intact and the rollback simply
// happened one tier up.
func protoCensus(m *MemStore, proto string) (rows, checkpoints int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		rows += countProtoInState(row.State, proto)
	}
	for _, c := range m.ckpt {
		checkpoints += countProtoInState(c.State, proto)
	}
	return rows, checkpoints
}

// ---------------------------------------------------------------------------------------------
// 1. The headline exploit.
// ---------------------------------------------------------------------------------------------

// TestStaleFinalFlushCannotRollBackALiveOwner builds the #432 exploit end to end and asserts it no
// longer works.
//
// Session A holds "Mark" at epoch 1 with a torch, flushed durably. A second login lands on another
// shard: that is modelled at the store seam (ClaimCharacter), which is exactly what server.go's claim
// block does, and it mints epoch 2 for session B. B externalizes the torch — gives it to an alt, who
// stuffs it in a bag — and saves. Then A is reaped and its saveFinal fires with a 1-epoch-old
// snapshot that still contains the torch.
//
// Pre-fix, that final flush rebased onto B's state_version and force-wrote, restoring the torch to
// Mark while the alt kept theirs: one torch became two. The assertion is therefore CARDINALITY across
// every row and every checkpoint, including nested container contents — not "the row changed".
func TestStaleFinalFlushCannotRollBackALiveOwner(t *testing.T) {
	const torch = "midgaard:obj:torch"
	ctx := context.Background()
	shard, z, mem := persistShard(t)

	// --- Session A: log in, walk to the market, pick up the one distinctive item, flush it durably.
	out := login(t, shard, z, "Mark")
	waitRow(t, mem, "Mark")
	waitEntityPID(t, z, "Mark")
	drainChan(out)
	sendInput(z, "Mark", "north")
	waitFrame(t, out, "Market")
	sendInput(z, "Mark", "get torch")
	waitFrame(t, out, "torch")

	z.post(drainFlushMsg{})
	rowA := waitRowWhere(t, mem, "Mark", func(s CharSnapshot) bool {
		return countProtoInState(s.State, torch) == 1
	})
	require.EqualValues(t, 1, rowA.OwnerEpoch,
		"premise: session A's own flush must have stamped its epoch (1) onto the row; the fence is armed by the first save")
	rows, ckpts := protoCensus(mem, torch)
	require.Equal(t, 1, rows, "premise: exactly one torch row-side before the exploit")
	require.Equal(t, 1, ckpts, "premise: the same torch, mirrored into A's checkpoint")

	// --- Session B: a second login on ANOTHER shard. The only thing that crosses the process
	// boundary is the store, so the claim is the whole of it. ClaimCharacter must mint strictly above
	// A's epoch — that is what makes A a zombie from this instant on.
	epochB, err := mem.ClaimCharacter(ctx, rowA.PID, 0)
	require.NoError(t, err, "the second login's ownership claim must succeed")
	require.Greater(t, epochB, rowA.OwnerEpoch,
		"a claim must mint STRICTLY above the row's current epoch, or both copies satisfy owner_epoch <= k and the fence is inert")

	// B externalizes the wealth: the torch goes to an alt, nested inside a bag (the container path a
	// top-level inventory scan would miss), and B saves itself WITHOUT the torch.
	altPID, err := mem.CreateCharacter(ctx, "Alt", "midgaard", "midgaard:room:temple")
	require.NoError(t, err)
	altRes, err := mem.SaveCharacter(ctx, CharSnapshot{
		PID: altPID, Name: "Alt", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple",
		StateVersion: 0, OwnerEpoch: 1,
		State: StateJSON{Inventory: []ItemJSON{{
			ProtoRef: "midgaard:obj:bag",
			Contents: []ItemJSON{{ProtoRef: torch}},
		}}},
	})
	require.NoError(t, err)
	require.Equal(t, SaveApplied, altRes.Outcome, "the alt's save must land")

	stripped := rowA
	stripped.OwnerEpoch = epochB
	stripped.State.Inventory = nil
	bRes, err := mem.SaveCharacter(ctx, stripped)
	require.NoError(t, err)
	require.Equal(t, SaveApplied, bRes.Outcome, "the LIVE owner's save must land")
	require.NoError(t, mem.Checkpoint(ctx, stripped), "B's checkpoint pulse")
	rows, ckpts = protoCensus(mem, torch)
	require.Equal(t, 1, rows, "premise: after externalization exactly one torch exists row-side — on the alt, in the bag")
	require.Equal(t, 0, ckpts, "premise: B's pulse overwrote the checkpoint slot, so no torch is mirrored there")

	// --- The exploit: A is reaped and its saveFinal fires with a snapshot that still holds the torch.
	// quit() posts the leave that enqueues saveFinal; FlushSaver is the existing barrier that drives
	// the saver (including finalizeFlush's whole retry ladder) to completion — no sleeps.
	quit(t, z, "Mark")
	fctx, fcancel := context.WithTimeout(ctx, 10*time.Second)
	defer fcancel()
	require.NoError(t, shard.FlushSaver(fctx), "the saver barrier must drain A's final flush")

	// --- The verdict. THE assertion is the count: a rollback resurrects the torch on Mark while the
	// alt keeps theirs, so the dupe shows as 2 row-side (and 1 checkpoint-side, once A's pulse lands).
	rows, ckpts = protoCensus(mem, torch)
	if rows != 1 {
		t.Fatalf("DUPE (#432): %d torches exist across the durable rows, want exactly 1 (the alt's, inside its "+
			"bag). A stale session's final flush rebased past the CAS and resurrected an item that had already "+
			"been externalized — the character rolls back while the items persist on the alt, repeatably", rows)
	}
	if ckpts != 0 {
		t.Fatalf("DUPE via the CHECKPOINT tier (#432): %d torches exist across the checkpoints, want 0. The "+
			"Postgres fence can be perfectly intact while the rollback happens one tier up: the checkpoint is a "+
			"single key per character, both copies pulse it, and the login read prefers it on a version tie", ckpts)
	}

	final, ok, err := mem.LoadCharacter(ctx, "Mark")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 0, countProtoInState(final.State, torch),
		"the live owner's inventory was rolled back to the zombie's: the torch reappeared on Mark")
	require.Equal(t, epochB, final.OwnerEpoch,
		"the row's owner_epoch must still name session B; a zombie write must never lower or re-stamp it")
	require.Equal(t, bRes.NewVersion, final.StateVersion,
		"the row's state_version must be exactly B's lineage (%d) — any advance means a write landed after B's",
		bRes.NewVersion)

	// And the checkpoint tier, which is where the rollback moved to when only Postgres was fenced.
	ck, ok, err := mem.LoadCheckpoint(ctx, "Mark")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 0, countProtoInState(ck.State, torch),
		"the CHECKPOINT was rolled back by the zombie's pulse — the Postgres fence is bypassed one tier up")
	require.Equal(t, epochB, ck.OwnerEpoch, "the checkpoint slot must still be owned by B")
}

// TestFinalFlushRefusesAnOwnershipLossWithoutRetrying pins finalizeFlush's own handling of the fence.
//
// finalizeFlush is entered only after a first CAS lost on state_version, so it sees SaveNotOwner when
// the epoch is raised BETWEEN its two calls — another shard claiming the character in the window
// between the refused flush and its retry. That is a real race, and its correct handling is to stop
// dead: the loss is definitive, so every one of the remaining retries would re-fail on the same
// conjunct while burning the flush budget (and, before the fence existed, this loop is precisely
// where `snap.StateVersion = cur.StateVersion` turned a refusal into a force-write).
//
// The assertion is the STORE CALL COUNT, because the fence at the sink makes the retries harmless to
// data — they are only ever wasted work on a shared drainer. A count of 1 says "definitive means
// definitive"; a count of finalFlushRetries says the loop is treating ownership as contention.
func TestFinalFlushRefusesAnOwnershipLossWithoutRetrying(t *testing.T) {
	ctx := context.Background()
	store := newCountingStore(0)
	shard := NewDemoShard().WithPersistence(store, store)
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go shard.Run(rctx)
	z := shard.Zone()

	// A row nobody is logged into, already claimed by another shard.
	pid, err := store.CreateCharacter(ctx, "Ghost", "midgaard", "midgaard:room:temple")
	require.NoError(t, err)
	row, err := store.SaveCharacter(ctx, CharSnapshot{
		PID: pid, Name: "Ghost", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 0, OwnerEpoch: 1,
	})
	require.NoError(t, err)
	require.Equal(t, SaveApplied, row.Outcome)
	epochB, err := store.ClaimCharacter(ctx, pid, 0)
	require.NoError(t, err)

	before := store.saved.Load()
	stale := CharSnapshot{
		PID: pid, Name: "Ghost", ZoneRef: "midgaard", RoomRef: "midgaard:room:market",
		StateVersion: 0, OwnerEpoch: 1, // the zombie's epoch
	}
	fctx, fcancel := context.WithTimeout(ctx, finalFlushBudget)
	defer fcancel()
	z.saver.finalizeFlush(fctx, saveRequest{snap: stale, zone: z, id: "Ghost", reason: saveFinal},
		stale, row.NewVersion)

	if got := store.saved.Load() - before; got != 1 {
		t.Fatalf("finalizeFlush made %d store calls for an OWNERSHIP loss, want exactly 1 (#432). An epoch loss is "+
			"definitive: the retry ladder cannot raise this writer's own epoch, so every extra pass re-fails on "+
			"the same conjunct while consuming the logout flush budget on the shared drainer", got)
	}
	final, ok, err := store.LoadCharacter(ctx, "Ghost")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "midgaard:room:temple", final.RoomRef, "the zombie's logout snapshot must never land")
	require.Equal(t, epochB, final.OwnerEpoch)
}

// ---------------------------------------------------------------------------------------------
// 2. The live path: an epoch loss must TERMINATE, not become a reconcile loop.
// ---------------------------------------------------------------------------------------------

// boundedSaveStore counts SaveCharacter calls for one character and hard-stops past a bound by
// returning an error, so a non-terminating reconcile fails the test with a diagnosis instead of
// hanging until the package timeout. The saver treats a store error as "log and give up", which is
// exactly the circuit-breaker we want here.
type boundedSaveStore struct {
	*MemStore
	mu       sync.Mutex
	name     string
	bound    int
	calls    int
	exceeded bool
}

func (b *boundedSaveStore) SaveCharacter(ctx context.Context, snap CharSnapshot) (SaveResult, error) {
	if snap.Name == b.name {
		b.mu.Lock()
		b.calls++
		over := b.calls > b.bound
		if over {
			b.exceeded = true
		}
		b.mu.Unlock()
		if over {
			return SaveResult{}, errors.New("save-call bound exceeded")
		}
	}
	return b.MemStore.SaveCharacter(ctx, snap)
}

func (b *boundedSaveStore) counts() (calls int, exceeded bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.exceeded
}

// TestStaleCadenceFlushDoesNotLoopOnTheSaverDrainer pins the SECOND force-write path — the live one,
// which the issue calls out as "two live owners ping-pong the row, each rebasing onto the other".
//
// A live-but-superseded session's ordinary cadence/drain flush loses on owner_epoch. The pre-#432
// answer to any refusal was saveConflictMsg -> Zone.saveConflict, which re-reads, re-dumps and
// re-enqueues IMMEDIATELY (not on the ~60s cadence). Routing an OWNERSHIP loss into that is an
// unbounded read-write loop on the drainer goroutine that every zone on the shard shares, and it can
// never succeed: the retry rebases state_version while the predicate it keeps losing on is
// owner_epoch. So this asserts two things — the durable state is not rolled back, and the store call
// count stays bounded.
func TestStaleCadenceFlushDoesNotLoopOnTheSaverDrainer(t *testing.T) {
	ctx := context.Background()
	// A generous bound: the terminating path makes exactly one call. Anything near this is a loop.
	store := &boundedSaveStore{MemStore: NewMemStore(), name: "Loop", bound: 8}
	shard := NewDemoShard().WithPersistence(store, store)
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go shard.Run(rctx)
	z := shard.Zone()

	out := login(t, shard, z, "Loop")
	waitRow(t, store.MemStore, "Loop")
	waitEntityPID(t, z, "Loop")
	drainChan(out)
	sendInput(z, "Loop", "north")
	waitFrame(t, out, "Market")

	// Flush so the session's own (market) state is durable and its epoch is stamped on the row.
	z.post(drainFlushMsg{})
	row := waitRowWhere(t, store.MemStore, "Loop", func(s CharSnapshot) bool {
		return s.RoomRef == "midgaard:room:market"
	})

	// Another shard claims the character and writes it back at the temple. The live session here is
	// now a zombie holding market at the old epoch.
	epochB, err := store.ClaimCharacter(ctx, row.PID, 0)
	require.NoError(t, err)
	require.Greater(t, epochB, row.OwnerEpoch, "premise: the claim must supersede the live session's epoch")
	moved := row
	moved.OwnerEpoch = epochB
	moved.RoomRef = "midgaard:room:temple"
	bRes, err := store.SaveCharacter(ctx, moved)
	require.NoError(t, err)
	require.Equal(t, SaveApplied, bRes.Outcome)

	before, _ := store.counts()

	// The zombie flushes. Exactly one store call may result.
	z.post(drainFlushMsg{})

	// Wait for the flush to RESOLVE one way or the other — the session evicted (the terminal handling
	// ran) or the store's circuit breaker tripped (a runaway loop). Waiting on either is what keeps the
	// loop-bound assertion reachable: a plain "wait for eviction" would time out first under a
	// regression and report the wrong diagnosis. No sleeps as synchronization.
	rolledBack := func() bool {
		snap, ok, _ := store.LoadCharacter(ctx, "Loop")
		return ok && snap.RoomRef != "midgaard:room:temple"
	}
	waitCond(t, "the refused flush to resolve (eviction, a rollback, or the store's call bound tripping)", func() bool {
		_, exceeded := store.counts()
		return exceeded || rolledBack() || !zoneHasPlayer(z, "Loop")
	})
	// Give a (buggy) loop a window to actually run away, bounded by the store's own circuit breaker.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, exceeded := store.counts(); exceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	calls, exceeded := store.counts()
	if exceeded {
		t.Fatalf("the not-owner reconcile DID NOT TERMINATE: more than %d saves for ONE refused flush (#432). "+
			"An epoch loss routed into saveConflict re-reads, re-dumps and re-enqueues immediately (not on the "+
			"~60s cadence) and can never succeed — the retry rebases state_version while the predicate it keeps "+
			"losing on is owner_epoch — so it spins on the drainer goroutine every zone on this shard shares",
			store.bound)
	}
	// (a) The durable state was not rolled back.
	final, ok, err := store.LoadCharacter(ctx, "Loop")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "midgaard:room:temple", final.RoomRef,
		"the zombie's LIVE flush rolled the durable location back over the legitimate owner's (#432) — the same "+
			"force-write as the logout path, on the cadence instead")
	require.Equal(t, bRes.NewVersion, final.StateVersion,
		"no write may land after the legitimate owner's (version %d)", bRes.NewVersion)
	require.Equal(t, epochB, final.OwnerEpoch, "the row must still be owned by the claimant")

	// (b) It cost exactly one store call, and the session is gone.
	if got := calls - before; got != 1 {
		t.Fatalf("one refused flush produced %d store calls, want exactly 1: an ownership loss is DEFINITIVE and "+
			"must never be retried or rebased", got)
	}
	require.False(t, zoneHasPlayer(z, "Loop"),
		"a session that has definitively lost ownership must be EVICTED: it can never persist again (only a "+
			"fresh claim mints an epoch), so continuing to accept its commands generates silently unsaveable play")
}

// ---------------------------------------------------------------------------------------------
// 3. Zone.ownershipLost — evict the zombie, but never a legitimate re-attach.
// ---------------------------------------------------------------------------------------------

// bareEpochShard returns a persistence-backed shard + home zone with NO Run loop, so the test can
// call zone methods directly (they are single-writer, and there is no competing goroutine).
//
// The saver's drainer is deliberately NOT started: that makes its request QUEUE the observable. A
// test that asserted on store call counts here would be vacuous — with no drainer, an enqueued save
// never reaches the store, so "the store saw nothing" would pass even for a path that queues a write.
// Counting queued requests catches the enqueue itself.
func bareEpochShard(t *testing.T) (*Zone, *countingStore) {
	t.Helper()
	store := newCountingStore(0)
	sh := NewDemoShard().WithPersistence(store, store)
	return sh.Zone(), store
}

// TestOwnershipLostEvictsTheZombieButNotALegitimateReattach pins all three halves of
// Zone.ownershipLost — the eviction, and the two guards in front of it.
//
// A not-owner verdict describes the epoch the refused SNAPSHOT carried, not the session's epoch now,
// and it does not say who the verdict is FOR. Two entirely different situations produce one:
//
//   - A genuine double-own. The session is a zombie; it can never persist again, because only a fresh
//     claim mints an epoch and this one will never get another. Evicting it is the whole point.
//   - This shard's OWN activity. A session legitimately raises its epoch (a re-attach adopting a
//     login claim, a failed handoff adopting its mint) or is mid-handoff while THIS shard mints the
//     epoch that beats its own in-flight save. Evicting there kicks a legitimate player.
//
// The frozen/pending clause is the one the expert panel found missing, and it is not expressible as
// an epoch comparison at all: mid-handoff, the epoch that beat the save is one this very process
// minted seconds ago, and it looks identical to a rival shard's claim from inside ownershipLost. Only
// the session's own state distinguishes them. See
// TestASaveRefusedByOurOwnHandoffMintDoesNotEvictTheMovingPlayer for the end-to-end version.
func TestOwnershipLostEvictsTheZombieButNotALegitimateReattach(t *testing.T) {
	cases := []struct {
		name         string
		sessionEpoch uint64
		verdictEpoch uint64
		ownerEpoch   uint64
		frozen       bool
		pending      bool
		wantEvicted  bool
		why          string
	}{
		{
			name: "zombie: the verdict names the epoch the session still holds",
			// The ordinary double-own: this session's own dump was refused, and it has claimed nothing
			// since. It can never persist again, so continuing to accept its commands would generate
			// silently unsaveable play.
			sessionEpoch: 3, verdictEpoch: 3, ownerEpoch: 4, wantEvicted: true,
			why: "a session whose current epoch lost the fence is a zombie and must be evicted",
		},
		{
			name: "zombie: the verdict is from a LATER save than the session's epoch",
			// Defensive: a verdict can only carry an epoch >= the session's if the session lowered its
			// own (which nothing may do). Treat it as live rather than silently ignoring it.
			sessionEpoch: 3, verdictEpoch: 4, ownerEpoch: 9, wantEvicted: true,
			why: "a verdict whose WINNER is above the session's epoch is not an echo and must be acted on",
		},
		{
			name: "zombie: the session raised its epoch but the WINNER is still above it",
			// The discriminating case for the tightened guard. The old form compared the refused
			// snapshot's epoch (3 < 4 -> spare); the correct form compares the winner (5 > 4 -> evict).
			// A session that caught up part-way is still outranked, and sparing it leaves a copy that can
			// never save.
			sessionEpoch: 4, verdictEpoch: 3, ownerEpoch: 5, wantEvicted: true,
			why: "the guard must compare against the epoch that BEAT us, not the one the refused snapshot carried",
		},
		{
			name: "STALE verdict: the session has since adopted a newer claim",
			// The re-attach / handoff-redirect case. The refused save was dumped at epoch 3; between the
			// dump and the verdict the session adopted epoch 5. It is the legitimate owner NOW.
			sessionEpoch: 5, verdictEpoch: 3, ownerEpoch: 5, wantEvicted: false,
			why: "a session already holding the winning epoch IS the owner; the verdict is an echo of its own past",
		},
		{
			name: "STALE verdict: the session has caught up to exactly the winning epoch",
			// The boundary the review's `<=` makes explicit: the winner is 4 and the session adopted 4,
			// so the session and the winner are the same claim. Strict `<` here would evict the owner.
			sessionEpoch: 4, verdictEpoch: 3, ownerEpoch: 4, wantEvicted: false,
			why: "equality means the session IS the winner; the comparison must be <=, not <",
		},
		{
			name: "MID-HANDOFF (frozen): this shard minted the epoch that beat our own save",
			// THE BLOCKER. beginHandoff raises the row while the source still holds its old epoch, so a
			// save enqueued before the move comes back not-owner naming an epoch we minted ourselves. No
			// epoch comparison can tell that from a rival shard's claim — the session's own state can.
			// The handoff already owns this session's fate: success reaps it without a save, failure
			// thaws it and adopts the mint.
			sessionEpoch: 3, verdictEpoch: 3, ownerEpoch: 4, frozen: true, wantEvicted: false,
			why: "evicting mid-move kicks a legitimate player, discards their in-flight delta, and strands the redirect",
		},
		{
			name: "MID-HANDOFF (pending): the destination-side copy is not an eviction target either",
			// The same argument from the other end: a pending arrival's fate belongs to the Commit/Abort
			// it is waiting on, not to a verdict about a save.
			sessionEpoch: 3, verdictEpoch: 3, ownerEpoch: 4, pending: true, wantEvicted: false,
			why: "a pending copy is owned by the handoff protocol, not by the saver's verdict",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z, store := bareEpochShard(t)
			s := newTestPlayerEntity(z, "Subject")
			s.epoch = tc.sessionEpoch
			z.join(s, "")
			s.frozen, s.pending = tc.frozen, tc.pending
			// A DURABLE identity is load-bearing for the "no save" half: enqueueSave skips a session with
			// no PersistID, so without this the assertion below would pass against a save-taking eviction
			// and prove nothing.
			pid := PersistID("mem-uuid-subject")
			s.entity.pid = &pid
			require.NotNil(t, z.players["Subject"], "premise: the session is resident")

			require.Empty(t, z.saver.reqs, "premise: nothing is queued for the saver before the verdict")

			z.ownershipLost(saveNotOwnerMsg{id: "Subject", ourEpoch: tc.verdictEpoch, ownerEpoch: tc.ownerEpoch})

			if evicted := z.players["Subject"] == nil; evicted != tc.wantEvicted {
				t.Fatalf("session evicted = %v, want %v (#432): %s", evicted, tc.wantEvicted, tc.why)
			}
			if n := len(z.saver.reqs); n != 0 {
				t.Fatalf("an ownership eviction QUEUED %d durable save(s); it must remove the session WITHOUT "+
					"one — persisting is precisely what the fence just refused, and a queued write would be the "+
					"force-write under another name", n)
			}
			require.Zero(t, store.saved.Load(), "and nothing reached the store either")
			require.Empty(t, z.pendingFinalFlush,
				"the zombie's logout snapshot must not be PARKED either: replaying it once a PersistID lands "+
					"would be the same force-write under another name")
			if tc.wantEvicted {
				if got := z.pop.Load(); got != 0 {
					t.Fatalf("zone occupancy = %d after evicting the only player, want 0 — leaveNoSave must unwind "+
						"the registrations, not just delete the map entry", got)
				}
				if s.entity != nil && s.entity.location != nil {
					t.Fatal("the evicted zombie's entity is still standing in a room")
				}
			}
		})
	}
}

// TestASaveRefusedByOurOwnHandoffMintDoesNotEvictTheMovingPlayer is the end-to-end form of the
// blocker both the distributed-systems and security reviewers found independently, and it is the most
// important test in this change after the dupe exploit itself.
//
// The sequence is ordinary play, not a crafted race:
//
//  1. the cadence dumps a save for a player at epoch N and hands it to the async saver;
//  2. the player walks a cross-shard exit, so THIS shard's beginHandoff mints N+1 from the row —
//     raising the durable row above the session that is still sitting here, frozen, holding N;
//  3. the queued save reaches Postgres and is refused as SaveNotOwner, naming an epoch this very
//     process minted moments ago.
//
// A verdict-handler that reasons only about epochs sees "somebody else owns this character" and
// evicts — kicking a player mid-move, discarding the delta in that snapshot, and stranding the
// redirect the gate is waiting on. The whole fix would have converted a dupe into a disconnect on
// every cross-shard step, which is a worse bug and a far more visible one.
//
// The zone loop is not running, so the test drives the inbox itself and controls the interleaving
// exactly: the failure is drained but NOT handled (the session stays frozen) before the save is
// enqueued, which is precisely the window step 3 lands in.
func TestASaveRefusedByOurOwnHandoffMintDoesNotEvictTheMovingPlayer(t *testing.T) {
	ctx := context.Background()
	peers := func(string) (handoffv1.HandoffClient, error) { return rejectingHandoffClient{}, nil }
	sh, z, s, pid, rowEpoch := handoffFixture(t, epochStubLocator{}, peers, 7)
	s.entity.pid = &pid
	// A distinctive carried item stands in for "the delta in the queued snapshot": if the session is
	// evicted, this is what the player loses.
	s.entity.short = "Mover"

	svctx, svcancel := context.WithCancel(ctx)
	defer svcancel()
	go sh.saver.run(svctx)

	// (2) The move mints N+1 from the row and then fails at Prepare.
	sh.beginHandoff(z, &handoffv1.PlayerSnapshot{CharacterId: "Mover", PersistId: string(pid)},
		"darkwood", "", s.epoch)
	fail := awaitHandoffFail(t, z)
	require.EqualValues(t, rowEpoch+1, fail.adoptEpoch, "premise: the move minted %d from the row", rowEpoch+1)
	// Deliberately NOT handled yet: the session is still frozen at its old epoch, which is the window.
	require.True(t, s.frozen, "premise: the source session is still frozen mid-move")

	// (1)+(3) The cadence save, dumped at the OLD epoch, now reaches the store and is refused.
	z.enqueueSave("Mover", s, saveFlush)
	verdict := awaitNotOwner(t, z)
	require.EqualValues(t, s.epoch, verdict.ourEpoch, "premise: the refused save carried the session's old epoch")
	require.Greater(t, verdict.ownerEpoch, s.epoch,
		"premise: the winning epoch is above the session's — indistinguishable, from inside ownershipLost, "+
			"from a rival shard's claim")

	z.handle(verdict)

	// THE assertion.
	require.Same(t, s, z.players["Mover"],
		"a player mid-cross-shard-move was EVICTED by a not-owner verdict for an epoch THIS SHARD minted (#432 "+
			"review blocker). beginHandoff raises the row while the source still holds its old epoch, so every "+
			"cross-shard step with a save in flight would kick the player, discard the delta in that snapshot, "+
			"and strand the redirect. The handoff owns a frozen session's fate on both branches; the verdict "+
			"handler must not")
	require.True(t, s.frozen, "the verdict must not have thawed or otherwise disturbed the moving session")
	require.NotNil(t, s.entity, "the session's entity (and with it the queued delta) must be intact")
	require.Empty(t, z.pendingFinalFlush, "no logout snapshot may be parked for a player who is merely moving")

	// And the redirect can still land: the failure path thaws, adopts the mint, and saves.
	z.handle(fail)
	require.False(t, s.frozen)
	require.Equal(t, fail.adoptEpoch, s.epoch, "the thaw must still adopt the mint after a spared verdict")
	res, err := sh.saver.store.SaveCharacter(ctx, dumpCharacter(s))
	require.NoError(t, err)
	require.Equal(t, SaveApplied, res.Outcome,
		"after the spared verdict and the thaw, the player must be saveable again (got %v)", res.Outcome)
}

// awaitNotOwner drains the next saveNotOwnerMsg from a zone's inbox, skipping anything else.
func awaitNotOwner(t *testing.T, z *Zone) saveNotOwnerMsg {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-z.inbox:
			if v, ok := m.(saveNotOwnerMsg); ok {
				return v
			}
		case <-deadline:
			t.Fatal("no saveNotOwnerMsg ever reached the zone; the refused save was swallowed")
			return saveNotOwnerMsg{}
		}
	}
}

// TestOwnershipLostFollowsAPlayerWhoWalkedToASiblingZone pins the forwarding branch.
//
// The save was enqueued in zone A, the player walked to zone B on the same shard (an intra-shard
// transfer leaves a forwarding entry behind), and the verdict comes back to A — which no longer holds
// the session. Returning early there is not harmless: the zombie keeps playing, unsaveably, in zone B,
// which is the single outcome this handler exists to prevent. leave() already forwards for exactly
// this reason; the verdict must too.
func TestOwnershipLostFollowsAPlayerWhoWalkedToASiblingZone(t *testing.T) {
	store := newCountingStore(0)
	sh := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil).
		WithPersistence(store, store)
	src, dst := sh.Zone(), sh.ZoneByID("darkwood")
	require.NotNil(t, dst, "premise: the shard hosts a second zone")

	s := newTestPlayerEntity(dst, "Walker")
	s.epoch = 3
	dst.join(s, "")
	pid := PersistID("mem-uuid-walker")
	s.entity.pid = &pid
	// The source zone remembers where the player went, exactly as an intra-shard transfer leaves it.
	src.forwarding["Walker"] = dst

	src.ownershipLost(saveNotOwnerMsg{id: "Walker", ourEpoch: 3, ownerEpoch: 4})

	// The forward is a POST, so the destination's own handler runs it.
	select {
	case m := <-dst.inbox:
		v, ok := m.(saveNotOwnerMsg)
		require.Truef(t, ok, "the forwarded message must arrive intact, got %T", m)
		dst.handle(v)
	case <-time.After(5 * time.Second):
		t.Fatal("the not-owner verdict was DROPPED when the player had walked to a sibling zone (#432): the " +
			"zombie keeps playing unsaveably in the zone it moved to, which is the one outcome this handler " +
			"exists to prevent — leave() forwards for exactly this reason and so must this")
	}
	require.Nil(t, dst.players["Walker"], "the forwarded verdict must evict the zombie in the zone that holds it")
}

// TestACheckpointRefusalAloneEvictsTheZombie pins the route the review added: the Redis tier's
// refusal is a zombie verdict, not a lost tick.
//
// This tier pulses roughly six times per Postgres flush cadence, which makes it the EARLIEST detector
// of a double-own the system has. Swallowing the refusal — returning nil, as the first cut did — threw
// that signal away every time it fired and left the zombie to keep generating unsaveable play, and
// keep externalizing wealth into another character's row, until the far slower Postgres fence caught
// up. The saver must therefore route ErrCheckpointNotOwner to the same eviction the Postgres verdict
// drives, WITHOUT needing a Postgres refusal to corroborate it.
//
// The store here deliberately ACCEPTS every save, so the only thing that can produce an eviction is
// the checkpoint route.
func TestACheckpointRefusalAloneEvictsTheZombie(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	// A store that never refuses: if this test goes green, it is the checkpoint tier that did it.
	sh := NewDemoShard().WithPersistence(alwaysApplyStore{mem}, mem)
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go sh.Run(rctx)
	z := sh.Zone()

	out := login(t, sh, z, "Pulse")
	waitEntityPID(t, z, "Pulse")
	drainChan(out)

	// A NEWER owner claims the checkpoint slot. Nothing touches Postgres.
	require.NoError(t, mem.Checkpoint(ctx, CharSnapshot{
		Name: "Pulse", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 1, OwnerEpoch: 99,
	}))

	// The zombie's next ~10s pulse is refused by the guard.
	z.post(drainFlushMsg{})

	waitCond(t, "the checkpoint refusal to evict the zombie", func() bool { return !zoneHasPlayer(z, "Pulse") })
}

// alwaysApplyStore accepts every save regardless of epoch or version, so a test can isolate the
// checkpoint tier as the only possible source of a not-owner verdict.
type alwaysApplyStore struct{ *MemStore }

func (a alwaysApplyStore) SaveCharacter(ctx context.Context, snap CharSnapshot) (SaveResult, error) {
	if cur, ok, _ := a.LoadCharacter(ctx, snap.Name); ok {
		snap.StateVersion = cur.StateVersion
		if snap.OwnerEpoch < cur.OwnerEpoch {
			snap.OwnerEpoch = cur.OwnerEpoch
		}
	}
	return a.MemStore.SaveCharacter(ctx, snap)
}

// ---------------------------------------------------------------------------------------------
// 4. The mint. Everything above rests on this being atomic.
// ---------------------------------------------------------------------------------------------

// TestClaimCharacterIsAtomic pins the property the entire fence rests on: an epoch value names
// exactly ONE live copy of a character.
//
// A read-then-bump implementation (read the current value, add one, write it back) hands two
// concurrent claimants the SAME epoch, and two copies that both satisfy `owner_epoch <= k` are back
// to fighting over state_version — the bug restored in a shape that looks fixed. The pgx store gets
// this from Postgres's row lock inside one UPDATE; MemStore gets it from the mutex. This is the
// MemStore half; TestPgxClaimCharacterIsAtomic (internal/store, gated) is the real-tier half.
func TestClaimCharacterIsAtomic(t *testing.T) {
	ctx := context.Background()

	t.Run("concurrent claims mint distinct, strictly increasing epochs", func(t *testing.T) {
		mem := NewMemStore()
		pid, err := mem.CreateCharacter(ctx, "Contended", "midgaard", "midgaard:room:temple")
		require.NoError(t, err)

		const n = 64
		epochs := make([]uint64, n)
		var start sync.WaitGroup
		var done sync.WaitGroup
		start.Add(1)
		for i := range n {
			done.Add(1)
			go func() {
				defer done.Done()
				start.Wait()
				ep, err := mem.ClaimCharacter(ctx, pid, 0)
				if err == nil {
					epochs[i] = ep
				}
			}()
		}
		start.Done()
		done.Wait()

		seen := map[uint64]bool{}
		for i, ep := range epochs {
			if ep == 0 {
				t.Fatalf("claim %d failed or minted 0; every claim must mint a real epoch", i)
			}
			if seen[ep] {
				t.Fatalf("epoch %d was minted TWICE (#432): a read-then-bump mint hands two logins the same "+
					"epoch, both satisfy `owner_epoch <= k`, and the fence silently degrades to the "+
					"state_version ping-pong it exists to end", ep)
			}
			seen[ep] = true
		}
		for want := uint64(1); want <= n; want++ {
			require.True(t, seen[want], "the %d claims must be exactly the contiguous run 1..%d; %d is missing", n, n, want)
		}
	})

	t.Run("floor only ever RAISES the mint", func(t *testing.T) {
		mem := NewMemStore()
		pid, err := mem.CreateCharacter(ctx, "Floored", "midgaard", "midgaard:room:temple")
		require.NoError(t, err)

		high, err := mem.ClaimCharacter(ctx, pid, 100)
		require.NoError(t, err)
		require.EqualValues(t, 101, high, "a floor above the row must raise the mint to floor+1")

		// The floor's source is the directory — evictable Redis (#340). A stale or missing value must
		// only ever fail to raise the mint, NEVER lower it: an assignment would hand a live character an
		// epoch below the one its last owner held, wedging them into a session whose every save is
		// refused for its whole lifetime.
		low, err := mem.ClaimCharacter(ctx, pid, 5)
		require.NoError(t, err)
		require.EqualValues(t, 102, low,
			"a floor BELOW the stored epoch must not lower the mint — an evicted/stale directory would "+
				"otherwise wedge a legitimate player with an epoch their own row already outranks")
	})

	t.Run("a missing row reports ErrNoCharacterRow", func(t *testing.T) {
		mem := NewMemStore()
		_, err := mem.ClaimCharacter(ctx, PersistID("mem-uuid-nope"), 0)
		require.ErrorIs(t, err, ErrNoCharacterRow,
			"a claim against a soft-deleted/nonexistent row must be distinguishable from an infrastructure "+
				"error: the caller degrades rather than refusing the login")
	})
}

// ---------------------------------------------------------------------------------------------
// 6a. fresherThan — the login read's comparator (the unit half of the checkpoint fence).
// ---------------------------------------------------------------------------------------------

// TestCheckpointCannotRollBackAcrossAnEpoch is the unit half: the login read must order candidates
// by (owner_epoch, state_version) LEXICOGRAPHICALLY.
//
// The epoch axis is load-bearing, not cosmetic. The checkpoint is a single key per character name and
// two live copies both pulse it every ~10s. state_version only advances on a durable Postgres CAS, so
// the stale and live copies sit at the SAME version for the whole window between a login and its
// first flush — and the `>=` tie-break from #322 then handed the next login the stale copy's content.
// The Postgres fence was intact and completely bypassed one tier up.
//
// Within one epoch the `>=` tie-break must SURVIVE unchanged (#322): at equal state_version the
// checkpoint is by construction at least as recent as the row, and that tie is the only state in
// which the checkpoint tier can contribute to crash recovery at all. And two zero-epoch snapshots (a
// legacy row, a pre-#432 checkpoint) must compare exactly as they did pre-#432.
func TestCheckpointCannotRollBackAcrossAnEpoch(t *testing.T) {
	snap := func(epoch, version uint64) CharSnapshot {
		return CharSnapshot{OwnerEpoch: epoch, StateVersion: version}
	}
	cases := []struct {
		name      string
		candidate CharSnapshot
		best      CharSnapshot
		want      bool
		why       string
	}{
		{
			name: "a lower-epoch candidate loses even at a HIGHER state_version",
			// The bypass in its strongest form: a zombie that has written more times than the live owner.
			candidate: snap(1, 99), best: snap(2, 3), want: false,
			why: "state_version must never let a superseded owner's checkpoint displace the live owner's",
		},
		{
			name:      "a lower-epoch candidate loses on a state_version TIE",
			candidate: snap(1, 5), best: snap(2, 5), want: false,
			why: "the tie is the REACHABLE case: two live copies sit at equal version until the first flush",
		},
		{
			name:      "a higher-epoch candidate wins even at a LOWER state_version",
			candidate: snap(3, 1), best: snap(2, 40), want: true,
			why: "the newest claim's state is the truth; its version lineage restarts from whatever it loaded",
		},
		{
			name:      "within one epoch the >= tie-break survives (#322)",
			candidate: snap(4, 7), best: snap(4, 7), want: true,
			why: "at equal version the checkpoint is at least as recent as the row; this is #322 and must not regress",
		},
		{
			name:      "within one epoch a strictly lower version still loses",
			candidate: snap(4, 6), best: snap(4, 7), want: false,
			why: "the epoch axis must not weaken contention ordering inside an epoch",
		},
		{
			name:      "two UNFENCED (zero-epoch) snapshots compare exactly as pre-#432: tie wins",
			candidate: snap(0, 9), best: snap(0, 9), want: true,
			why: "a legacy row + a pre-#432 checkpoint must keep the old behavior, not silently change on upgrade",
		},
		{
			name:      "two UNFENCED snapshots: a lower version still loses",
			candidate: snap(0, 8), best: snap(0, 9), want: false,
		},
		{
			name:      "an UNFENCED candidate never displaces a CLAIMED one",
			candidate: snap(0, 500), best: snap(1, 1), want: false,
			why: "a zero epoch sorts lowest, so a pre-#432 checkpoint can never beat a claimed row however many times it was written",
		},
		{
			name:      "a CLAIMED candidate displaces an UNFENCED one",
			candidate: snap(1, 1), best: snap(0, 500), want: true,
			why: "the fence self-heals: the first claimed write outranks any legacy value",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fresherThan(tc.candidate, tc.best); got != tc.want {
				t.Fatalf("fresherThan(epoch=%d ver=%d, epoch=%d ver=%d) = %v, want %v (#432): %s",
					tc.candidate.OwnerEpoch, tc.candidate.StateVersion,
					tc.best.OwnerEpoch, tc.best.StateVersion, got, tc.want, tc.why)
			}
		})
	}
}

// TestMemStoreCheckpointGuardRefusesAZombiesPulse pins the MemStore mirror of the Redis guard. Every
// world-package test of the fence runs against MemStore, so a MemStore checkpoint tier that accepted
// any writer would make them vacuously green while the real tier was the only thing holding the line.
func TestMemStoreCheckpointGuardRefusesAZombiesPulse(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()

	require.NoError(t, mem.Checkpoint(ctx, CharSnapshot{
		Name: "Slot", ZoneRef: "darkwood", RoomRef: "darkwood:room:grove", StateVersion: 4, OwnerEpoch: 2,
	}))
	// A zombie's ~10s pulse at the OLD epoch must be REFUSED, and the refusal must be OBSERVABLE.
	// Swallowing it (returning nil) throws away the earliest double-own signal the system has: this
	// tier pulses roughly six times per Postgres flush cadence, so a zombie detected here is evicted a
	// cadence sooner than the Postgres fence alone would manage — a cadence in which it would otherwise
	// keep playing unsaveably and externalizing wealth into another character's row.
	err := mem.Checkpoint(ctx, CharSnapshot{
		Name: "Slot", ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 9, OwnerEpoch: 1,
	})
	require.ErrorIs(t, err, ErrCheckpointNotOwner,
		"MemStore must report an ownership refusal with the same sentinel the Redis tier does (#432): the saver "+
			"routes exactly that sentinel to saveNotOwnerMsg, so a MemStore that returned nil would make every "+
			"hermetic test of the checkpoint-eviction route vacuous; got %v", err)

	got, ok, err := mem.LoadCheckpoint(ctx, "Slot")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "darkwood:room:grove", got.RoomRef,
		"a superseded owner's checkpoint pulse overwrote the live owner's slot — the Postgres fence is bypassed one tier up")
	require.EqualValues(t, 2, got.OwnerEpoch, "the slot's epoch must still name the live owner")

	// The equal-epoch case must still apply (that is the ordinary same-owner pulse).
	require.NoError(t, mem.Checkpoint(ctx, CharSnapshot{
		Name: "Slot", ZoneRef: "midgaard", RoomRef: "midgaard:room:market", StateVersion: 5, OwnerEpoch: 2,
	}))
	got, _, _ = mem.LoadCheckpoint(ctx, "Slot")
	require.Equal(t, "midgaard:room:market", got.RoomRef, "the OWNER's own later pulse must apply")
}

// ---------------------------------------------------------------------------------------------
// 8. The login claim: fail closed on a store outage, but never wedge on a directory outage.
// ---------------------------------------------------------------------------------------------

// epochStubLocator is a benign Locator: every coordination call succeeds and nothing is recorded.
// Tests embed it and override the one method they want to fail.
type epochStubLocator struct{}

func (epochStubLocator) ShardForZone(context.Context, string) (string, error)     { return "", nil }
func (epochStubLocator) EndpointForShard(context.Context, string) (string, error) { return "", nil }
func (epochStubLocator) SetPlayerShard(context.Context, string, string, string, uint64) (bool, error) {
	return true, nil
}

func (epochStubLocator) RegisterPlacement(context.Context, string, string, string, uint64, uint64) (bool, error) {
	return true, nil
}

func (epochStubLocator) ClearPlayerShard(context.Context, string, string, string, uint64, uint64) (bool, error) {
	return true, nil
}

func (epochStubLocator) PlayerEpoch(context.Context, string) (uint64, bool, error) {
	return 0, false, nil
}

func (epochStubLocator) PlayerShard(context.Context, string) (string, bool, error) {
	return "", false, nil
}

// epochBrokenDirectory is a directory whose PlayerEpoch read fails — an evicted or unreachable Redis
// (#340). It must never be able to wedge a legitimate login.
type epochBrokenDirectory struct{ epochStubLocator }

func (epochBrokenDirectory) PlayerEpoch(context.Context, string) (uint64, bool, error) {
	return 0, false, errors.New("directory unreachable")
}

// claimFailingStore is a healthy store whose ownership mint fails — the store outage case.
type claimFailingStore struct{ *MemStore }

func (claimFailingStore) ClaimCharacter(context.Context, PersistID, uint64) (uint64, error) {
	return 0, errors.New("postgres unavailable")
}

// ---------------------------------------------------------------------------------------------
// 9. beginHandoff mints from the row; a failed handoff must adopt what it minted.
// ---------------------------------------------------------------------------------------------

// unreachableLocator makes destination RESOLUTION fail — the PRE-mint failure path. Since the review
// moved the mint to sit after ShardForZone/EndpointForShard/peers all succeed, this locator no longer
// reaches the mint at all, which is the property TestPreMintHandoffFailureAdoptsNothing pins.
type unreachableLocator struct{ epochStubLocator }

func (unreachableLocator) ShardForZone(context.Context, string) (string, error) {
	return "", errors.New("destination zone not in directory")
}

// rejectingHandoffClient resolves fine and then REJECTS the Prepare — the cheapest POST-mint failure,
// and the shape a real one takes (a destination that is up enough to dial but restarting/draining).
type rejectingHandoffClient struct{}

func (rejectingHandoffClient) Prepare(context.Context, *handoffv1.PrepareRequest, ...grpc.CallOption) (*handoffv1.PrepareResponse, error) {
	return nil, errors.New("destination refused the prepare")
}

func (rejectingHandoffClient) Commit(context.Context, *handoffv1.CommitRequest, ...grpc.CallOption) (*handoffv1.CommitResponse, error) {
	return nil, errors.New("unreachable")
}

func (rejectingHandoffClient) Abort(context.Context, *handoffv1.AbortRequest, ...grpc.CallOption) (*handoffv1.AbortResponse, error) {
	return nil, errors.New("unreachable")
}

func (rejectingHandoffClient) AdoptZone(context.Context, *handoffv1.AdoptZoneRequest, ...grpc.CallOption) (*handoffv1.AdoptZoneResponse, error) {
	return nil, errors.New("unreachable")
}

// handoffFixture stands up a source shard whose destination resolution SUCCEEDS (epochStubLocator)
// and whose peer dialer is supplied by the caller, a frozen source session mid-move, and a durable row
// already claimed to rowEpoch. Returns the shard, its zone, the frozen session, the row's pid and the
// epoch the row currently holds.
//
// The zone's Run loop is deliberately NOT started: beginHandoff posts its result to the inbox and the
// test drives z.handle itself, so the session's epoch can be read without racing the single writer.
func handoffFixture(t *testing.T, dir Locator, peers HandoffDialer, rowEpoch uint64) (*Shard, *Zone, *session, PersistID, uint64) {
	t.Helper()
	ctx := context.Background()
	mem := NewMemStore()
	sh := NewShard("midgaard", "addr-a", dir, peers).WithPersistence(mem, mem)
	z := sh.Zone()

	pid, err := mem.CreateCharacter(ctx, "Mover", "midgaard", "midgaard:room:temple")
	require.NoError(t, err)
	// Put the ROW well above the session's epoch, so a mint of session+1 (which would land at 2) is
	// distinguishable from a mint off the row.
	got, err := mem.ClaimCharacter(ctx, pid, rowEpoch-1)
	require.NoError(t, err)
	require.Equal(t, rowEpoch, got)

	s := newTestPlayerEntity(z, "Mover")
	s.epoch = 1
	z.join(s, "")
	s.frozen = true
	s.frozenFrom = s.entity.location
	Move(s.entity, nil) // as the cross-shard move does
	return sh, z, s, pid, rowEpoch
}

// awaitHandoffFail drains the one message beginHandoff posts back to the source zone.
func awaitHandoffFail(t *testing.T, z *Zone) handoffFailMsg {
	t.Helper()
	select {
	case m := <-z.inbox:
		fail, ok := m.(handoffFailMsg)
		require.Truef(t, ok, "expected a handoffFailMsg, got %T", m)
		return fail
	case <-time.After(5 * time.Second):
		t.Fatal("beginHandoff never posted a failure back to the source zone")
		return handoffFailMsg{}
	}
}

// TestHandoffFailureAdoptsTheMintedEpoch pins the self-inflicted wedge (#432).
//
// beginHandoff mints the destination's epoch from the durable ROW — it must, because a login mints
// row+1 while a handoff computing session+1 would collide with it whenever the row has not been saved
// since the session's last claim, and two live copies at one epoch defeat the fence. But minting
// RAISES the row before the move has succeeded. If the move then fails and the source thaws back at
// its old epoch, every subsequent save is refused as not-owner by a value the source minted itself:
// a live player, unsaveable and (via ownershipLost) evictable, produced by an ordinary movement
// hiccup. The thaw must therefore ADOPT the mint, which is safe precisely because the mint was
// exclusive — nobody else holds it, and a failed handoff leaves the source the sole owner again.
//
// The failure is injected at PREPARE, because that is where a post-mint failure actually occurs now
// that the mint sits after destination resolution. A test that failed at resolution instead would
// never reach the mint and would assert nothing about adoption (see
// TestPreMintHandoffFailureAdoptsNothing, which pins that other half deliberately).
func TestHandoffFailureAdoptsTheMintedEpoch(t *testing.T) {
	ctx := context.Background()
	peers := func(string) (handoffv1.HandoffClient, error) { return rejectingHandoffClient{}, nil }
	sh, z, s, pid, rowEpoch := handoffFixture(t, epochStubLocator{}, peers, 7)

	sh.beginHandoff(z, &handoffv1.PlayerSnapshot{CharacterId: "Mover", PersistId: string(pid)},
		"darkwood", "", s.epoch)

	fail := awaitHandoffFail(t, z)
	require.EqualValues(t, rowEpoch+1, fail.adoptEpoch,
		"the failed handoff must report the epoch it MINTED FROM THE ROW (%d+1). A mint of session+1 would "+
			"collide with a concurrent login's row+1 and put two live copies at one epoch", rowEpoch)

	z.handle(fail)

	require.False(t, s.frozen, "premise: the failed handoff thaws the source session")
	require.Equal(t, fail.adoptEpoch, s.epoch,
		"the thawed source must ADOPT the epoch its own handoff minted (#432); left at %d it is outranked by "+
			"the row it raised itself, and every save it makes afterwards is refused as not-owner", s.epoch)

	// And prove it concretely: the next save LANDS.
	res, err := sh.saver.store.SaveCharacter(ctx, dumpCharacter(s))
	require.NoError(t, err)
	require.Equal(t, SaveApplied, res.Outcome,
		"the thawed source's next save was refused (%v) — a wedge manufactured by its own rollback", res.Outcome)
}

// TestPreMintHandoffFailureAdoptsNothing pins the OTHER half of the review's mint relocation.
//
// The mint moved to sit after ShardForZone / EndpointForShard / peers all succeed, for two reasons
// worth keeping executable. First, cost: a player can drive a failed move at movement-command rate
// simply by walking at an exit whose destination shard is down, and minting first made every one of
// those a `characters` row UPDATE. Second, and more important, the gap between "the row is raised"
// and "the source has adopted the new epoch" used to span two directory round-trips plus a peer dial
// — and that gap is exactly the window in which a save enqueued before the move comes back not-owner
// against an epoch this shard minted itself.
//
// So a failure BEFORE the mint must raise nothing and adopt nothing: adoptEpoch 0, the row untouched,
// and the thawed session still at the epoch it started with.
func TestPreMintHandoffFailureAdoptsNothing(t *testing.T) {
	ctx := context.Background()
	sh, z, s, pid, rowEpoch := handoffFixture(t, unreachableLocator{}, nil, 7)
	startEpoch := s.epoch

	sh.beginHandoff(z, &handoffv1.PlayerSnapshot{CharacterId: "Mover", PersistId: string(pid)},
		"darkwood", "", s.epoch)

	fail := awaitHandoffFail(t, z)
	require.Zero(t, fail.adoptEpoch,
		"a handoff that failed at destination RESOLUTION must carry adoptEpoch 0 — it never minted, so there is "+
			"nothing to adopt. A non-zero value here means the mint is happening before the destination is known "+
			"to be reachable, which charges a row UPDATE to every failed move and re-opens the raised-but-not-yet-"+
			"adopted window the frozen-session guard exists to cover")

	z.handle(fail)
	require.False(t, s.frozen, "premise: the failed handoff thaws the source session")
	require.Equal(t, startEpoch, s.epoch,
		"a pre-mint failure must leave the session's epoch untouched; adoption is a max() so 0 is a no-op")

	// The ROW must be untouched too — that is the cost half of the argument.
	row, ok, err := sh.saver.store.LoadCharacter(ctx, "Mover")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, rowEpoch, row.OwnerEpoch,
		"a move that failed before reaching the destination wrote to the characters row anyway (epoch %d -> %d); "+
			"that is a player-drivable write amplification on an exit whose destination is down", rowEpoch, row.OwnerEpoch)

	// And the session must still be able to save — no wedge from a raise it never learned about.
	res, err := sh.saver.store.SaveCharacter(ctx, dumpCharacter(s))
	require.NoError(t, err)
	require.Equal(t, SaveNotOwner, res.Outcome,
		"premise check: the session is legitimately BELOW the row here (the row was pre-claimed by the fixture), "+
			"so its save is refused — the point of this test is that the FAILED HANDOFF did not make that worse")
}
