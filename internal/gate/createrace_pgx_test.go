package gate

// createrace_pgx_test.go is the TELOS_TEST_DSN-gated, REAL-Postgres tier of the create-window
// logout-race coverage (FOLLOW-UPS §2). The MemStore regressions (TestShardRestartCreateRaceLosesMove
// here, TestCreateWindow* in internal/world) pin the fix against the in-memory store; real Postgres
// is the higher-risk env — its create round-trip (a network INSERT + UUID mint) is materially WIDER
// than MemStore's map write, so the quit-inside-the-window race is more likely in production. These
// tests prove the same fix (the pendingFinalFlush stash replay on createdMsg + the createFailedMsg
// eviction) holds against the real pgx store, end to end over the real gate->world Play stream.
//
// They drive the FULL exported-API path (the gate harness + a real world.Shard.WithPersistence(pgx))
// rather than poking zone internals, so the pgx store, the async saver, the create goroutine, and the
// logout flush are all the production seams. The window is forced deterministically by a thin store
// DECORATOR that blocks CreateCharacter on a channel — exactly as the MemStore gatedCreateStore does,
// but wrapping a live *store.Pool so the eventual INSERT/CAS hit real Postgres.
//
// Each test t.Skip()s with no TELOS_TEST_DSN (so `go test ./...` with no DB stays green), migrates the
// schema (idempotent), and hard-deletes its own rows on cleanup so re-runs against the same database
// start clean (the CITEXT name is UNIQUE).

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/double-nibble/telosmud/db"
	"github.com/double-nibble/telosmud/internal/store"
	"github.com/double-nibble/telosmud/internal/world"
)

// nameSeq backs uniqueName so concurrently-running tests (and chaos iterations) never collide on the
// CITEXT-UNIQUE character name.
var nameSeq atomic.Uint64

// uniqueName builds a short, gate-VALID character name (<=20 runes, no leading digit, no dot) that is
// unique per call. Format: prefix + a base36 process-time millis + a base36 monotonic counter — letters
// and digits only, comfortably under the cap. The prefix carries a leading letter so the name is valid.
func uniqueName(prefix string) string {
	ms := strconv.FormatInt(time.Now().UnixMilli()%1_000_000_000, 36) // ~6 base36 chars
	n := strconv.FormatUint(nameSeq.Add(1), 36)
	return prefix + ms + n
}

// pgxTestStore opens the gated test database (skipping when TELOS_TEST_DSN is unset), migrates the
// schema, and returns a live *store.Pool that satisfies world.CharacterStore. It mirrors the store
// package's own testPool, duplicated here because that helper is unexported in another package — the
// duplication is a few lines and keeps this gate-side test self-contained.
func pgxTestStore(t *testing.T) *store.Pool {
	t.Helper()
	dsn := os.Getenv("TELOS_TEST_DSN")
	if dsn == "" {
		t.Skip("TELOS_TEST_DSN not set; skipping real-Postgres create-race test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate test db: %v", err)
	}
	p, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// cleanupChar hard-deletes a character row by name so re-runs start clean (the CITEXT name is UNIQUE).
// store.Pool exposes no delete (it is not a runtime op), so this opens a fresh short-lived pgxpool via
// the gated DSN and DELETEs directly — independent of the world pool's lifecycle/cleanup ordering, and
// mirroring how internal/store/store_test.go cleans its own rows (it reaches p.pool, unexported here).
func cleanupChar(t *testing.T, name string) {
	t.Helper()
	dsn := os.Getenv("TELOS_TEST_DSN")
	if dsn == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Logf("cleanup: open: %v", err)
		return
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DELETE FROM characters WHERE name = $1`, name); err != nil {
		t.Logf("cleanup: delete %q: %v", name, err)
	}
}

// gatedPgxStore wraps a real *store.Pool and gates CreateCharacter so the test can force the player to
// move + quit INSIDE the create round-trip (the pid==nil window). It blocks the WHOLE CreateCharacter
// — INSERT included — until released, then optionally fails: this most directly reproduces the original
// bug (the entity never received its PID before logout). On release with failErr==nil the real INSERT
// runs against Postgres and the minted PID is returned, so createdMsg replays the deferred logout
// flush over the REAL CAS path. All other methods delegate to the embedded *store.Pool unchanged.
//
// delay (when >0) is the chaos knob: instead of blocking on the release channel, CreateCharacter waits
// a randomized small duration before proceeding, so the quit can land at a randomized offset relative
// to create completion. release and delay are mutually exclusive per construction.
type gatedPgxStore struct {
	*store.Pool
	once    sync.Once
	entered chan struct{} // closed when the first CreateCharacter is entered
	release chan struct{} // CreateCharacter blocks here until released (nil => use delay instead)
	delay   time.Duration // when release is nil: sleep this long before proceeding (chaos offset)
	failErr error         // when non-nil, the gated CreateCharacter returns this instead of inserting
}

func (s *gatedPgxStore) CreateCharacter(ctx context.Context, name, zoneRef, roomRef string) (world.PersistID, error) {
	s.once.Do(func() {
		if s.entered != nil {
			close(s.entered)
		}
	})
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	} else if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if s.failErr != nil {
		return "", s.failErr
	}
	return s.Pool.CreateCharacter(ctx, name, zoneRef, roomRef)
}

// TestPgxCreateWindowLogoutRacePersistsMove is the real-Postgres equivalent of the MemStore
// TestShardRestartCreateRaceLosesMove: a brand-new character whose CreateCharacter has NOT returned the
// PID by the time the player moves (temple->market) and quits. The deferred logout flush must replay on
// create completion and durably record the MOVED room against REAL Postgres — not silently drop it at
// the pid==nil guard. We block the create, drive move+quit inside the window, release the create
// (the real INSERT lands), then assert the durable row reflects market.
func TestPgxCreateWindowLogoutRacePersistsMove(t *testing.T) {
	pool := pgxTestStore(t)
	const addr1 = "addr-pgx-cr"
	name := uniqueName("Surv")
	t.Cleanup(func() { cleanupChar(t, name) })

	gated := &gatedPgxStore{
		Pool:    pool,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	h := newHarness(t)
	dir := &mutableDir{addr: addr1}
	sh1 := world.NewShard("midgaard", addr1, nil, nil).WithPersistence(gated, nil)
	h.serveShard(addr1, sh1)
	h.serveGate(dir)

	s1 := h.dial(t)
	s1.login(t, name)
	s1.expect(t, "Temple Square")
	// The create goroutine has entered CreateCharacter but is blocked before returning the PID
	// (s.entity.pid==nil). Move + quit in this window — the exact race a real PG round-trip widens.
	<-gated.entered
	s1.send(t, "north")
	s1.expect(t, "Market Square")
	s1.send(t, "quit")
	s1.expect(t, "Farewell.")
	s1.close(t)
	// Let CreateCharacter complete: the real INSERT mints the PID, createdMsg lands on the gone session
	// and replays the deferred logout flush (market) via the real CAS — it must NOT be dropped.
	close(gated.release)

	waitDurableRoom(t, pool, name, "midgaard:room:market")
}

// TestPgxCreateWindowFailLeavesNoRow is the real-Postgres companion to the MemStore eviction guard
// (TestCreateWindowFailEvictsLogoutStash): when the gated create quits inside the window AND then FAILS
// permanently, the deferred logout flush is evicted (createFailedMsg) and — crucially against real PG —
// NO durable row is ever resurrected. The MemStore test asserts the in-memory stash drains; here we
// assert the externally-observable invariant the auditor cares about: the failed create leaves Postgres
// with zero rows for that name (the data was never persistable, and the eviction did not later land it).
func TestPgxCreateWindowFailLeavesNoRow(t *testing.T) {
	pool := pgxTestStore(t)
	const addr1 = "addr-pgx-fail"
	name := uniqueName("Doom")
	t.Cleanup(func() { cleanupChar(t, name) })

	gated := &gatedPgxStore{
		Pool:    pool,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		failErr: errors.New("permanent create failure (injected, pgx-gated)"),
	}

	h := newHarness(t)
	dir := &mutableDir{addr: addr1}
	sh1 := world.NewShard("midgaard", addr1, nil, nil).WithPersistence(gated, nil)
	h.serveShard(addr1, sh1)
	h.serveGate(dir)

	s1 := h.dial(t)
	s1.login(t, name)
	s1.expect(t, "Temple Square")
	<-gated.entered
	s1.send(t, "north")
	s1.expect(t, "Market Square")
	s1.send(t, "quit")
	s1.expect(t, "Farewell.")
	s1.close(t)
	// Release: the create FAILS permanently. createFailedMsg evicts the orphaned stash; no createdMsg,
	// so nothing replays. The durable store must stay empty for this name.
	close(gated.release)

	// Give the failure path time to process (eviction is async on the zone goroutine), then assert the
	// row never appears — poll for a window so a late, erroneous resurrection would still be caught.
	requireNoDurableRow(t, pool, name, 1500*time.Millisecond)
}

// TestPgxCreateWindowChaosRandomOffsets is the chaos variant: it loops N iterations, each forcing the
// move+quit inside a create window whose completion lands at a RANDOMIZED small offset relative to the
// quit (the gated store sleeps a random sub-window before proceeding, instead of blocking on a signal),
// across both SUCCESS and FAILURE outcomes. Regardless of where the quit falls relative to create
// completion, the durable end-state must ALWAYS be correct: a SUCCESS persists exactly the moved room
// (market), a FAILURE leaves no row. No offset may produce a lost move, a stale (temple) row, or a
// duplicated/orphaned row. The assertion is deterministic despite the randomized timing because it
// pins the INVARIANT (final durable state), never the interleaving.
func TestPgxCreateWindowChaosRandomOffsets(t *testing.T) {
	pool := pgxTestStore(t)

	const iterations = 20
	rng := rand.New(rand.NewSource(0xC0FFEE)) // fixed seed: the timing jitter is reproducible.

	for i := 0; i < iterations; i++ {
		i := i
		t.Run("", func(t *testing.T) {
			addr := "addr-pgx-chaos"
			name := uniqueName("Chaos")
			t.Cleanup(func() { cleanupChar(t, name) })

			// Half the iterations exercise the permanent-failure branch (eviction), half the slow-success
			// branch (replay). The create completes after a randomized 0-12ms offset, so the quit lands at
			// an arbitrary point relative to create completion.
			fail := i%2 == 0
			var failErr error
			if fail {
				failErr = errors.New("chaos permanent create failure")
			}
			gated := &gatedPgxStore{
				Pool:    pool,
				entered: make(chan struct{}),
				delay:   time.Duration(rng.Intn(13)) * time.Millisecond,
				failErr: failErr,
			}

			h := newHarness(t)
			dir := &mutableDir{addr: addr}
			sh := world.NewShard("midgaard", addr, nil, nil).WithPersistence(gated, nil)
			h.serveShard(addr, sh)
			h.serveGate(dir)

			s1 := h.dial(t)
			s1.login(t, name)
			s1.expect(t, "Temple Square")
			<-gated.entered // the create goroutine is live; it will complete after the random delay.
			s1.send(t, "north")
			s1.expect(t, "Market Square")
			s1.send(t, "quit")
			s1.expect(t, "Farewell.")
			s1.close(t)

			if fail {
				// FAILURE: no durable row may ever appear (eviction holds at every offset).
				requireNoDurableRow(t, pool, name, 800*time.Millisecond)
				return
			}
			// SUCCESS: the moved room is durably persisted (replay holds at every offset) — never temple.
			snap := waitDurableRoom(t, pool, name, "midgaard:room:market")
			if snap.RoomRef == "midgaard:room:temple" {
				t.Fatalf("chaos iter %d: durable row stranded at the start room (lost move)", i)
			}
		})
	}
}

// TestPgxCrossSessionSameNameEvictionDoesNotClobber is the belt-and-suspenders cross-session test (the
// security-auditor's recommendation, locking the eviction invariant). Two sessions reuse the SAME name:
//
//	A: gated create that will FAIL; logs in, moves, quits INSIDE the window (parks a stash keyed by name).
//	B: after A is gone, logs in reusing the name with a gated create that SUCCEEDS; moves, quits-after-move.
//	Then A's failure is released LAST.
//
// A's createFailedMsg must NOT evict B's legitimate logout flush. This proves the structural-impossibility
// argument the auditor made: the pendingFinalFlush stash is a PER-ZONE map and characterCreateFailed
// deletes only from the zone its own create goroutine posts to. A and B run on DISTINCT shards/zones
// (each with its own stash map), so A's late failure-eviction can only touch A's empty map — it has no
// reference to B's entry. The durable end-state must be B's moved room (market), proving B's flush
// survived A's release-last failure. (The shared Postgres row is the only thing A and B contend on, and
// A's create FAILS so it never writes it — only B's INSERT + replay land.)
func TestPgxCrossSessionSameNameEvictionDoesNotClobber(t *testing.T) {
	pool := pgxTestStore(t)
	const addr1 = "addr-pgx-xsession"
	name := uniqueName("Shar")
	t.Cleanup(func() { cleanupChar(t, name) })

	// Session A: a gated create that FAILS, released LAST. We hold its release until the very end.
	gateA := &gatedPgxStore{
		Pool:    pool,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		failErr: errors.New("session-A permanent create failure (injected)"),
	}

	h := newHarness(t)
	dir := &mutableDir{addr: addr1}
	sh1 := world.NewShard("midgaard", addr1, nil, nil).WithPersistence(gateA, nil)
	h.serveShard(addr1, sh1)
	h.serveGate(dir)

	// A logs in, moves, quits INSIDE its (still-blocked) create window — parking a stash under `name`.
	a := h.dial(t)
	a.login(t, name)
	a.expect(t, "Temple Square")
	<-gateA.entered
	a.send(t, "north")
	a.expect(t, "Market Square")
	a.send(t, "quit")
	a.expect(t, "Farewell.")
	a.close(t)

	// Session B reuses the SAME name, but needs a SUCCEEDING create — and a shard's store is fixed at
	// construction (gateA always fails). So B runs on a SECOND shard/zone backed by a fresh succeeding
	// gate, sharing the same Postgres pool + the same name. This also makes the eviction guarantee
	// STRONGER than a same-zone test: A and B now hold genuinely separate pendingFinalFlush maps, so A's
	// failure-eviction provably cannot reach B's entry — the structural-impossibility claim made concrete.
	const addr2 = "addr-pgx-xsession-b"
	gateB := &gatedPgxStore{
		Pool:    pool,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		// failErr nil => B's create SUCCEEDS (slowly); createdMsg replays B's deferred logout flush.
	}
	sh2 := world.NewShard("midgaard", addr2, nil, nil).WithPersistence(gateB, nil)
	h.serveShard(addr2, sh2)
	dir.set(addr2) // route the next login to B's shard.

	b := h.dial(t)
	b.login(t, name)
	b.expect(t, "Temple Square")
	<-gateB.entered
	b.send(t, "north")
	b.expect(t, "Market Square")
	b.send(t, "quit")
	b.expect(t, "Farewell.")
	b.close(t)

	// Release B's create FIRST so its legitimate flush lands (market), then release A's FAILURE LAST.
	close(gateB.release)
	waitDurableRoom(t, pool, name, "midgaard:room:market")

	// A's permanent failure is released LAST. Its createFailedMsg fires on A's (separate) zone goroutine
	// and can only touch A's own pendingFinalFlush keyed by `name` — which is empty (A quit, never
	// re-attached, and A's zone never held B's stash). It must NOT clobber B's durable market row.
	close(gateA.release)

	// Assert B's row SURVIVES with B's moved room: poll for a window so a late, erroneous eviction-driven
	// clobber (a delete or a temple overwrite) would be caught.
	requireDurableRoomStable(t, pool, name, "midgaard:room:market", 1500*time.Millisecond)
}

// --- durable-state polling helpers (real Postgres) ----------------------------------------------

// waitDurableRoom polls the durable row for name until it exists with the wanted room, then returns it.
func waitDurableRoom(t *testing.T, p *store.Pool, name, wantRoom string) world.CharSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, ok, err := p.LoadCharacter(context.Background(), name)
		if err != nil {
			t.Fatalf("load %q: %v", name, err)
		}
		if ok && snap.RoomRef == wantRoom {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable row for %q never reached room=%q (found=%v room=%q)", name, wantRoom, ok, snap.RoomRef)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireNoDurableRow asserts no durable row appears for name within the window — the failed-create
// invariant (the row was never persistable and the eviction never resurrected it).
func requireNoDurableRow(t *testing.T, p *store.Pool, name string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		snap, ok, err := p.LoadCharacter(context.Background(), name)
		if err != nil {
			t.Fatalf("load %q: %v", name, err)
		}
		if ok {
			t.Fatalf("permanent create failure left a durable row for %q: %+v", name, snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireDurableRoomStable asserts the durable row for name STAYS at wantRoom for the whole window — no
// late eviction-driven clobber (delete or overwrite) reverts it.
func requireDurableRoomStable(t *testing.T, p *store.Pool, name, wantRoom string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		snap, ok, err := p.LoadCharacter(context.Background(), name)
		if err != nil {
			t.Fatalf("load %q: %v", name, err)
		}
		if !ok {
			t.Fatalf("durable row for %q vanished (an eviction clobbered a legitimate flush)", name)
		}
		if snap.RoomRef != wantRoom {
			t.Fatalf("durable row for %q reverted to room=%q, want %q (eviction clobbered the flush)", name, snap.RoomRef, wantRoom)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
