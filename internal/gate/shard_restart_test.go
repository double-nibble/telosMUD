package gate

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/world"
)

// shard_restart_test.go is the Wave-2 BLACK-BOX regression for TRUE SHARD-RESTART persistence
// (the GAP flagged in docs/TEST-COVERAGE.md): distinct from a reconnect (same process, link-death
// resume), this brings a world shard DOWN and back UP as a fresh process — a NEW *world.Shard
// instance — and confirms a character's durable state survives the PROCESS boundary and routes
// correctly on reconnect. It is the cross-process leg of the memory->Redis->Postgres durability
// ladder: the in-memory session on the old shard is GONE, so the only way the player lands back in
// their saved room is the durable store.
//
// The restart is faithful: the old shard's gRPC server is stopped and its zone goroutine cancelled
// (dropShard), then a BRAND-NEW shard is constructed from the SAME store and served at a NEW
// endpoint (a restarted container commonly re-registers at a fresh address; the directory re-
// resolves). The gate's directory seam is repointed at the new endpoint, exactly as a real
// directory would after the restarted shard re-registers its zone. The reconnecting player must
// rehydrate from the store into their saved room — proving nothing but the durable record carried
// the state across the process death.
func TestShardRestartPreservesPersistedState(t *testing.T) {
	const addr1, addr2 = "addr-a", "addr-a2"
	// One store outlives BOTH shard instances — the live stack's single Postgres. The first shard
	// flushes the player's moved room into it; the second (restarted) shard loads from it.
	store := world.NewMemStore()

	h := newHarness(t)
	// A directory seam the test can REPOINT: it starts routing every login to addr1, and after the
	// restart we swap it to addr2 (the restarted shard's new endpoint).
	dir := &mutableDir{addr: addr1}

	// --- Boot the original shard, walk the player to a new room, quit (durable flush). ---
	sh1 := world.NewShard("midgaard", addr1, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr1, sh1)
	h.serveGate(dir)

	s1 := h.dial(t)
	s1.login(t, "Survivor")
	s1.expect(t, "Temple Square")
	s1.send(t, "north") // temple -> market: a durable state change
	s1.expect(t, "Market Square")
	s1.send(t, "quit")
	s1.expect(t, "Farewell.")
	s1.close(t)

	// The logout flush must land the moved room in the store before we kill the shard — otherwise
	// the "restart" would just be testing the seed room. Poll the store (async saver writes in ms).
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, ok, _ := store.LoadCharacter(t.Context(), "Survivor")
		if ok && snap.RoomRef == "midgaard:room:market" {
			break
		}
		if time.Now().After(deadline) {
			snap, _, _ := store.LoadCharacter(t.Context(), "Survivor")
			t.Fatalf("pre-restart: logout flush did not record the moved room: durable room=%q, want midgaard:room:market", snap.RoomRef)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// --- THE RESTART: kill the original shard (process death), boot a FRESH shard from the SAME
	// store at a NEW endpoint, and repoint the directory at it. ---
	h.dropShard(addr1)
	sh2 := world.NewShard("midgaard", addr2, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr2, sh2)
	dir.set(addr2) // the restarted shard re-registered; the directory now resolves the zone here.

	// --- Reconnect to the restarted shard. The player must rehydrate from the DURABLE store into
	// their saved (moved) room — the old in-memory session is gone, so only the store can carry it. ---
	s2 := h.dial(t)
	s2.login(t, "Survivor")
	s2.expect(t, "Market Square")
	if got := s2.acc.String(); strings.Contains(got, "Temple Square") {
		t.Fatalf("after shard restart the player was dumped at the start room, not the saved room:\n%s", got)
	}
	// And they are LIVE on the restarted shard: a command round-trips (the load path left the session
	// fully attached, with the input-seq fence reset so the first input is not muted).
	s2.send(t, "say survived the restart")
	s2.expect(t, "You say, 'survived the restart'")
	s2.close(t)
}

// mutableDir is a gate directory seam whose target endpoint can be SWAPPED at runtime, so a test can
// model a shard restarting at a new endpoint (the directory re-resolving the zone to the new
// address). It routes every character to the current addr. Concurrency-safe: the gate reads it from
// the handle goroutine while the test writes it.
type mutableDir struct {
	mu   sync.Mutex
	addr string
}

func (d *mutableDir) set(addr string) {
	d.mu.Lock()
	d.addr = addr
	d.mu.Unlock()
}

func (d *mutableDir) ShardForCharacter(string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.addr == "" {
		return "", false
	}
	return d.addr, true
}
