package world

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// createfail_test.go is the regression for the create-window logout-stash leak (FOLLOW-UPS §2,
// hardening the commit-00956c3 durability fix): a brand-new character whose async CreateCharacter
// FAILS PERMANENTLY while the player has ALSO quit inside the create round-trip. leave() parks a
// final logout snapshot in pendingFinalFlush expecting createdMsg to replay it; with the create
// dead, no createdMsg ever comes, so without active eviction that entry would linger for the zone's
// lifetime. The fix (approach (a)): createCharacter's error branch posts createFailedMsg, which the
// zone handles by deleting the orphaned stash. This test drives that exact path deterministically.

// gateFailStore wraps a MemStore and makes the FIRST CreateCharacter BLOCK until released, then
// optionally FAIL — modelling the async create round-trip that has not returned by the time the
// brand-new player has moved + quit, and (when failErr is set) that ultimately fails permanently
// (no createdMsg, only createFailedMsg). All other store ops delegate to the embedded MemStore.
type gateFailStore struct {
	*MemStore
	entered chan struct{} // closed when the first CreateCharacter is entered (the goroutine is live)
	release chan struct{} // CreateCharacter blocks here until the test releases it
	once    sync.Once
	failErr error // when non-nil, the gated CreateCharacter returns this instead of inserting
}

func (s *gateFailStore) CreateCharacter(ctx context.Context, name, zoneRef, roomRef string) (PersistID, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if s.failErr != nil {
		return "", s.failErr
	}
	return s.MemStore.CreateCharacter(ctx, name, zoneRef, roomRef)
}

// TestCreateWindowFailEvictsLogoutStash proves the FOLLOW-UPS §2 fix: when a brand-new character
// quits inside the create window AND CreateCharacter then fails permanently, the deferred logout
// snapshot is actively evicted from pendingFinalFlush (it is unpersistable — the row never existed)
// rather than lingering. We assert the stash is parked while the create is in flight, then EMPTY
// after the failure is processed.
func TestCreateWindowFailEvictsLogoutStash(t *testing.T) {
	store := &gateFailStore{
		MemStore: NewMemStore(),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		failErr:  errors.New("permanent create failure (injected)"),
	}
	shard := NewDemoShard().WithPersistence(store, store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(ctx)
	z := shard.Zone()

	// Fresh login => createCharacter spawns the gated goroutine; it blocks before returning a PID,
	// so s.entity.pid stays nil. The player moves then quits IN this window — leave() parks the
	// final logout snapshot in pendingFinalFlush (the deferred flush awaiting createdMsg).
	out := login(t, shard, z, "Doomed")
	<-store.entered
	drainChan(out)
	sendInput(z, "Doomed", "north") // temple -> market: a real move whose flush would be deferred
	waitFrame(t, out, "Market")
	quit(t, z, "Doomed") // leave() while pid==nil => stash parked

	// The stash is parked (the deferral happened) — observed race-free via the zone presence probe.
	if !zoneProbe(z, "Doomed").stashed {
		t.Fatal("expected a parked create-window logout stash after quit-in-window, found none")
	}

	// Let CreateCharacter return — it FAILS permanently. createCharacter's error branch posts
	// createFailedMsg, the zone evicts the orphaned stash. No createdMsg, so nothing replays.
	close(store.release)

	// The stash must drain to empty (active eviction), and stay empty.
	deadline := time.After(2 * time.Second)
	for zoneProbe(z, "Doomed").stashed {
		select {
		case <-deadline:
			t.Fatal("create-window logout stash was NOT evicted after permanent create failure (leak)")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// And the failed create never wrote a durable row (sanity: nothing was resurrected onto a row).
	if snap, ok, _ := store.LoadCharacter(context.Background(), "Doomed"); ok {
		t.Fatalf("permanent create failure must leave no durable row, found %+v", snap)
	}
}

// TestCreateWindowSlowSuccessStillReplays is the false-eviction guard: a SLOW-but-successful create
// (the gate releases without failErr) must STILL replay the deferred logout flush — the eviction
// path must never drop an entry a legitimate createdMsg will replay. createCharacter posts exactly
// one of createdMsg / createFailedMsg, so a slow success and a permanent failure are mutually
// exclusive; this proves the happy-path replay is untouched by the new eviction.
func TestCreateWindowSlowSuccessStillReplays(t *testing.T) {
	store := &gateFailStore{
		MemStore: NewMemStore(),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		// failErr nil => the gated create SUCCEEDS (just slowly), so createdMsg replays the stash.
	}
	shard := NewDemoShard().WithPersistence(store, store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(ctx)
	z := shard.Zone()

	out := login(t, shard, z, "Survivor")
	<-store.entered
	drainChan(out)
	sendInput(z, "Survivor", "north") // temple -> market
	waitFrame(t, out, "Market")
	quit(t, z, "Survivor") // stash parked while pid==nil

	if !zoneProbe(z, "Survivor").stashed {
		t.Fatal("expected a parked create-window logout stash before the slow create returns")
	}

	// Release: the create SUCCEEDS late; createdMsg replays the deferred logout flush to the MOVED
	// room (market), not the start room — proving the slow-but-successful create is not evicted.
	close(store.release)
	snap := waitRowWhere(t, store.MemStore, "Survivor", func(s CharSnapshot) bool {
		return s.RoomRef == "midgaard:room:market"
	})
	if snap.RoomRef != "midgaard:room:market" {
		t.Fatalf("slow-success replay landed room=%q, want the moved market room", snap.RoomRef)
	}
	// The stash is consumed by the replay, not left behind.
	if zoneProbe(z, "Survivor").stashed {
		t.Fatal("create-window logout stash lingered after a successful replay")
	}
}
