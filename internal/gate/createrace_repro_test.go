package gate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/world"
)

// gatedCreateStore wraps a MemStore and BLOCKS CreateCharacter until released, modelling the real-
// world race where the async CreateCharacter round-trip (a goroutine spawned at fresh login) has not
// returned the minted PersistID by the time the player has already moved and quit. Under -race + CPU
// contention the live CreateCharacter goroutine can be descheduled long enough to lose this race for
// real (that was the source of the TestShardRestartPreservesPersistedState flake); blocking on a
// channel makes it deterministic so this regression always exercises the create-window logout path.
type gatedCreateStore struct {
	*world.MemStore
	once    sync.Once
	created chan struct{} // closed when CreateCharacter is entered (the create goroutine is running)
	release chan struct{} // CreateCharacter blocks here until the test releases it
}

func (s *gatedCreateStore) CreateCharacter(ctx context.Context, name, zoneRef, roomRef string) (world.PersistID, error) {
	s.once.Do(func() { close(s.created) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return s.MemStore.CreateCharacter(ctx, name, zoneRef, roomRef)
}

// TestShardRestartCreateRaceLosesMove is the deterministic regression for the create-window logout
// loss that flaked TestShardRestartPreservesPersistedState (REAL durability bug): a brand-new
// character whose CreateCharacter has NOT returned the PersistID by the time the player moves
// (temple->market) and quits. The logout flush must NOT be silently skipped — the durable record
// must record the MOVED room, not the create's start room. Before the fix this stranded temple at
// state_version 0 (only the CreateCharacter INSERT ever landed; the saveFinal was dropped at the
// pid==nil guard and then again when createdMsg reached a gone session).
func TestShardRestartCreateRaceLosesMove(t *testing.T) {
	const addr1 = "addr-cr"
	store := &gatedCreateStore{
		MemStore: world.NewMemStore(),
		created:  make(chan struct{}),
		release:  make(chan struct{}),
	}

	h := newHarness(t)
	dir := &mutableDir{addr: addr1}
	sh1 := world.NewShard("midgaard", addr1, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr1, sh1)
	h.serveGate(dir)

	s1 := h.dial(t)
	s1.login(t, "Survivor")
	s1.expect(t, "Temple Square")
	// The create goroutine has entered CreateCharacter but is BLOCKED before returning the PID, so
	// s.entity.pid is still nil. Move + quit in this window — the exact race the flake hits under load.
	<-store.created
	s1.send(t, "north")
	s1.expect(t, "Market Square")
	s1.send(t, "quit")
	s1.expect(t, "Farewell.")
	s1.close(t)
	// Now let CreateCharacter complete: createdMsg lands on the gone session and must replay the
	// deferred logout flush (the moved room), not drop it.
	close(store.release)

	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, ok, _ := store.LoadCharacter(t.Context(), "Survivor")
		if ok && snap.RoomRef == "midgaard:room:market" {
			return
		}
		if time.Now().After(deadline) {
			snap, _, _ := store.LoadCharacter(t.Context(), "Survivor")
			t.Fatalf("create-race logout flush lost the move: durable room=%q version=%d, want midgaard:room:market",
				snap.RoomRef, snap.StateVersion)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
